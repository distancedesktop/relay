#!/usr/bin/env bash
# How fast does a viewer learn that the agent shut down gracefully?
#
# Two prior single samples disagree: one direct SIGTERM run unblocked the client in
# 0.01s with end=session-closed:0, another took the full 30s read deadline with
# end=read-timeout. Relayed runs have shown both 5.995s-then-30s and 38s. A coin flip
# on both sides is a very different finding from "the splice loses the signal", so
# repeat each 5x and report the distribution of (client return latency, end kind).
#
# The agent installs a SIGTERM handler (cmd/fakeagent/main.go:114-120) that calls
# server.Close() and then lets main return, so the question is whether the graceful
# CONNECTION_CLOSE is flushed before process exit -- the same race as the client-abort
# case, seen from the other end.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
APORT=53087
RPORT=53098
N="${N:-5}"

one() {
  local mode="$1" i="$2"
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
  pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
  sleep 0.6

  ./bin/fakeagent -listen "127.0.0.1:$APORT" -stats 0 >"$OUT/t-agent.log" 2>&1 &
  local apid=$!
  sleep 1.2
  local afp url pin
  afp=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/t-agent.log" | head -1)
  [ -z "$afp" ] && { echo "  agent failed"; return 1; }

  if [ "$mode" = relayed ]; then
    ./bin/relay -listen "127.0.0.1:$RPORT" \
      -route "/wt=https://127.0.0.1:$APORT/wt,fingerprint=$afp" \
      -stats 0 >"$OUT/t-relay-$i.log" 2>&1 &
    sleep 1.2
    pin=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/t-relay-$i.log" | head -1)
    url="https://127.0.0.1:$RPORT/wt"
    [ -z "$pin" ] && { echo "  relay failed"; return 1; }
  else
    pin="$afp"; url="https://127.0.0.1:$APORT/wt"
  fi

  ./bin/loadclient -label "term-$mode-$i" -url "$url" -pin "$pin" \
    -duration 60s >"$OUT/term-$mode-$i.txt" 2>&1 &
  local lc=$!
  sleep 8
  local t0
  t0=$(date +%s.%N)
  kill -TERM "$apid" 2>/dev/null
  wait "$lc" 2>/dev/null
  local dt
  dt=$(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.2f", b-a}')
  printf '  %-9s rep%d: client unblocked %6ss after SIGTERM   %s\n' \
    "$mode" "$i" "$dt" "$(grep -oP '^teardown +\K.*' "$OUT/term-$mode-$i.txt")"
  if [ "$mode" = relayed ]; then
    grep -a -oP 'relay\[s1\]: (upstream|client) leg ended \([a-z-]+\)' "$OUT/t-relay-$i.log" | sed 's/^/           relay saw: /'
  fi
  pkill -f "bin/relay -listen 127.0.0.1:$RPORT" 2>/dev/null
  pkill -f "bin/fakeagent -listen 127.0.0.1:$APORT" 2>/dev/null
}

echo "=== DIRECT: SIGTERM the agent 8s into a 60s session, x$N"
for i in $(seq 1 "$N"); do one direct "$i"; done
echo
echo "=== RELAYED: same, x$N"
for i in $(seq 1 "$N"); do one relayed "$i"; done
