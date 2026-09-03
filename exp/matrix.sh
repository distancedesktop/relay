#!/usr/bin/env bash
# Experiment matrix. One run per case (not two — a second run for JSON would double
# the wall clock and the text report already carries every number FINDINGS.md needs).
#
# For each case the relay/agent log growth during the run is captured separately, so
# a stall line can be attributed to the experiment that caused it.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
AGENT_FP=$(cat "$OUT/agent.fp")
RELAY_FP=$(cat "$OUT/relay-default.fp")
SMALL_FP=$(cat "$OUT/relay-64k.fp")

DIRECT="https://127.0.0.1:53080/wt"
RELAY="https://127.0.0.1:53090/wt"
SMALL="https://127.0.0.1:53091/wt"

# lc <name> <url> <pin> [flags...]
lc() {
  local name="$1" url="$2" pin="$3"; shift 3
  local a0 r0 s0
  a0=$(stat -c %s "$OUT/agent.log")
  r0=$(stat -c %s "$OUT/relay-default.log")
  s0=$(stat -c %s "$OUT/relay-64k.log")

  echo "### $name"
  echo "\$ ./bin/loadclient -label '$name' -url $url -pin <pin> $*"
  ./bin/loadclient -label "$name" -url "$url" -pin "$pin" "$@" 2>&1 | tee "$OUT/$name.txt"

  tail -c "+$((a0 + 1))" "$OUT/agent.log"        >"$OUT/$name.agentlog"
  tail -c "+$((r0 + 1))" "$OUT/relay-default.log" >"$OUT/$name.relaylog"
  tail -c "+$((s0 + 1))" "$OUT/relay-64k.log"     >"$OUT/$name.relay64klog"
  echo
}

case "${1:-}" in
  fast)
    lc F1-direct-fast   "$DIRECT" "$AGENT_FP" -duration 20s -datagrams 25
    lc F2-relay-fast    "$RELAY"  "$RELAY_FP" -duration 20s -datagrams 25
    ;;
  slow)
    lc S1-direct-slow   "$DIRECT" "$AGENT_FP" -duration 30s -read-rate 125000
    lc S2-relay-slow    "$RELAY"  "$RELAY_FP" -duration 30s -read-rate 125000
    lc S3-relay-slow64k "$SMALL"  "$SMALL_FP" -duration 30s -read-rate 125000
    ;;
  veryslow)
    # 45s, not 30s: the queue must reach steady state before the latency figure
    # means anything, and at 40 kB/s the predicted steady state is ~11 s direct and
    # ~23 s relayed.
    lc V1-direct-vslow  "$DIRECT" "$AGENT_FP" -duration 45s -read-rate 40000
    lc V2-relay-vslow   "$RELAY"  "$RELAY_FP" -duration 45s -read-rate 40000
    lc V3-relay-vslow64k "$SMALL" "$SMALL_FP" -duration 45s -read-rate 40000
    ;;
  sustained)
    lc L1-direct-3min   "$DIRECT" "$AGENT_FP" -duration 180s -datagrams 60
    lc L2-relay-3min    "$RELAY"  "$RELAY_FP" -duration 180s -datagrams 60
    ;;
  teardown)
    lc T1-relay-abort    "$RELAY" "$RELAY_FP" -duration 30s -abort-after 5s
    lc T2-relay-skipstop "$RELAY" "$RELAY_FP" -duration 6s -skip-stop
    lc T3-direct-abort   "$DIRECT" "$AGENT_FP" -duration 30s -abort-after 5s
    ;;
  datagrams)
    lc D1-relay-dgram-load "$RELAY" "$RELAY_FP" -duration 20s -datagrams 200
    lc D2-direct-dgram-load "$DIRECT" "$AGENT_FP" -duration 20s -datagrams 200
    ;;
esac
