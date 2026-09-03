#!/usr/bin/env bash
# E1-probe: one 30s slow-reader run direct, one relayed, 1s stats on both sides.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
RATE=50000
start_agent e1probe -stats 1s -stall-log 1ms
start_relay e1probe -stats 1s
sleep 0.4
echo "### DIRECT 30s @ ${RATE} B/s"
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 30s -read-rate $RATE \
  -label "direct-slow-30s" -json > "$OUT/e1p-direct.json" 2>"$OUT/e1p-direct.err"
sumline "$OUT/e1p-direct.json"
echo "### RELAYED 30s @ ${RATE} B/s"
"$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 30s -read-rate $RATE \
  -label "relayed-slow-30s" -json > "$OUT/e1p-relay.json" 2>"$OUT/e1p-relay.err"
sumline "$OUT/e1p-relay.json"
stop_all
echo "=========== AGENT LOG ==========="
cat "$AGENT_LOG"
echo "=========== RELAY LOG ==========="
cat "$RELAY_LOG"
