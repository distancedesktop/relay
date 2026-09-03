#!/usr/bin/env bash
# E6: failure modes.
#  6a client vanishes mid-stream (-abort-after) -> is the upstream torn down?
#  6b client skips stop then closes
#  6c agent killed mid-session -> does the client learn promptly?
#  6d relay killed mid-session -> does the client learn promptly?
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh

echo "################ 6a: -abort-after 2s, relayed ################"
start_agent e6a -stats 2s
start_relay e6a -stats 2s
sleep 0.4
for i in 1 2; do
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 30s -abort-after 2s \
    -label "abort-relayed-$i" -json > "$OUT/e6a-$i.json" 2>&1
  sumline "$OUT/e6a-$i.json"
done
sleep 3
echo "--- did the agent session end? (expect one 'session down' per client) ---"
grep -E "session up|session down|AcceptStream ended|control: read ended" "$AGENT_LOG"
echo "--- relay view ---"
grep -E "spliced|session down after|leg ended|mirroring|WARNING" "$RELAY_LOG"
echo "--- goroutine-leak warnings: $(grep -c 'WARNING' "$RELAY_LOG") ---"
echo "--- relay threads after both aborts ---"
awk '{print "threads="$20}' /proc/$RELAY_PID/stat
stop_all

echo
echo "################ 6a-direct control: -abort-after 2s straight at the agent ################"
start_agent e6adir -stats 2s
sleep 0.3
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 30s -abort-after 2s \
  -label "abort-direct" -json > "$OUT/e6a-direct.json" 2>&1
sumline "$OUT/e6a-direct.json"
sleep 2
grep -E "session up|session down|AcceptStream ended|control: read ended" "$AGENT_LOG"
stop_all

echo
echo "################ 6b: -skip-stop, direct vs relayed ################"
start_agent e6b -stats 5s
start_relay e6b -stats 5s
sleep 0.4
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 5s -skip-stop \
  -label "skipstop-direct" -json > "$OUT/e6b-direct.json" 2>&1
sumline "$OUT/e6b-direct.json"
"$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 5s -skip-stop \
  -label "skipstop-relayed" -json > "$OUT/e6b-relay.json" 2>&1
sumline "$OUT/e6b-relay.json"
sleep 2
grep -E "session down|control: client closed" "$AGENT_LOG"
grep -E "session down after|leg ended" "$RELAY_LOG"
stop_all
