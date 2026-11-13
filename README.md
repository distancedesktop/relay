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
  -upstream-fingerprint <fakeagent-fp> &
./loadclient -url https://127.0.0.1:53190/wt -pin <relay-fp> -duration 10s
```

Against a real agent, point `-upstream` at it with its fingerprint and pass an `-upstream-origin` the agent accepts. The flow-control windows default to 64 KB per stream and 128 KB per connection (initial and max) on both legs. Don't raise them, and don't pass `0` (quic-go's 512 KB / 6 MB defaults): a slow viewer then builds a multi-second queue (see `FINDINGS.md`, "The central finding").

## What the browser pins

Only the relay's certificate. The agent's fingerprint stays server-side in `-upstream-fingerprint`. If the relay serves a publicly trusted cert, browsers need no pin at all.
