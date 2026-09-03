#!/usr/bin/env bash
# E6c/6d: upstream vanishing. Kill the AGENT mid-session, then (separately) kill
# the RELAY mid-session, and measure how long the client takes to notice and what
# it reports. A relay that leaves the client hanging until an idle timeout is a
# worse failure than a clean error.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh

echo "############ 6c: kill the AGENT 8s into a 60s relayed session ############"
for i in 1 2; do
  start_agent "e6c-$i" -stats 30s
  start_relay "e6c-$i" -stats 30s
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s \
    -label "killagent-relayed-$i" -json > "$OUT/e6c-$i.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(date +%s.%N)
  kill -9 $AGENT_PID; echo "  killed agent pid=$AGENT_PID at t=8s (SIGKILL, no CONNECTION_CLOSE on the wire)"
  wait $CLPID
  T1=$(date +%s.%N)
  echo "  client returned $(python3 -c "print(round($T1-$T0,2))")s after the kill"
  sumline "$OUT/e6c-$i.json"
  echo "  --- relay log tail ---"
  tail -6 "$OUT/relay-e6c-$i.log"
  stop_all
done

echo
echo "############ 6c-direct control: kill the AGENT 8s into a 60s DIRECT session ############"
start_agent e6cdir -stats 30s
sleep 0.3
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 60s \
  -label "killagent-direct" -json > "$OUT/e6c-direct.json" 2>&1 &
CLPID=$!
sleep 8
T0=$(date +%s.%N)
kill -9 $AGENT_PID; echo "  killed agent at t=8s"
wait $CLPID
T1=$(date +%s.%N)
echo "  client returned $(python3 -c "print(round($T1-$T0,2))")s after the kill"
sumline "$OUT/e6c-direct.json"
stop_all

echo
echo "############ 6d: kill the RELAY 8s into a 60s relayed session ############"
for i in 1 2; do
  start_agent "e6d-$i" -stats 30s
  start_relay "e6d-$i" -stats 30s
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s \
    -label "killrelay-$i" -json > "$OUT/e6d-$i.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(date +%s.%N)
  kill -9 $RELAY_PID; echo "  killed relay pid=$RELAY_PID at t=8s"
  wait $CLPID
  T1=$(date +%s.%N)
  echo "  client returned $(python3 -c "print(round($T1-$T0,2))")s after the kill"
  sumline "$OUT/e6d-$i.json"
  echo "  --- did the agent see its upstream leg die? ---"
  tail -4 "$OUT/agent-e6d-$i.log"
  stop_all
done
