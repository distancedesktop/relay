#!/usr/bin/env bash
# E1: slow-reader latency plateau, direct vs relayed, repeated.
#   $1 = duration (e.g. 30s), $2 = reps, $3 = read rate bytes/sec, $4 = tag
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
DUR=${1:-30s}
REPS=${2:-3}
RATE=${3:-50000}
TAG=${4:-e1}
start_agent "$TAG" -stats 1s -stall-log 1ms
start_relay "$TAG" -stats 2s
sleep 0.4
ai=0
for i in $(seq 1 "$REPS"); do
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration "$DUR" -read-rate "$RATE" \
    -label "direct-$DUR-$RATE-$i" -json > "$OUT/$TAG-direct-$i.json" 2>"$OUT/$TAG-direct-$i.err"
  python3 "$OUT/analyze.py" "$OUT/$TAG-direct-$i.json" "$AGENT_LOG" - $ai -1 "$RATE"
  ai=$((ai+1))
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$DUR" -read-rate "$RATE" \
    -label "relayed-$DUR-$RATE-$i" -json > "$OUT/$TAG-relay-$i.json" 2>"$OUT/$TAG-relay-$i.err"
  sleep 1.5
  python3 "$OUT/analyze.py" "$OUT/$TAG-relay-$i.json" "$AGENT_LOG" "$RELAY_LOG" $ai -1 "$RATE"
  ai=$((ai+1))
done
echo "=== agent stalls per session ==="
grep "session down" "$AGENT_LOG"
echo "=== relay video-direction stall log lines (u->c) ==="
grep "u->c: write blocked" "$RELAY_LOG" | head -20
echo "count: $(grep -c 'u->c: write blocked' "$RELAY_LOG")"
stop_all
