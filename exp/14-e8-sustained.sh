#!/usr/bin/env bash
# E8: sustained run over minutes, watching for drift and leaks rather than
# throughput. 5 minutes fast reader, relayed, sampling relay RSS/threads/CPU and
# open FDs every 15s. A relay that is fine for 20s but leaks a goroutine or a
# buffer per chunk would show here.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
DUR=${1:-300s}
start_agent e8 -stats 60s
start_relay e8 -stats 60s
sleep 0.4
echo "sampling relay pid=$RELAY_PID agent pid=$AGENT_PID"

"$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$DUR" \
  -label "sustained-relayed" -json > "$OUT/e8-relay.json" 2>&1 &
CLPID=$!
printf "%6s %10s %8s %8s %8s %10s %8s %8s\n" t relayRSS rThr rFDs rCPU agentRSS aThr aCPU
t=0
while kill -0 $CLPID 2>/dev/null; do
  rrss=$(awk '/VmRSS/{print $2}' /proc/$RELAY_PID/status 2>/dev/null)
  rthr=$(awk '/Threads/{print $2}' /proc/$RELAY_PID/status 2>/dev/null)
  rfd=$(ls /proc/$RELAY_PID/fd 2>/dev/null | wc -l)
  rcpu=$(awk '{print ($14+$15)/100}' /proc/$RELAY_PID/stat 2>/dev/null)
  arss=$(awk '/VmRSS/{print $2}' /proc/$AGENT_PID/status 2>/dev/null)
  athr=$(awk '/Threads/{print $2}' /proc/$AGENT_PID/status 2>/dev/null)
  acpu=$(awk '{print ($14+$15)/100}' /proc/$AGENT_PID/stat 2>/dev/null)
  printf "%6s %10s %8s %8s %8s %10s %8s %8s\n" "${t}s" "${rrss}kB" "$rthr" "$rfd" "${rcpu}s" "${arss}kB" "$athr" "${acpu}s"
  t=$((t+15)); sleep 15
done
wait $CLPID
sumline "$OUT/e8-relay.json"
sleep 1.5
grep -E "session down after|u->c bytes=|c->u bytes=|WARNING" "$RELAY_LOG"
grep "session down" "$AGENT_LOG"
echo "--- goroutine WARNING count: $(grep -c WARNING "$RELAY_LOG") ---"
stop_all
