#!/usr/bin/env bash
# E9: many concurrent sessions through one relay. Each relayed session is its own
# upstream QUIC connection, so N sessions means N x (two connections + two splice
# goroutine pairs). Question: does per-session cost stay linear, does any session
# starve, and do goroutines/FDs come back after all clients leave?
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
N=${1:-8}
DUR=${2:-30s}
start_agent e9 -stats 30s
start_relay e9 -stats 30s
sleep 0.4
snap() { # $1=label
  printf "%-10s relay: rss=%skB thr=%s fd=%s cpu=%ss | agent: rss=%skB thr=%s cpu=%ss\n" "$1" \
    "$(awk '/VmRSS/{print $2}' /proc/$RELAY_PID/status)" "$(awk '/Threads/{print $2}' /proc/$RELAY_PID/status)" \
    "$(ls /proc/$RELAY_PID/fd | wc -l)" "$(awk '{print ($14+$15)/100}' /proc/$RELAY_PID/stat)" \
    "$(awk '/VmRSS/{print $2}' /proc/$AGENT_PID/status)" "$(awk '/Threads/{print $2}' /proc/$AGENT_PID/status)" \
    "$(awk '{print ($14+$15)/100}' /proc/$AGENT_PID/stat)"
}
snap before
pids=""
for i in $(seq 1 $N); do
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration "$DUR" \
    -label "conc-$i" -json > "$OUT/e9-$i.json" 2>&1 &
  pids="$pids $!"
done
sleep 5
snap "mid(${N}up)"
for p in $pids; do wait $p; done
snap after
sleep 3
snap "after+3s"
echo "--- per-session client results ---"
for i in $(seq 1 $N); do sumline "$OUT/e9-$i.json"; done
echo "--- total agent bytes ---"
grep "session down" "$AGENT_LOG"
echo "--- relay: sessions up/down counts ---"
echo "up=$(grep -c 'spliced' "$RELAY_LOG") down=$(grep -c 'session down after' "$RELAY_LOG") warnings=$(grep -c WARNING "$RELAY_LOG")"
grep -E "u->c bytes=" "$RELAY_LOG" | tail -$N
stop_all
