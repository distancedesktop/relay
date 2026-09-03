#!/usr/bin/env bash
# Goroutine-leak reproducer.
#
# Hypothesis: when a client session closes while the relay is blocked writing video
# DOWN to that client, the relay's splice goroutine never exits. Suspected cause,
# read from webtransport-go stream.go:75-119:
#
#   Write -> quic write fails with WTSessionGoneErrorCode
#         -> line 78 calls s.onClose(), which REMOVES the stream from streamsMap
#         -> line 82 calls handleSessionGoneError(), which waits on s.closed
#         -> s.closed is only closed by closeWithSession, reached only by
#            streamsMap.CloseSession iterating the map the stream was just removed from
#
# If that is right the wait is not "until the session closes" but forever, and the
# leak is 2 goroutines per affected session (the wedged pump plus the wg.Wait watcher).
#
# Each iteration: slow reader so the downstream write is certainly blocked, then a
# hard abort so the session dies mid-write.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
PORT="${PORT:-53095}"
LOG="$OUT/relay-leak.log"
AGENT_FP=$(cat "$OUT/agent.fp")
N="${N:-4}"

pkill -f "bin/relay -listen 127.0.0.1:$PORT" 2>/dev/null
sleep 0.5

# 2s stats so the goroutine baseline is sampled between sessions, and a 5s idle
# timeout so a vanished client does not hold the run open for 30s.
./bin/relay -listen "127.0.0.1:$PORT" \
  -route "/wt=https://127.0.0.1:53080/wt,fingerprint=$AGENT_FP" \
  -stats 2s -max-idle 5s >"$LOG" 2>&1 &
sleep 1.3
FP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$LOG" | head -1)
if [ -z "$FP" ]; then echo "relay failed to start:"; tail -5 "$LOG"; exit 1; fi
echo "leak-test relay on $PORT"

echo "--- baseline before any session:"
sleep 2.5
grep -a -oP 'goroutines=\d+ sessions_live=\w+' "$LOG" | tail -1

for i in $(seq 1 "$N"); do
  ./bin/loadclient -label "leak-$i" -url "https://127.0.0.1:$PORT/wt" -pin "$FP" \
    -read-rate 30000 -duration 20s -abort-after 6s >"$OUT/leak-$i.txt" 2>&1
  # The client exits at ~6s. After that: ~5s for the idle timeout to notice the
  # vanished client, then 5s for the splice's wg.Wait watchdog to give up and dump
  # stacks, then a 2s stats tick to sample the idle goroutine count. 14s covers it.
  sleep 14
  echo "--- after session $i: $(grep -a -oP 'goroutines=\d+ sessions_live=false' "$LOG" | tail -1), warnings so far: $(grep -a -c 'did not exit' "$LOG")"
done

echo
echo "--- stuck-goroutine warnings: $(grep -a -c 'WARNING' "$LOG")"
echo "--- goroutine timeline (idle samples only):"
grep -a -oP 'goroutines=\d+ sessions_live=false' "$LOG" | uniq -c

pkill -f "bin/relay -listen 127.0.0.1:$PORT"
