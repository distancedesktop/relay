#!/usr/bin/env bash
# E4: dropping encoder. -drop-on-block 50ms means the agent drops a frame rather
# than block. The question: does the relay's extra buffer suppress drops while the
# client falls further behind? Compare drops AND client latency, direct vs relayed.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
RATE=50000
start_agent e4 -stats 10s -stall-log 1ms -drop-on-block 50ms
start_relay e4 -stats 10s
sleep 0.4
for i in 1 2; do
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 60s -read-rate $RATE \
    -label "drop-direct-$i" -json > "$OUT/e4-direct-$i.json" 2>&1
  sleep 0.8
  python3 "$OUT/analyze.py" "$OUT/e4-direct-$i.json" "$AGENT_LOG" - -1 -1 $RATE
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s -read-rate $RATE \
    -label "drop-relayed-$i" -json > "$OUT/e4-relay-$i.json" 2>&1
  sleep 1.5
  python3 "$OUT/analyze.py" "$OUT/e4-relay-$i.json" "$AGENT_LOG" "$RELAY_LOG" -1 -1 $RATE
done
echo "=== agent sessions (drop counts) ==="
grep "session down" "$AGENT_LOG"
stop_all
