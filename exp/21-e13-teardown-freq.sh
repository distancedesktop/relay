#!/usr/bin/env bash
# E13: how OFTEN does a clean upstream shutdown leave the client hanging?
#
# E11 saw 30.09s and 30.0s relayed vs 0.01s direct, but n=2 and E11b was
# nondeterministic (0.01s / 30.0s), so the frequency is the number that matters.
# 6 reps each. Also records the client's video vs control end kinds per rep,
# because E11a rep1 showed them DISAGREEING (control got the relay's close
# message, video timed out at 30s) which is the fingerprint of the mechanism.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
REPS=${1:-6}
t_now() { date +%s.%N; }
dt() { python3 -c "print(round($2-$1,2))"; }
endkinds() { python3 -c "
import json,sys
r=json.load(open(sys.argv[1]))
print('      video_end=%s control_end=%s errors=%s' % (r['video_end_kind'], r['control_end_kind'], (r.get('errors') or [])))
" "$1"; }

echo "########## E13: SIGTERM the agent 8s in; how long until the client notices? ##########"
echo "--- DIRECT (n=$REPS) ---"
for rep in $(seq 1 $REPS); do
  start_agent "e13d-$rep" -stats 60s
  APID=$AGENT_PID
  sleep 0.3
  "$BIN/loadclient" -url "https://$AGENT_ADDR/wt" -pin "$AGENT_FP" -duration 120s \
    -label "e13-direct-$rep" -json > "$OUT/e13-direct-$rep.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(t_now); kill -TERM $APID; wait $CLPID; T1=$(t_now)
  echo "  direct rep$rep: client woke after $(dt $T0 $T1)s"
  endkinds "$OUT/e13-direct-$rep.json"
  stop_all
done

echo "--- RELAYED (n=$REPS) ---"
for rep in $(seq 1 $REPS); do
  start_agent "e13r-$rep" -stats 60s
  start_relay "e13r-$rep" -stats 60s
  APID=$AGENT_PID
  sleep 0.4
  "$BIN/loadclient" -url "https://$RELAY_ADDR/wt" -pin "$RELAY_FP" -duration 120s \
    -label "e13-relayed-$rep" -json > "$OUT/e13-relayed-$rep.json" 2>&1 &
  CLPID=$!
  sleep 8
  T0=$(t_now); kill -TERM $APID; wait $CLPID; T1=$(t_now)
  echo "  relayed rep$rep: client woke after $(dt $T0 $T1)s"
  endkinds "$OUT/e13-relayed-$rep.json"
  grep -E "session down after" "$OUT/relay-e13r-$rep.log" | head -1 | sed 's/^/      relay: /'
  stop_all
done
