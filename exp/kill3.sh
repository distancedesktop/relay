#!/usr/bin/env bash
# Is the 30s post-close read stall a SPLICE cost or a LIBRARY defect?
#
# K1 (relayed, agent SIGTERM -> its own server.Close()): the relay observed the
# upstream end in 5.995s and closed the client session immediately -- the client even
# printed the relay's close message -- yet the client's video str.Read sat until its
# own 30s read deadline. That is the shape of ReceiveStream.handleSessionGoneError
# (stream.go:278-311) waiting on s.closed after the stream was already removed from
# the map, i.e. the read-side twin of the known SendStream bug.
#
# If a DIRECT client stalls the same 30s on the same agent shutdown, the splice is
# not the cause and the relay is exonerated. If direct is prompt, it is a splice cost
# and belongs in the verdict as one.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
APORT=53086

run_case() {
  local name="$1" sig="$2"
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  sleep 0.7
  ./bin/fakeagent -listen "127.0.0.1:$APORT" -stats 0 >"$OUT/d-agent-$name.log" 2>&1 &
  local apid=$!
  sleep 1.3
  local afp
  afp=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/d-agent-$name.log" | head -1)
  [ -z "$afp" ] && { echo "$name: agent failed to start"; return 1; }

  echo "=== $name: DIRECT to agent, agent gets SIG$sig 6s into a 60s session"
  ./bin/loadclient -label "$name" -url "https://127.0.0.1:$APORT/wt" -pin "$afp" \
    -duration 60s >"$OUT/$name.txt" 2>&1 &
  local lc=$!
  sleep 6
  local t0=$SECONDS
  kill "-$sig" "$apid" 2>/dev/null
  wait "$lc" 2>/dev/null
  echo "client returned $((SECONDS - t0))s after the agent died (54s of video left to read)"
  grep -E '^(video|frames|teardown)' "$OUT/$name.txt"
  grep -E '^  !' "$OUT/$name.txt"
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  echo
}

run_case D1-direct-sigterm TERM
run_case D2-direct-sigkill KILL
