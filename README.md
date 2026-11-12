# Distance Relay

A WebTransport relay that terminates a browser session, dials the real agent, and splices the two — so viewers never handle certificate fingerprints directly.

Status: tested on localhost against a fake agent (identical speed, +0.2ms delay) and against the live agent (control + error pass-through verified, video blocked only by a locked screen at the time). See `FINDINGS.md` for numbers.

## Layout

- `cmd/relay/` — the relay. Listens on one port, dials upstream per session, splices both directions with a fixed 32KB chunk. Start here.
- `cmd/fakeagent/` — pretend agent for lab tests. Speaks the same protocol, makes fake video frames.
- `cmd/loadclient/` — test client. Runs a full session (list → start → watch → stop) and prints speed, delay, and errors. Works direct or through the relay.
- `internal/` — error-code translation between WebTransport and HTTP/3, plus protocol message types.
- `exp/` — measurement scripts from the original experiment.
- `SPIKE.md` — the original question and verified protocol facts.
- `FINDINGS.md` — the verdict with measured numbers.

## Quick start

```bash
go build ./...

# Fake agent on :53180, relay in front on :53190, client through the relay:
./fakeagent -listen 127.0.0.1:53180 &
./relay -listen 127.0.0.1:53190 \
  -upstream https://127.0.0.1:53180/wt \
  -upstream-fingerprint <fakeagent-fp> \
  -stream-window 65536 -max-stream-window 65536 \
  -conn-window 131072 -max-conn-window 131072 &
./loadclient -url https://127.0.0.1:53190/wt -pin <relay-fp> -duration 10s
```

Against a real agent, point `-upstream` at it with its fingerprint and pass an `-upstream-origin` the agent accepts. Keep the 64KB window flags — without them a slow viewer builds a multi-second queue (see `FINDINGS.md`, "The central finding").

## Viewer token

The agent refuses WebTransport upgrades without its viewer token (`distance-agent --print-token`). By default the relay forwards each viewer's own token upstream as `Authorization: Bearer` — browsers send it as `?token=` on the relay URL, `loadclient` with `-token` — so the relay grants no access the viewer did not already have. Setting a token on a route (`-route /wt/d1=<url>,token=<tok>`, `"token"` in the `-routes` JSON, or `-upstream-token-file`) makes the relay present it for every viewer of that route instead; only do that when the relay itself is access-controlled.

## What the browser pins

Only the relay's certificate. The agent's fingerprint stays server-side in `-upstream-fingerprint`. If the relay serves a publicly trusted cert, browsers need no pin at all.
