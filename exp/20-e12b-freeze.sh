#!/usr/bin/env bash
# E12b: corrected SIGSTOP buffer probe.
#
# Why 19-e12-freeze.sh was invalid: it started one agent and reused it for all
# four probes. A frozen client does not close its session, so the previous
# session stayed alive with its video pump blocked and its 1s stats ticker still
# running. abytes() does `tail -1` over the whole agent log, so it read whichever
# session logged last -> rep2 baselines came from the previous session's frozen
# counter (direct-2 "3811 bytes", relayed-2 "0 bytes" are garbage).
#
# Fix: one fresh agent (and fresh relay) per probe, so the log can only ever
# contain session s1.
#
# Known confound, stated rather than hidden: a SIGSTOPped client stops sending
# QUIC ACKs, so the sender stalls on congestion control as well as flow control.
# The plateau is therefore an upper bound on (flow-control buffer + cwnd in
# flight), not a pure receive-window measurement. It is still directly comparable
# direct vs relayed because both sides suffer it identically.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
PROXY="$(dirname "${BASH_SOURCE[0]}")"/delayproxy.py
P1=${P1:-127.0.0.1:53880}
P2=${P2:-127.0.0.1:53881}
RTT=${1:-0}
WARM=${2:-20}
BR=${3:-2000000}
PFX=${PFX:-r0}
PPIDS=""
kill_proxies() { for p in $PPIDS; do kill "$p" 2>/dev/null; done; PPIDS=""; sleep 0.3; }
start_proxy() { python3 "$PROXY" "$1" "$2" "$3" > "$OUT/proxy-$PFX-$4.log" 2>&1 & PPIDS="$PPIDS $!"; sleep 0.5; }

# bytes on session s1 of the CURRENT agent only
abytes() { grep -o 'fakeagent\[s1\]: stats frames=[0-9]* bytes=[0-9]*' "$AGENT_LOG" | tail -1 | grep -o 'bytes=[0-9]*' | cut -d= -f2; }

probe() { # $1=url $2=pin $3=tag  (agent already started, log fresh)
  local url="$1" pin="$2" tag="$3"
  "$BIN/loadclient" -url "$url" -pin "$pin" -duration 200s -label "freeze-$tag" -json > "$OUT/e12b-$PFX-$tag.json" 2>&1 &
  local cl=$!
  sleep "$WARM"
  local b0; b0=$(abytes)
  kill -STOP $cl
  echo "    frozen at agent s1 bytes=$b0 (client pid $cl SIGSTOPped)"
  local prev=-1 same=0 b=0 s
  for s in $(seq 1 25); do
    sleep 1
    b=$(abytes)
    if [ "$b" = "$prev" ]; then same=$((same+1)); else same=0; fi
    prev=$b
    [ $((s % 4)) -eq 0 ] && echo "      t=+${s}s agent bytes=$b (delta $((b-b0)))"
    [ $same -ge 5 ] && { echo "      plateau held 5s, stopping at t=+${s}s"; break; }
  done
  echo "    RESULT $tag: absorbed_after_freeze=$((b-b0)) bytes = $(python3 -c "print(round(($b-$b0)/($BR/8.0),2))")s of video at $BR bit/s"
  kill -CONT $cl 2>/dev/null; kill -9 $cl 2>/dev/null; wait $cl 2>/dev/null
}

echo "########## E12b: SIGSTOP buffer probe (fresh procs per probe). e2e RTT=${RTT}ms warm=${WARM}s bitrate=$BR ##########"
if [ "$RTT" != "0" ]; then HALF=$(python3 -c "print($RTT/2)"); QTR=$(python3 -c "print($RTT/4)"); else HALF=0; QTR=0; fi

for rep in 1 2; do
  echo "--- DIRECT rep$rep ---"
  start_agent "e12b-$PFX-d$rep" -stats 1s -stall-log 2s -bitrate "$BR" -fps 60
  if [ "$RTT" != "0" ]; then
    start_proxy "$P1" "$AGENT_ADDR" "$HALF" "d$rep"
    probe "https://$P1/wt" "$AGENT_FP" "direct-$rep"
    kill_proxies
  else
    probe "https://$AGENT_ADDR/wt" "$AGENT_FP" "direct-$rep"
  fi
  grep -E "video: write blocked" "$AGENT_LOG" | tail -1
  stop_all
done

for rep in 1 2; do
  echo "--- RELAYED rep$rep (relay windows: quic-go defaults) ---"
  start_agent "e12b-$PFX-r$rep" -stats 1s -stall-log 2s -bitrate "$BR" -fps 60
  if [ "$RTT" != "0" ]; then start_proxy "$P2" "$AGENT_ADDR" "$QTR" "u$rep"; UPT="$P2"; else UPT="$AGENT_ADDR"; fi
  RELAY_LOG="$OUT/relay-e12b-$PFX-$rep.log"; rm -f "$RELAY_LOG"
  "$BIN/relay" -listen "$RELAY_ADDR" -route "/wt=https://$UPT/wt,fingerprint=$AGENT_FP" -stats 5s -stall-threshold 2s $QUICGO_WINDOWS > "$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  for w in $(seq 1 100); do
    RELAY_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY_LOG" 2>/dev/null); [ -n "$RELAY_FP" ] && break; sleep 0.05
  done
  if [ "$RTT" != "0" ]; then start_proxy "$P1" "$RELAY_ADDR" "$QTR" "dr$rep"; CLURL="https://$P1/wt"; else CLURL="https://$RELAY_ADDR/wt"; fi
  probe "$CLURL" "$RELAY_FP" "relayed-$rep"
  echo "    relay rss=$(awk '/VmRSS/{print $2}' /proc/$RELAY_PID/status 2>/dev/null)kB while both legs are stuck"
  grep -E "video: write blocked" "$AGENT_LOG" | tail -1
  grep -E "u->c: write blocked" "$RELAY_LOG" | tail -1
  kill $RELAY_PID 2>/dev/null
  kill_proxies
  stop_all
done
