#!/usr/bin/env bash
# E10: three failure questions the earlier experiments do not answer.
#
# 10a FROZEN VIEWER. A client that stops reading but keeps its connection alive
#     (ACKs still flow, so no idle timeout). Both legs are then stuck with zero
#     progress. Does the relay hold both sessions and the agent's pump open
#     indefinitely, and how much memory does it hold while doing so?
#
# 10b BLOCKED-SPLICE RELEASE. Client reads slowly (so the relay is parked inside
#     dst.Write) and then vanishes. Does the blocked splice goroutine get released
#     promptly, or does the relay sit until the 30s QUIC idle timeout? A leak here
#     is per-abandoned-session, so it matters more than any latency number.
#
# 10c DATAGRAMS DURING A STREAM STALL. Datagrams must not be head-of-line blocked
#     by the stalled video splice. Slow reader + datagrams: are they still echoed?
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh

echo "############ 10a: frozen viewer (read-rate 500 B/s) for 45s, relayed ############"
start_agent e10a -stats 15s -stall-log 1ms
start_relay e10a -stats 15s
sleep 0.4
"$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 45s -read-rate 500 \
  -label frozen-viewer -json > "$OUT/e10a.json" 2>&1 &
CLPID=$!
for t in 10 20 30 40; do
  sleep 10
  printf "  t=%ss relay rss=%skB thr=%s fd=%s cpu=%ss | agent rss=%skB thr=%s cpu=%ss | agent frames=%s\n" "$t" \
    "$(awk '/VmRSS/{print $2}' /proc/$RELAY_PID/status)" "$(awk '/Threads/{print $2}' /proc/$RELAY_PID/status)" \
    "$(ls /proc/$RELAY_PID/fd | wc -l)" "$(awk '{print ($14+$15)/100}' /proc/$RELAY_PID/stat)" \
    "$(awk '/VmRSS/{print $2}' /proc/$AGENT_PID/status)" "$(awk '/Threads/{print $2}' /proc/$AGENT_PID/status)" \
    "$(awk '{print ($14+$15)/100}' /proc/$AGENT_PID/stat)" \
    "$(grep -o 'frames=[0-9]*' "$AGENT_LOG" | tail -1)"
done
wait $CLPID
sumline "$OUT/e10a.json"
sleep 1.5
echo "  --- agent: did its pump stay blocked the whole time? ---"
grep -E "session down|stats frames" "$AGENT_LOG" | tail -4
echo "  --- relay u->c ---"
grep "u->c bytes=" "$RELAY_LOG" | tail -1
stop_all

echo
echo "############ 10b: slow reader, then client vanishes while the splice is blocked ############"
for i in 1 2; do
  start_agent "e10b-$i" -stats 15s
  start_relay "e10b-$i" -stats 15s
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s -read-rate 50000 \
    -abort-after 15s -label "slow-abort-$i" -json > "$OUT/e10b-$i.json" 2>&1
  T0=$(date +%s.%N)
  sumline "$OUT/e10b-$i.json"
  # poll for the relay's session-down line, up to 40s (idle timeout is 30s)
  for j in $(seq 1 400); do
    grep -q "session down after" "$RELAY_LOG" && break
    sleep 0.1
  done
  T1=$(date +%s.%N)
  echo "  relay logged session-down $(python3 -c "print(round($T1-$T0,2))")s after the client returned"
  grep -E "leg ended|session down after|write blocked" "$RELAY_LOG" | tail -5
  echo "  --- agent: did it learn? ---"
  grep -E "session down|read ended|write failed" "$OUT/agent-e10b-$i.log" | tail -3
  stop_all
done

echo
echo "############ 10c: datagrams while the video splice is stalled ############"
start_agent e10c -stats 20s
start_relay e10c -stats 20s
sleep 0.4
for i in 1 2; do
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 30s -read-rate 50000 \
    -datagrams 20 -label "dgram-slow-$i" -json > "$OUT/e10c-$i.json" 2>&1
  python3 - "$OUT/e10c-$i.json" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
print("  %-18s datagrams sent=%s echoed=%s | video bytes=%d p50=%.0fms" % (
  r.get("label"), r.get("datagrams_sent"), r.get("datagrams_echoed"), r["video_bytes"], r["frame_latency_p50_ms"]))
for e in (r.get("errors") or []): print("    ! "+e)
PY
done
grep -E "datagram|too_big|dropped" "$RELAY_LOG" | tail -4
stop_all
