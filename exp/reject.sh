#!/usr/bin/env bash
# Failure-mode checks: everything that must be rejected cleanly, with the status the
# viewer actually sees.
set -u

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 1

OUT="${OUT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out}"
AGENT_FP=$(cat "$OUT/agent.fp")
RELAY_FP=$(cat "$OUT/relay-default.fp")

hdr() { echo; echo "=== $* ==="; }

hdr "R1 unknown path -> 404 (routing happens before any upstream work)"
echo "\$ ./bin/loadclient -url https://127.0.0.1:53090/wt/nosuchdesktop -pin <relay> -duration 1s"
./bin/loadclient -url https://127.0.0.1:53090/wt/nosuchdesktop -pin "$RELAY_FP" -duration 1s 2>&1 | tail -4

hdr "R2 known alternate path /wt/desktop-1 -> 200 (phonebook hit)"
echo "\$ ./bin/loadclient -url https://127.0.0.1:53090/wt/desktop-1 -pin <relay> -duration 2s"
./bin/loadclient -url https://127.0.0.1:53090/wt/desktop-1 -pin "$RELAY_FP" -duration 2s 2>&1 | grep -E "^(url|video|frames|teardown|fingerprint)"

hdr "R3 client pins the AGENT's cert against the relay -> TLS failure, client side"
echo "\$ ./bin/loadclient -url https://127.0.0.1:53090/wt -pin <AGENT fp> -duration 1s"
./bin/loadclient -url https://127.0.0.1:53090/wt -pin "$AGENT_FP" -duration 1s 2>&1 | tail -3

hdr "R4 foreign Origin -> relay refuses before dialling"
echo "\$ ./bin/loadclient -url https://127.0.0.1:53090/wt -pin <relay> -origin https://evil.example -duration 1s"
./bin/loadclient -url https://127.0.0.1:53090/wt -pin "$RELAY_FP" -origin https://evil.example -duration 1s 2>&1 | tail -3

hdr "R5 relay whose upstream pin is wrong -> 502 to the client"
./bin/relay -listen 127.0.0.1:53092 \
  -route "/wt=https://127.0.0.1:53080/wt,fingerprint=0000000000000000000000000000000000000000000000000000000000000000" \
  -stats 0 >"$OUT/relay-badpin.log" 2>&1 &
sleep 1.3
BADPIN_FP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/relay-badpin.log" | head -1)
echo "\$ ./bin/loadclient -url https://127.0.0.1:53092/wt -pin <relay-badpin> -duration 1s"
./bin/loadclient -url https://127.0.0.1:53092/wt -pin "$BADPIN_FP" -duration 1s 2>&1 | tail -3
echo "--- relay side:"
grep -a "upstream dial" "$OUT/relay-badpin.log" | tail -2
pkill -f 'bin/relay -listen 127.0.0.1:53092'

hdr "R6 upstream not running at all -> 502 to the client"
./bin/relay -listen 127.0.0.1:53093 \
  -route "/wt=https://127.0.0.1:53099/wt,fingerprint=$AGENT_FP" \
  -upstream-timeout 2s -stats 0 >"$OUT/relay-nodial.log" 2>&1 &
sleep 1.3
NODIAL_FP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/relay-nodial.log" | head -1)
echo "\$ ./bin/loadclient -url https://127.0.0.1:53093/wt -pin <relay-nodial> -connect-timeout 6s -duration 1s"
./bin/loadclient -url https://127.0.0.1:53093/wt -pin "$NODIAL_FP" -connect-timeout 6s -duration 1s 2>&1 | tail -3
echo "--- relay side:"
grep -a "upstream dial" "$OUT/relay-nodial.log" | tail -2
pkill -f 'bin/relay -listen 127.0.0.1:53093'

hdr "R7 no fingerprint configured and no -insecure-upstream -> refuses to trust anything"
./bin/relay -listen 127.0.0.1:53094 -upstream https://127.0.0.1:53080/wt -stats 0 >"$OUT/relay-nopin.log" 2>&1 &
sleep 1.3
NOPIN_FP=$(grep -oP 'cert SHA-256 \K[0-9a-f]+' "$OUT/relay-nopin.log" | head -1)
echo "\$ ./bin/loadclient -url https://127.0.0.1:53094/wt -pin <relay-nopin> -duration 1s"
./bin/loadclient -url https://127.0.0.1:53094/wt -pin "$NOPIN_FP" -duration 1s 2>&1 | tail -3
echo "--- relay side:"
grep -a "upstream dial" "$OUT/relay-nopin.log" | tail -2
pkill -f 'bin/relay -listen 127.0.0.1:53094'

hdr "R8 client offers an application protocol the relay cannot forward"
echo "\$ ./bin/loadclient -url https://127.0.0.1:53090/wt -pin <relay> -protocols distance/1 -duration 2s"
./bin/loadclient -url https://127.0.0.1:53090/wt -pin "$RELAY_FP" -protocols distance/1 -duration 2s 2>&1 | grep -E "^(negotiated|video|frames|teardown)"
