#!/usr/bin/env bash
# Experiment harness for the WebTransport terminating-relay spike.
#
# Everything is logged so FINDINGS.md can quote real output rather than a summary
# of it. Each experiment gets its own file under $OUT.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
mkdir -p "$OUT"

AGENT_PORT=53080
RELAY_PORT=53090
RELAY_SMALL_PORT=53091

fp_of() { grep -oP 'cert SHA-256 \K[0-9a-f]+' "$1" | head -1; }

start_agent() {
  local log="$OUT/agent.log"
  ./bin/fakeagent -listen "127.0.0.1:$AGENT_PORT" -stats 15s "$@" >"$log" 2>&1 &
  echo $! >"$OUT/agent.pid"
  sleep 1.3
  AGENT_FP=$(fp_of "$log")
  [ -n "$AGENT_FP" ] || { echo "agent failed to start"; tail -5 "$log"; exit 1; }
  echo "agent pid $(cat "$OUT/agent.pid") fp $AGENT_FP"
}

start_relay() {  # $1 = port, $2 = logfile, rest = extra flags
  local port="$1" log="$2"; shift 2
  ./bin/relay -listen "127.0.0.1:$port" \
    -route "/wt=https://127.0.0.1:$AGENT_PORT/wt,fingerprint=$AGENT_FP" \
    -route "/wt/desktop-1=https://127.0.0.1:$AGENT_PORT/wt,fingerprint=$AGENT_FP" \
    -stats 15s "$@" >"$log" 2>&1 &
  echo $! >"${log%.log}.pid"
  sleep 1.3
  RELAY_FP=$(fp_of "$log")
  [ -n "$RELAY_FP" ] || { echo "relay on $port failed to start"; tail -5 "$log"; exit 1; }
  echo "relay pid $(cat "${log%.log}.pid") port $port fp $RELAY_FP"
}

stop_all() {
  pkill -f 'bin/relay -listen' 2>/dev/null
  pkill -f 'bin/fakeagent -listen' 2>/dev/null
  sleep 1
}

case "${1:-}" in
  boot)
    stop_all
    start_agent
    echo "$AGENT_FP" >"$OUT/agent.fp"
    start_relay "$RELAY_PORT" "$OUT/relay-default.log" \
      -stream-window 0 -max-stream-window 0 -conn-window 0 -max-conn-window 0
    echo "$RELAY_FP" >"$OUT/relay-default.fp"
    start_relay "$RELAY_SMALL_PORT" "$OUT/relay-64k.log" \
      -stream-window 65536 -max-stream-window 65536 -conn-window 131072 -max-conn-window 131072
    echo "$RELAY_FP" >"$OUT/relay-64k.fp"
    ;;
  stop)
    stop_all
    echo stopped
    ;;
  *)
    echo "usage: run.sh boot|stop"
    ;;
esac
