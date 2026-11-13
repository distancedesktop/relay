#!/usr/bin/env bash
# Harness for the relay skeptic runs. Absolute paths only.
#
# Paths were originally job-scratch absolutes; they now resolve relative to this
# file so the committed scripts still run. Override OUT to keep output elsewhere.
BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin"
OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
mkdir -p "$OUT"
# Ports are overridable so a second, independent track can run in parallel on a
# different port set. Defaults are the track-1 ports.
AGENT_ADDR=${AGENT_ADDR:-127.0.0.1:53180}
RELAY_ADDR=${RELAY_ADDR:-127.0.0.1:53190}
RELAY2_ADDR=${RELAY2_ADDR:-127.0.0.1:53191}

AGENT_PID=""
RELAY_PID=""
RELAY2_PID=""

# The relay now defaults to 64 KB/128 KB windows; pass these to get the quic-go
# defaults the "default" baselines below were measured with.
QUICGO_WINDOWS="-stream-window 0 -max-stream-window 0 -conn-window 0 -max-conn-window 0"

start_agent() { # $1=tag, rest=extra flags
  local tag="$1"; shift
  AGENT_LOG="$OUT/agent-$tag.log"
  rm -f "$AGENT_LOG"
  "$BIN/fakeagent" -listen "$AGENT_ADDR" "$@" > "$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  local _w; for _w in $(seq 1 100); do
    AGENT_FP=$(awk '/cert SHA-256/{print $NF}' "$AGENT_LOG" 2>/dev/null)
    [ -n "$AGENT_FP" ] && break
    sleep 0.05
  done
  [ -z "$AGENT_FP" ] && { echo "FATAL: no agent fingerprint"; cat "$AGENT_LOG"; exit 1; }
  echo "agent pid=$AGENT_PID fp=${AGENT_FP:0:16} flags: $*"
}

start_relay() { # $1=tag, rest=extra flags
  local tag="$1"; shift
  RELAY_LOG="$OUT/relay-$tag.log"
  rm -f "$RELAY_LOG"
  "$BIN/relay" -listen "$RELAY_ADDR" \
    -route "/wt=https://$AGENT_ADDR/wt,fingerprint=$AGENT_FP" "$@" > "$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  local _w; for _w in $(seq 1 100); do
    RELAY_FP=$(awk "/cert SHA-256/{print \$NF}" "$RELAY_LOG" 2>/dev/null)
    [ -n "$RELAY_FP" ] && break
    sleep 0.05
  done
  [ -z "$RELAY_FP" ] && { echo "FATAL: no relay fingerprint"; cat "$RELAY_LOG"; exit 1; }
  echo "relay pid=$RELAY_PID fp=${RELAY_FP:0:16} flags: $*"
}

# second relay hop: relay2 listens on RELAY2_ADDR and forwards to the agent;
# relay (hop 1) then points at relay2 instead of the agent.
start_relay2_chain() { # $1=tag, rest=extra flags applied to BOTH hops
  local tag="$1"; shift
  RELAY2_LOG="$OUT/relay2-$tag.log"
  rm -f "$RELAY2_LOG"
  "$BIN/relay" -listen "$RELAY2_ADDR" \
    -route "/wt=https://$AGENT_ADDR/wt,fingerprint=$AGENT_FP" "$@" > "$RELAY2_LOG" 2>&1 &
  RELAY2_PID=$!
  local _w; for _w in $(seq 1 100); do
    RELAY2_FP=$(awk '/cert SHA-256/{print $NF}' "$RELAY2_LOG" 2>/dev/null)
    [ -n "$RELAY2_FP" ] && break
    sleep 0.05
  done
  [ -z "$RELAY2_FP" ] && { echo "FATAL: no relay2 fingerprint"; cat "$RELAY2_LOG"; exit 1; }
  RELAY_LOG="$OUT/relay-$tag.log"
  rm -f "$RELAY_LOG"
  "$BIN/relay" -listen "$RELAY_ADDR" \
    -route "/wt=https://$RELAY2_ADDR/wt,fingerprint=$RELAY2_FP" "$@" > "$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
  local _w; for _w in $(seq 1 100); do
    RELAY_FP=$(awk "/cert SHA-256/{print \$NF}" "$RELAY_LOG" 2>/dev/null)
    [ -n "$RELAY_FP" ] && break
    sleep 0.05
  done
  echo "relay2 pid=$RELAY2_PID fp=${RELAY2_FP:0:16} ; relay1 pid=$RELAY_PID fp=${RELAY_FP:0:16}"
}

stop_all() {
  for p in $RELAY_PID $RELAY2_PID $AGENT_PID; do
    [ -n "$p" ] && kill "$p" 2>/dev/null
  done
  sleep 0.4
  for p in $RELAY_PID $RELAY2_PID $AGENT_PID; do
    [ -n "$p" ] && kill -9 "$p" 2>/dev/null
  done
  RELAY_PID=""; RELAY2_PID=""; AGENT_PID=""
}

# summarise one loadclient JSON report onto one line
sumline() { # $1=json file
  python3 - "$1" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
print(("%-28s bytes=%-9d frames=%-6d secs=%6.2f mbps=%6.3f p50=%9.1f p99=%9.1f max=%9.1f "
       "gaps=%-5d ooo=%-3d first_byte=%7.1f startRTT=%6.1f stopRTT=%9.1f end=%s/%s") % (
  r.get("label",""), r["video_bytes"], r["video_frames"], r["video_seconds"], r["throughput_mbps"],
  r["frame_latency_p50_ms"], r["frame_latency_p99_ms"], r["frame_latency_max_ms"],
  r["frame_number_gaps"], r["frames_out_of_order"], r["first_video_byte_ms_after_start"],
  r["start_rtt_ms"], r["stop_rtt_ms"], r["video_end_kind"], r["control_end_kind"]))
for e in r.get("errors",[]) or []:
    print("      ! "+e)
PY
}
