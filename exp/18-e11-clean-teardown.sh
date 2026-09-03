#!/usr/bin/env bash
# E11: separate "the relay told the client" from "the client's own timeout fired".
#
# E6c/6d killed with SIGKILL. Both the relay's QUIC MaxIdleTimeout and the
# loadclient's video read deadline are 30s, so the two fire simultaneously and the
# measurement cannot attribute the client's wake-up. SIGTERM makes fakeagent shut
# down cleanly, which sends a CONNECTION_CLOSE / session close capsule; a relay
# that propagates faithfully should close the client leg within milliseconds. If
# the client still takes ~30s, the relay is swallowing the upstream's close.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh

t_now() { date +%s.%N; }
dt() { python3 -c "print(round($2-$1,2))"; }

echo "############ 11a: SIGTERM the AGENT 8s into a 60s RELAYED session (x2) ############"
for rep in 1 2; do
  start_agent "e11a-$rep" -stats 30s
  start_relay "e11a-$rep" -stats 30s
  APID=$AGENT_PID
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s \
    -label "term-agent-relayed-$rep" -json > "$OUT/e11a-$rep.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(t_now); kill -TERM $APID
  wait $CLPID; T1=$(t_now)
  echo "  rep$rep: SIGTERM agent -> client returned after $(dt $T0 $T1)s"
  sumline "$OUT/e11a-$rep.json"
  grep -E "leg ended|mirroring session close|session down after" "$OUT/relay-e11a-$rep.log" | tail -4
  stop_all
done

echo
echo "############ 11a-direct control: SIGTERM the AGENT in a DIRECT session ############"
start_agent e11adir -stats 30s
APID=$AGENT_PID
sleep 0.3
"$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 60s \
  -label term-agent-direct -json > "$OUT/e11a-direct.json" 2>&1 &
CLPID=$!
sleep 8
T0=$(t_now); kill -TERM $APID
wait $CLPID; T1=$(t_now)
echo "  direct: SIGTERM agent -> client returned after $(dt $T0 $T1)s"
sumline "$OUT/e11a-direct.json"
stop_all

echo
echo "############ 11b: SIGTERM the RELAY 8s into a 60s relayed session (x2) ############"
for rep in 1 2; do
  start_agent "e11b-$rep" -stats 30s
  start_relay "e11b-$rep" -stats 30s
  RPID=$RELAY_PID
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 60s \
    -label "term-relay-$rep" -json > "$OUT/e11b-$rep.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(t_now); kill -TERM $RPID
  wait $CLPID; T1=$(t_now)
  echo "  rep$rep: SIGTERM relay -> client returned after $(dt $T0 $T1)s"
  sumline "$OUT/e11b-$rep.json"
  echo "  agent view:"; grep -E "read ended|write failed|session down" "$OUT/agent-e11b-$rep.log" | tail -3
  stop_all
done
