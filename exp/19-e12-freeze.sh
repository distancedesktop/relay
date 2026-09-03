#!/usr/bin/env bash
# E12: measure the added buffer WITHOUT the -read-rate throttle, by freezing the
# client process with SIGSTOP.
#
# Why this is a better measurement than E1/E3:
#   * -read-rate throttles reads from the very first byte, so quic-go's receive
#     window auto-tuning never fires and every window in the chain stays at its
#     initial size. That measures the initial-window case only.
#   * Here the client reads AS FAST AS IT CAN for warm seconds (auto-tuning is free
#     to grow every window on both legs to whatever the path warrants), and is then
#     SIGSTOPped. From that instant nothing downstream consumes anything, so the
#     agent keeps writing until every buffer in the chain is full and its
#     SendStream.Write blocks for good.
#   * The bytes the agent pushes between the freeze and the permanent block ARE the
#     total buffer in the chain, read off the agent's own counter. No latency
#     filter, no throttle, no client-side arithmetic.
#
# direct   chain: agent-send-buf | client-recv-window | client socket rmem
# relayed  chain: agent-send-buf | RELAY-recv-window | relay 32KB chunk |
#                 client-recv-window | client socket rmem
# The delta (relayed - direct) is what terminating adds.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
PROXY="$(dirname "${BASH_SOURCE[0]}")"/delayproxy.py
P1=${P1:-127.0.0.1:53880}
P2=${P2:-127.0.0.1:53881}
RTT=${1:-0}          # e2e ms; 0 = no proxies at all (pure loopback)
WARM=${2:-20}        # seconds of fast reading before the freeze
BR=${3:-2000000}
PFX=${PFX:-r0}       # filename prefix so parallel RTT settings do not collide
PPIDS=""
kill_proxies() { for p in $PPIDS; do kill "$p" 2>/dev/null; done; PPIDS=""; sleep 0.3; }
start_proxy() { python3 "$PROXY" "$1" "$2" "$3" > "$OUT/proxy-$PFX-$4.log" 2>&1 & PPIDS="$PPIDS $!"; sleep 0.5; }

# agent bytes right now, from its 1s stats line
abytes() { grep -o 'stats frames=[0-9]* bytes=[0-9]*' "$AGENT_LOG" | tail -1 | grep -o 'bytes=[0-9]*' | cut -d= -f2; }

freeze_probe() { # $1=url $2=pin $3=tag
  local url="$1" pin="$2" tag="$3"
  "$BIN/loadclient" -url "$url" -pin "$pin" -duration 200s -label "freeze-$tag" -json > "$OUT/e12-$PFX-$tag.json" 2>&1 &
  local cl=$!
  sleep "$WARM"
  local b0; b0=$(abytes)
  kill -STOP $cl
  echo "    frozen at agent bytes=$b0 (client pid $cl SIGSTOPped)"
  local prev=-1 same=0 b=0
  for s in $(seq 1 40); do
    sleep 1
    b=$(abytes)
    if [ "$b" = "$prev" ]; then same=$((same+1)); else same=0; fi
    prev=$b
    [ $((s % 5)) -eq 0 ] && echo "      t=+${s}s agent bytes=$b (delta $((b-b0)))"
    [ $same -ge 4 ] && { echo "      plateau after ${s}s"; break; }
  done
  echo "    RESULT $tag: buffered_after_freeze=$((b-b0)) bytes = $(python3 -c "print(round(($b-$b0)/($BR/8),2))") s of video at $BR bit/s"
  grep -E "video: write blocked" "$AGENT_LOG" | tail -1
  kill -CONT $cl 2>/dev/null; kill -9 $cl 2>/dev/null; wait $cl 2>/dev/null
}

echo "########## E12: SIGSTOP-the-client buffer probe. e2e RTT=${RTT}ms warm=${WARM}s bitrate=$BR ##########"
start_agent "e12-$PFX" -stats 1s -stall-log 500ms -bitrate "$BR" -fps 60
if [ "$RTT" != "0" ]; then
  HALF=$(python3 -c "print($RTT/2)"); QTR=$(python3 -c "print($RTT/4)")
else
  HALF=0; QTR=0
fi

for rep in 1 2; do
  echo "--- DIRECT rep$rep ---"
  if [ "$RTT" != "0" ]; then
    start_proxy "$P1" "$AGENT_ADDR" "$HALF" "e12-d-$rep"
    freeze_probe "https://$P1/wt" "$AGENT_FP" "direct-$rep"
    kill_proxies
  else
    freeze_probe "https://$AGENT_ADDR/wt" "$AGENT_FP" "direct-$rep"
  fi
done

for rep in 1 2; do
  echo "--- RELAYED rep$rep (relay windows: quic-go defaults) ---"
  if [ "$RTT" != "0" ]; then
    start_proxy "$P2" "$AGENT_ADDR" "$QTR" "e12-u-$rep"
    UPTARGET="$P2"
  else
    UPTARGET="$AGENT_ADDR"
  fi
  RELAY_LOG="$OUT/relay-e12-$PFX-$rep.log"; rm -f "$RELAY_LOG"
  "$BIN/relay" -listen "$RELAY_ADDR" -route "/wt=https://$UPTARGET/wt,fingerprint=$AGENT_FP" -stats 5s \
    > "$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  for w in $(seq 1 100); do
    RELAY_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY_LOG" 2>/dev/null); [ -n "$RELAY_FP" ] && break; sleep 0.05
  done
  if [ "$RTT" != "0" ]; then start_proxy "$P1" "$RELAY_ADDR" "$QTR" "e12-dr-$rep"; CLURL="https://$P1/wt"; else CLURL="https://$RELAY_ADDR/wt"; fi
  freeze_probe "$CLURL" "$RELAY_FP" "relayed-$rep"
  echo "    relay rss=$(awk '/VmRSS/{print $2}' /proc/$RELAY_PID/status)kB while both legs are stuck"
  grep -E "u->c: write blocked" "$RELAY_LOG" | tail -2
  kill $RELAY_PID 2>/dev/null
  kill_proxies
done
stop_all
