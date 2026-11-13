#!/usr/bin/env bash
# E3b: the tradeoff. Do the small relay receive windows that fix the slow-reader
# latency cost throughput on a FAST reader? Fast reader = no -read-rate, so the
# client drains as fast as it can; the agent is the only pacer at 2 Mbps, which
# would hide a window-limited ceiling. So also run an UNPACED case: -bitrate very
# high with -fps high so the agent tries to push much faster than 2 Mbps and the
# window becomes the binding constraint.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh

run_case() { # $1=label-tag  rest=relay flags
  local tag="$1"; shift
  start_relay "e3b-$tag" -stats 30s "$@"
  sleep 0.4
  for i in 1 2; do
    "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 20s \
      -label "fast-$tag-$i" -json > "$OUT/e3b-$tag-$i.json" 2>&1
    sumline "$OUT/e3b-$tag-$i.json"
  done
  sleep 1.2
  grep -E "u->c bytes=" "$RELAY_LOG" | tail -2
  kill $RELAY_PID 2>/dev/null; sleep 0.3
}

for BR in 2000000 50000000; do
  echo "################################################################"
  echo "### FAST READER, agent bitrate=$BR fps=60"
  echo "################################################################"
  start_agent "e3b-$BR" -stats 30s -bitrate "$BR" -fps 60
  sleep 0.3
  echo "--- direct (no relay) ---"
  for i in 1 2; do
    "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 20s \
      -label "fast-direct-$i" -json > "$OUT/e3b-direct-$BR-$i.json" 2>&1
    sumline "$OUT/e3b-direct-$BR-$i.json"
  done
  echo "--- relay, quic-go default windows ---"
  # shellcheck disable=SC2086
  run_case "def-$BR" $QUICGO_WINDOWS
  echo "--- relay, 64 KB stream window ---"
  run_case "w64k-$BR" -stream-window 65536 -max-stream-window 65536 -conn-window 131072 -max-conn-window 131072
  echo "--- relay, 16 KB stream window ---"
  run_case "w16k-$BR" -stream-window 16384 -max-stream-window 16384 -conn-window 32768 -max-conn-window 32768
  echo "=== agent sessions for bitrate=$BR ==="
  grep "session down" "$AGENT_LOG"
  stop_all
done
