#!/usr/bin/env bash
# E5b: find the splice ceiling. The agent paces on a ticker at -fps, so peak
# throughput is fps*frame_size; to saturate we raise BOTH until the ticker can no
# longer keep up and the pump becomes write-bound. Fast reader throughout.
# Reports client Mbps and CPU seconds per process, so cost is attributable.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
cpu_of() { awk '{print ($14+$15)/100}' /proc/$1/stat 2>/dev/null; }
DUR=20s

for spec in "8000000 60" "50000000 60" "200000000 120" "800000000 240"; do
  set -- $spec; BR=$1; FPS=$2
  echo "################ bitrate=$BR fps=$FPS (target $(python3 -c "print(round($BR/1e6,1))") Mbps) ################"
  start_agent "e5b-$BR" -stats 20s -bitrate "$BR" -fps "$FPS"
  start_relay "e5b-$BR" -stats 20s
  sleep 0.4
  for i in 1 2; do
    a0=$(cpu_of $AGENT_PID); r0=$(cpu_of $RELAY_PID)
    "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration $DUR \
      -label "sat-direct-$BR-$i" -json > "$OUT/e5b-direct-$BR-$i.json" 2>&1
    a1=$(cpu_of $AGENT_PID); r1=$(cpu_of $RELAY_PID)
    sumline "$OUT/e5b-direct-$BR-$i.json"
    echo "    cpu: agent +$(python3 -c "print(round($a1-$a0,2))")s relay +$(python3 -c "print(round($r1-$r0,2))")s"

    a0=$(cpu_of $AGENT_PID); r0=$(cpu_of $RELAY_PID)
    "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration $DUR \
      -label "sat-relayed-$BR-$i" -json > "$OUT/e5b-relay-$BR-$i.json" 2>&1
    a1=$(cpu_of $AGENT_PID); r1=$(cpu_of $RELAY_PID)
    sumline "$OUT/e5b-relay-$BR-$i.json"
    echo "    cpu: agent +$(python3 -c "print(round($a1-$a0,2))")s relay +$(python3 -c "print(round($r1-$r0,2))")s"
  done
  echo "--- agent sessions ---"; grep "session down" "$AGENT_LOG"
  echo "--- relay u->c summaries ---"; grep "u->c bytes=" "$RELAY_LOG"
  stop_all
done
