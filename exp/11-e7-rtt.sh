#!/usr/bin/env bash
# E7: the same experiments over an emulated WAN path. tc/netem is unavailable, so
# delay is added by a userspace UDP proxy (delayproxy.py) inserted on each leg.
#
# Two topologies, arranged so END-TO-END RTT IS EQUAL between them. That isolates
# "the relay terminates and re-buffers" from "the relay adds a network hop":
#
#   direct   : client --(one-way D)--> agent                    e2e RTT = 2D
#   relayed  : client --(D/2)--> relay --(D/2)--> agent         e2e RTT = 2D
#
# Why this matters: on localhost RTT~0 so quic-go's receive-window auto-tuning
# never fires and the relay's window stays at its 512 KB initial value. With real
# RTT the auto-tuner doubles the window toward 6 MB, which should make the
# relayed slow-reader penalty WORSE than anything measured on loopback.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
PROXY="$(dirname "${BASH_SOURCE[0]}")"/delayproxy.py
P1=${P1:-127.0.0.1:53280}     # in front of whatever the client dials
P2=${P2:-127.0.0.1:53281}     # in front of the agent, used only in the relayed case
PPIDS=""

kill_proxies() { for p in $PPIDS; do kill "$p" 2>/dev/null; done; PPIDS=""; sleep 0.3; }

start_proxy() { # $1=listen $2=target $3=one-way ms $4=logtag
  python3 "$PROXY" "$1" "$2" "$3" > "$OUT/proxy-$4.log" 2>&1 &
  PPIDS="$PPIDS $!"
  sleep 0.5
}

RATE=${1:-50000}          # client read rate, bytes/s (0 = fast reader)
DUR=${2:-60s}
TOTAL_RTT=${3:-200}       # ms, end to end, both topologies
TAG=${4:-e7}
HALF=$(python3 -c "print($TOTAL_RTT/2)")      # one-way for direct
QTR=$(python3 -c "print($TOTAL_RTT/4)")       # one-way per leg for relayed
RFLAGS_DESC="$5"
shift 5 2>/dev/null || shift $#
RELAY_EXTRA=("$@")

rate_flag=()
[ "$RATE" != "0" ] && rate_flag=(-read-rate "$RATE")

echo "############### E7 $TAG: e2e RTT=${TOTAL_RTT}ms  read-rate=$RATE  dur=$DUR  relay-windows='${RFLAGS_DESC:-default}' ###############"
start_agent "$TAG" -stats 20s -stall-log 1ms

echo "--- DIRECT over ${TOTAL_RTT}ms RTT (client -> proxy@${HALF}ms -> agent) ---"
start_proxy "$P1" "$AGENT_ADDR" "$HALF" "$TAG-d1"
for i in 1 2; do
  "$BIN/loadclient" -url "https://$P1/wt" -pin "$AGENT_FP" -duration "$DUR" "${rate_flag[@]}" \
    -label "$TAG-direct-rtt$TOTAL_RTT-$i" -json > "$OUT/$TAG-direct-$i.json" 2>&1
  sleep 0.8
  python3 "$OUT/analyze.py" "$OUT/$TAG-direct-$i.json" "$AGENT_LOG" - -1 -1 "${RATE:-50000}"
done
kill_proxies

echo "--- RELAYED over ${TOTAL_RTT}ms RTT (client -> proxy@${QTR}ms -> relay -> proxy@${QTR}ms -> agent) ---"
start_proxy "$P2" "$AGENT_ADDR" "$QTR" "$TAG-u"
RELAY_LOG="$OUT/relay-$TAG.log"; rm -f "$RELAY_LOG"
"$BIN/relay" -listen "$RELAY_ADDR" -route "/wt=https://$P2/wt,fingerprint=$AGENT_FP" -stats 20s \
  "${RELAY_EXTRA[@]}" > "$RELAY_LOG" 2>&1 &
RELAY_PID=$!
for i in $(seq 1 100); do
  RELAY_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY_LOG" 2>/dev/null); [ -n "$RELAY_FP" ] && break; sleep 0.05
done
[ -z "$RELAY_FP" ] && { echo "FATAL: relay did not start"; cat "$RELAY_LOG"; kill_proxies; stop_all; exit 1; }
echo "relay pid=$RELAY_PID flags: ${RELAY_EXTRA[*]}"
start_proxy "$P1" "$RELAY_ADDR" "$QTR" "$TAG-d2"
for i in 1 2; do
  "$BIN/loadclient" -url "https://$P1/wt" -pin "$RELAY_FP" -duration "$DUR" "${rate_flag[@]}" \
    -label "$TAG-relayed-rtt$TOTAL_RTT-$i" -json > "$OUT/$TAG-relay-$i.json" 2>&1
  sleep 1.5
  python3 "$OUT/analyze.py" "$OUT/$TAG-relay-$i.json" "$AGENT_LOG" "$RELAY_LOG" -1 -1 "${RATE:-50000}"
done
echo "--- proxy datagram counters (loss check: up/down should be large and equal-ish) ---"
tail -1 "$OUT/proxy-$TAG-u.log"; tail -1 "$OUT/proxy-$TAG-d2.log"
echo "--- agent sessions ---"
grep "session down" "$AGENT_LOG"
kill_proxies
stop_all
