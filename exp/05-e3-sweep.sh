#!/usr/bin/env bash
# E3: window sweep. Same agent throughout; relay restarted per window config.
# The key metric is NOT just client latency (the client drops any frame whose
# latency exceeds 60 s from its percentiles) but agent bytes handed to QUIC in a
# fixed wall-clock window: relayed_minus_direct == the buffer the relay inserted.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
RATE=50000
start_agent e3 -stats 10s -stall-log 1ms
sleep 0.3

echo "########## DIRECT BASELINE (no relay in path) ##########"
for i in 1 2; do
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 60s -read-rate $RATE \
    -label "direct-60s-$i" -json > "$OUT/e3-direct-$i.json" 2>&1
  sleep 0.8
  python3 "$OUT/analyze.py" "$OUT/e3-direct-$i.json" "$AGENT_LOG" - -1 -1 $RATE
done

run_cfg() { # $1=name $2=duration ; rest = relay flags
  local name="$1" dur="$2"; shift 2
  start_relay "e3-$name" -stats 10s "$@"
  sleep 0.4
  for i in 1 2; do
    "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$dur" -read-rate $RATE \
      -label "relay-$name-$i" -json > "$OUT/e3-$name-$i.json" 2>&1
    sleep 1.5
    python3 "$OUT/analyze.py" "$OUT/e3-$name-$i.json" "$AGENT_LOG" "$RELAY_LOG" -1 -1 $RATE
  done
  echo "--- relay $name: u->c stall lines (first 4) ---"
  grep "u->c: write blocked" "$RELAY_LOG" | head -4
  echo "--- relay $name: total u->c stall lines: $(grep -c 'u->c: write blocked' "$RELAY_LOG") ---"
  kill $RELAY_PID 2>/dev/null; sleep 0.3; kill -9 $RELAY_PID 2>/dev/null; RELAY_PID=""
}

echo "########## RELAY, quic-go DEFAULT windows (512K stream / 768K conn / 6M max) ##########"
# shellcheck disable=SC2086
run_cfg default 60s $QUICGO_WINDOWS
echo "########## RELAY, 64 KB stream window ##########"
run_cfg w64k 60s -stream-window 65536 -max-stream-window 65536 -conn-window 131072
echo "########## RELAY, 16 KB stream window ##########"
run_cfg w16k 60s -stream-window 16384 -max-stream-window 16384 -conn-window 32768
echo "########## RELAY, 6 MB stream window (emulates a fully auto-tuned window) ##########"
run_cfg w6m 90s -stream-window 6291456 -max-stream-window 6291456 -conn-window 15728640 -max-conn-window 15728640

echo "=========== AGENT SESSIONS (in order) ==========="
grep "session down" "$AGENT_LOG"
stop_all
