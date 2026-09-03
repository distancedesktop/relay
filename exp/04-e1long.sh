#!/usr/bin/env bash
# E1-long: is the plateau actually a plateau? 60s and 120s, direct vs relayed.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
RATE=50000
start_agent e1long -stats 5s -stall-log 1ms
start_relay e1long -stats 5s
sleep 0.4
ai=0
for DUR in 60s 120s; do
  for i in 1 2; do
    "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration "$DUR" -read-rate $RATE \
      -label "direct-$DUR-r$i" -json > "$OUT/e1long-direct-$DUR-$i.json" 2>&1
    sleep 1
    python3 "$OUT/analyze.py" "$OUT/e1long-direct-$DUR-$i.json" "$AGENT_LOG" - -1 -1 $RATE
    "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$DUR" -read-rate $RATE \
      -label "relayed-$DUR-r$i" -json > "$OUT/e1long-relay-$DUR-$i.json" 2>&1
    sleep 1.5
    python3 "$OUT/analyze.py" "$OUT/e1long-relay-$DUR-$i.json" "$AGENT_LOG" "$RELAY_LOG" -1 -1 $RATE
  done
done
echo "=== agent sessions ==="
grep "session down" "$AGENT_LOG"
stop_all
