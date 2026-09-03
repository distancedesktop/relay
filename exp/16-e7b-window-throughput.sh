#!/usr/bin/env bash
# E7b: the tradeoff that localhost hides. Shrinking the relay's receive window is
# what collapsed the slow-reader latency penalty (E3) -- but a receive window is
# also the throughput ceiling on a path with RTT: max rate = window / RTT.
#
# Topology (same equal-RTT arrangement as E7a):
#   direct  : client --(one-way D)--> agent                 e2e RTT = 2D
#   relayed : client --(D/2)--> relay --(D/2)--> agent      e2e RTT = 2D, per-leg RTT = D
#
# FAST reader throughout, so the only thing that can limit throughput is a window.
# Prediction to falsify: with 100ms per-leg RTT, a 16 KB relay stream window caps
# the agent->relay leg at 16384/0.1 = 160 KB/s = 1.3 Mbps, below the 2 Mbps the
# agent is producing, so the client should fall behind and end short.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
PROXY="$(dirname "${BASH_SOURCE[0]}")"/delayproxy.py
P1=${P1:-127.0.0.1:53680}
P2=${P2:-127.0.0.1:53681}
TOTAL_RTT=${1:-200}
DUR=${2:-30s}
BR=${3:-2000000}
PPIDS=""
kill_proxies() { for p in $PPIDS; do kill "$p" 2>/dev/null; done; PPIDS=""; sleep 0.3; }
start_proxy() { python3 "$PROXY" "$1" "$2" "$3" > "$OUT/proxy-$4.log" 2>&1 & PPIDS="$PPIDS $!"; sleep 0.5; }
HALF=$(python3 -c "print($TOTAL_RTT/2)")
QTR=$(python3 -c "print($TOTAL_RTT/4)")

echo "########### E7b: FAST reader, e2e RTT=${TOTAL_RTT}ms, agent bitrate=$BR ###########"
echo "###          per-leg RTT in the relayed topology = ${HALF}ms"
start_agent e7b -stats 30s -bitrate "$BR" -fps 60

echo "--- DIRECT over ${TOTAL_RTT}ms RTT (client window is quic-go default) ---"
start_proxy "$P1" "$AGENT_ADDR" "$HALF" "e7b-d1"
for i in 1 2; do
  "$BIN/loadclient" -url "https://$P1/wt" -pin "$AGENT_FP" -duration "$DUR" \
    -label "e7b-direct-$i" -json > "$OUT/e7b-direct-$i.json" 2>&1
  sumline "$OUT/e7b-direct-$i.json"
done
kill_proxies

for cfg in "default:" \
           "w64k:-stream-window 65536 -max-stream-window 65536 -conn-window 131072 -max-conn-window 131072" \
           "w16k:-stream-window 16384 -max-stream-window 16384 -conn-window 32768 -max-conn-window 32768"; do
  name="${cfg%%:*}"; flags="${cfg#*:}"
  echo "--- RELAYED over ${TOTAL_RTT}ms RTT, relay windows: ${name} (${flags:-quic-go defaults}) ---"
  start_proxy "$P2" "$AGENT_ADDR" "$QTR" "e7b-u-$name"
  RELAY_LOG="$OUT/relay-e7b-$name.log"; rm -f "$RELAY_LOG"
  # shellcheck disable=SC2086
  "$BIN/relay" -listen "$RELAY_ADDR" -route "/wt=https://$P2/wt,fingerprint=$AGENT_FP" -stats 30s $flags \
    > "$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  for i in $(seq 1 100); do
    RELAY_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY_LOG" 2>/dev/null); [ -n "$RELAY_FP" ] && break; sleep 0.05
  done
  start_proxy "$P1" "$RELAY_ADDR" "$QTR" "e7b-d-$name"
  for i in 1 2; do
    "$BIN/loadclient" -url "https://$P1/wt" -pin "$RELAY_FP" -duration "$DUR" \
      -label "e7b-relayed-$name-$i" -json > "$OUT/e7b-$name-$i.json" 2>&1
    sumline "$OUT/e7b-$name-$i.json"
  done
  sleep 1.2
  grep -E "u->c bytes=" "$RELAY_LOG" | tail -2
  kill $RELAY_PID 2>/dev/null
  kill_proxies
done
echo "--- agent sessions (bytes the agent got to send tells us if it was window-starved) ---"
grep "session down" "$AGENT_LOG"
stop_all
