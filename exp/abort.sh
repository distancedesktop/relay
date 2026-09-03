#!/usr/bin/env bash
# Does a client's abort code round-trip through the splice?
#
# T1 in the matrix showed one case where a hard abort was NOT observed by the relay
# at all: the relay sat until its 30s QUIC idle timeout, and the client's code 7
# became 0 upstream. T3 (direct, same client code, same abort) delivered
# "loadclient abort" to the agent instantly. One sample each is not a finding, so
# repeat both N times and count.
#
# Two distinct questions, and the log lines separate them:
#   "client leg ended (session-closed)"  -> the WT_CLOSE_SESSION capsule arrived
#   "client leg ended (idle-timeout)"    -> it did not; the timer tore things down
# and then whether the code the relay mirrored upstream was 7 or 0.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
PORT="${PORT:-53096}"
LOG="$OUT/relay-abort.log"
AGENT_FP=$(cat "$OUT/agent.fp")
N="${N:-6}"

pkill -f "bin/relay -listen 127.0.0.1:$PORT" 2>/dev/null
sleep 0.5

# 6s idle timeout, so a lost capsule is visible as a ~6s teardown instead of 30s and
# the run stays short. The timeout VALUE is not the finding; whether it is reached is.
./bin/relay -listen "127.0.0.1:$PORT" \
  -route "/wt=https://127.0.0.1:53080/wt,fingerprint=$AGENT_FP" \
  -max-idle 6s -stats 0 >"$LOG" 2>&1 &
sleep 1.3
FP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$LOG" | head -1)
if [ -z "$FP" ]; then echo "relay failed to start:"; tail -5 "$LOG"; exit 1; fi

AGLOG="$OUT/abort-agent.log"
: >"$AGLOG"
a0=$(stat -c %s "$OUT/agent.log")

echo "=== $N relayed aborts (abort 3s into a 20s session, fast reader) on :$PORT"
for i in $(seq 1 "$N"); do
  ./bin/loadclient -label "ab-relay-$i" -url "https://127.0.0.1:$PORT/wt" -pin "$FP" \
    -duration 20s -abort-after 3s >"$OUT/ab-relay-$i.txt" 2>&1
  sleep 8   # longer than the 6s idle timeout, so each session is fully resolved
done

echo "=== $N direct aborts (control: same client, no splice)"
for i in $(seq 1 "$N"); do
  ./bin/loadclient -label "ab-direct-$i" -url "https://127.0.0.1:53080/wt" -pin "$AGENT_FP" \
    -duration 20s -abort-after 3s >"$OUT/ab-direct-$i.txt" 2>&1
  sleep 1
done

tail -c "+$((a0 + 1))" "$OUT/agent.log" >"$AGLOG"

echo
echo "--- relay: how each client leg ended"
grep -a -oP 'client leg ended \(\K[a-z-]+' "$LOG" | sort | uniq -c
echo "--- relay: what code was mirrored upstream"
grep -a -oP 'client->upstream: (mirroring session close code=\d+|no session code to mirror)' "$LOG" | sort | uniq -c
echo "--- relay: session lifetimes (abort was at 3s)"
grep -a -oP 'session down after \K[0-9.]+s' "$LOG" | tr '\n' ' '; echo
echo "--- relay: stuck-goroutine warnings: $(grep -a -c 'did not exit' "$LOG")"
echo
echo "--- agent: what it saw for the RELAYED aborts vs the DIRECT ones"
grep -a -oP 'control: read ended kind=\K[a-z-]+( err=.*)?' "$AGLOG" | sort | uniq -c
echo "--- agent: full control-end lines"
grep -a 'control: read ended' "$AGLOG"

pkill -f "bin/relay -listen 127.0.0.1:$PORT"
