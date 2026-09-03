#!/usr/bin/env bash
# When the AGENT vanishes mid-stream, how does the client find out?
#
# First pass established that SIGTERM and SIGKILL are equivalent here: fakeagent
# installs no signal handler, so Go's default kills the process without running
# deferred code, and no QUIC CONNECTION_CLOSE is emitted either way. That leaves
# the idle timer as the only detector, so the question becomes: whose timer, and
# what does the client get told? Vary -max-idle to prove detection latency is that
# timer and nothing else.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
APORT=53085
RPORT=53097

run_case() {
  local name="$1" idle="$2" sig="$3"
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
  sleep 0.7

  ./bin/fakeagent -listen "127.0.0.1:$APORT" -stats 0 >"$OUT/kill-agent-$name.log" 2>&1 &
  local apid=$!
  sleep 1.3
  local afp
  afp=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/kill-agent-$name.log" | head -1)
  ./bin/relay -listen "127.0.0.1:$RPORT" \
    -route "/wt=https://127.0.0.1:$APORT/wt,fingerprint=$afp" \
    -max-idle "$idle" -stats 0 >"$OUT/kill-relay-$name.log" 2>&1 &
  sleep 1.3
  local rfp
  rfp=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/kill-relay-$name.log" | head -1)
  [ -z "$rfp" ] && { echo "$name: relay failed to start"; return 1; }

  echo "=== $name: agent gets SIG$sig 6s into a 60s session, relay -max-idle $idle"
  ./bin/loadclient -label "$name" -url "https://127.0.0.1:$RPORT/wt" -pin "$rfp" \
    -duration 60s >"$OUT/$name.txt" 2>&1 &
  local lc=$!
  sleep 6
  local t0=$SECONDS
  kill "-$sig" "$apid" 2>/dev/null
  wait "$lc" 2>/dev/null
  echo "client returned $((SECONDS - t0))s after the agent died (it had 54s of video left to read)"
  grep -E '^(video|frames|teardown)' "$OUT/$name.txt"
  grep -E '^  !' "$OUT/$name.txt"
  sleep 3   # let the relay finish teardown + its 5s stuck-goroutine watchdog before pkill
  sleep 4
  echo "--- relay:"
  grep -a -E 'leg ended|mirroring|no session code|session down after|did not exit' "$OUT/kill-relay-$name.log"
  pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  echo
}

run_case K1-sigterm-idle8  8s  TERM
run_case K2-sigkill-idle8  8s  KILL
run_case K3-sigkill-idle25 25s KILL
