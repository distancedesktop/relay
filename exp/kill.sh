#!/usr/bin/env bash
# What happens when the AGENT vanishes mid-stream?
#
# The relay's teardown logic is symmetric on paper, but every measurement so far
# exercised the client-dies direction. This is the other one, and it is the one a
# real deployment hits most (agent process restart, machine sleep).
#
# Two sub-cases, distinguished because they produce different signals:
#   K1: agent closes gracefully (SIGTERM -> its own session close)   -> code should mirror
#   K2: agent is killed with SIGKILL (no CONNECTION_CLOSE at all)    -> only the idle
#       timeout can notice, so the client waits MaxIdleTimeout
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
APORT=53085
RPORT=53097

pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
sleep 0.5

start_stack() {
  ./bin/fakeagent -listen "127.0.0.1:$APORT" -stats 0 >"$OUT/kill-agent.log" 2>&1 &
  AGENT_PID=$!
  sleep 1.3
  AFP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/kill-agent.log" | head -1)
  ./bin/relay -listen "127.0.0.1:$RPORT" \
    -route "/wt=https://127.0.0.1:$APORT/wt,fingerprint=$AFP" \
    -max-idle 8s -stats 0 >"$OUT/kill-relay-$1.log" 2>&1 &
  RELAY_PID=$!
  sleep 1.3
  RFP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/kill-relay-$1.log" | head -1)
}

run_case() {
  local name="$1" sig="$2"
  start_stack "$name"
  echo "=== $name: agent gets $sig 6s into a 30s session (relay -max-idle 8s)"
  ./bin/loadclient -label "$name" -url "https://127.0.0.1:$RPORT/wt" -pin "$RFP" \
    -duration 30s >"$OUT/$name.txt" 2>&1 &
  LC_PID=$!
  sleep 6
  local t0=$SECONDS
  kill "-$sig" "$AGENT_PID" 2>/dev/null
  wait "$LC_PID" 2>/dev/null
  echo "client returned $((SECONDS - t0))s after the agent was killed"
  grep -E '^(video|frames|teardown|session error|  !)' "$OUT/$name.txt"
  echo "--- relay:"
  grep -a -E 'leg ended|mirroring|no session code|session down after|did not exit' "$OUT/kill-relay-$name.log"
  pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  sleep 1
  echo
}

run_case K1-agent-sigterm TERM
run_case K2-agent-sigkill KILL
