#!/usr/bin/env bash
# E0: baseline, fast reader, direct vs relayed, 3 runs each, same agent process.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
start_agent e0 -stats 5s
start_relay e0 -stats 5s
sleep 0.4
for i in 1 2 3; do
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 10s \
    -label "direct-fast-$i" -json > "$OUT/e0-direct-$i.json" 2>"$OUT/e0-direct-$i.err"
  sumline "$OUT/e0-direct-$i.json"
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 10s \
    -label "relayed-fast-$i" -json > "$OUT/e0-relay-$i.json" 2>"$OUT/e0-relay-$i.err"
  sumline "$OUT/e0-relay-$i.json"
done
echo "=== agent session-down lines ==="
grep "session down" "$AGENT_LOG"
echo "=== agent stall lines (count) ==="
grep -c "write blocked" "$AGENT_LOG" || true
echo "=== relay session-down lines ==="
grep -A2 "session down" "$RELAY_LOG"
echo "=== relay cpu lines ==="
grep "process cpu" "$RELAY_LOG" | tail -3
stop_all
