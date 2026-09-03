#!/usr/bin/env bash
# E5: high bitrate + fast reader. 8 Mbps @ 60 fps, direct vs relayed, and a
# deliberately abusive 50 Mbps case to find the splice ceiling. CPU from
# /proc/<pid>/stat before and after, so the splice cost is attributable.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
BR=${1:-8000000}
FPS=${2:-60}
DUR=${3:-30s}
TAG=${4:-e5}
start_agent "$TAG" -stats 10s -bitrate "$BR" -fps "$FPS"
start_relay "$TAG" -stats 10s
sleep 0.4

cpu_of() { awk '{print ($14+$15)/100}' /proc/$1/stat 2>/dev/null; }

for i in 1 2; do
  a0=$(cpu_of $AGENT_PID); r0=$(cpu_of $RELAY_PID)
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration "$DUR" \
    -label "direct-${BR}bps-$i" -json > "$OUT/$TAG-direct-$i.json" 2>&1
  a1=$(cpu_of $AGENT_PID); r1=$(cpu_of $RELAY_PID)
  sumline "$OUT/$TAG-direct-$i.json"
  echo "    cpu: agent +$(python3 -c "print(round($a1-$a0,2))")s relay +$(python3 -c "print(round($r1-$r0,2))")s"

  a0=$(cpu_of $AGENT_PID); r0=$(cpu_of $RELAY_PID)
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$DUR" \
    -label "relayed-${BR}bps-$i" -json > "$OUT/$TAG-relay-$i.json" 2>&1
  a1=$(cpu_of $AGENT_PID); r1=$(cpu_of $RELAY_PID)
  sumline "$OUT/$TAG-relay-$i.json"
  echo "    cpu: agent +$(python3 -c "print(round($a1-$a0,2))")s relay +$(python3 -c "print(round($r1-$r0,2))")s"
done
echo "=== agent sessions ==="
grep "session down" "$AGENT_LOG"
echo "=== relay session summaries ==="
grep -A2 "session down after" "$RELAY_LOG"
echo "=== relay u->c stalls count: $(grep -c 'u->c: write blocked' "$RELAY_LOG") ==="
stop_all
