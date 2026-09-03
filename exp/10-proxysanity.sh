#!/usr/bin/env bash
# Sanity check for delayproxy.py: does inserting it on the client leg actually
# show up as control RTT, and does the session still work at all?
# Ports: proxies on 53280 (client->relay) and 53281 (relay->agent).
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
PROXY="$(dirname "${BASH_SOURCE[0]}")"/delayproxy.py
CLIENT_PROXY=127.0.0.1:53280
UP_PROXY=127.0.0.1:53281
DELAY=${1:-25}   # one-way ms

start_agent psan -stats 10s
# upstream delay proxy: listens on UP_PROXY, forwards to the agent
python3 "$PROXY" "$UP_PROXY" "$AGENT_ADDR" "$DELAY" > "$OUT/proxy-up.log" 2>&1 &
PUP=$!
# relay dials the agent THROUGH the upstream proxy, pinning the agent's real cert
RELAY_LOG="$OUT/relay-psan.log"; rm -f "$RELAY_LOG"
"$BIN/relay" -listen "$RELAY_ADDR" -route "/wt=https://$UP_PROXY/wt,fingerprint=$AGENT_FP" -stats 10s \
  > "$RELAY_LOG" 2>&1 &
RELAY_PID=$!
for i in $(seq 1 100); do
  RELAY_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY_LOG" 2>/dev/null)
  [ -n "$RELAY_FP" ] && break
  sleep 0.05
done
# client delay proxy: listens on CLIENT_PROXY, forwards to the relay
python3 "$PROXY" "$CLIENT_PROXY" "$RELAY_ADDR" "$DELAY" > "$OUT/proxy-cl.log" 2>&1 &
PCL=$!
sleep 0.6
echo "delay=${DELAY}ms one way on EACH leg (so ~$((DELAY*2))ms RTT per leg)"

echo "--- A: direct to agent, no proxy (RTT ~0) ---"
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 5s -label san-direct -json > "$OUT/psan-a.json" 2>&1
sumline "$OUT/psan-a.json"

echo "--- B: through the UPSTREAM proxy straight to agent (one delayed leg) ---"
"$BIN/loadclient" -url "https://$UP_PROXY/wt" -pin "$AGENT_FP" -duration 5s -label san-updelay -json > "$OUT/psan-b.json" 2>&1
sumline "$OUT/psan-b.json"

echo "--- C: relayed, delay on BOTH legs ---"
"$BIN/loadclient" -url "https://$CLIENT_PROXY/wt" -pin "$RELAY_FP" -duration 5s -label san-relayed-both -json > "$OUT/psan-c.json" 2>&1
sumline "$OUT/psan-c.json"

echo "--- proxy counters ---"
tail -2 "$OUT/proxy-up.log"; tail -2 "$OUT/proxy-cl.log"
kill $PUP $PCL 2>/dev/null
stop_all
