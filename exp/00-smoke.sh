#!/usr/bin/env bash
# smoke: start my own agent+relay pair, verify ports, tear down.
source "$(dirname "${BASH_SOURCE[0]}")"/lib.sh
start_agent smoke
start_relay smoke
sleep 0.5
echo "AGENT_FP=$AGENT_FP"
echo "RELAY_FP=$RELAY_FP"
ss -lunp 2>/dev/null | grep -E "531(80|90|91)"
echo "--- agent log ---"
cat "$AGENT_LOG"
echo "--- relay log ---"
cat "$RELAY_LOG"
stop_all
