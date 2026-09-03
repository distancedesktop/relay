# Distance Relay — termination spike

Throwaway prototype. **Not a repo, not a PR, not shipped.** Question to answer:

> Can a relay *terminate* a browser's WebTransport session, re-dial the real
> agent, and splice the two so the viewer works unchanged — while the browser
> trusts only the relay's certificate and never learns the agent's fingerprint?

If yes, Distance gets: no fingerprint pinning, no QR/paste dance, no 13-day cert
rotation visible to users, auth enforced before a packet reaches any agent, and a
real desktop phonebook keyed on a path or token instead of DNS/SNI.

## Verdict

**Yes.** Built and measured: `FINDINGS.md` has the numbers. Goodput through the splice
is identical to direct (byte for byte, 2 Mbit/s through 800 Mbit/s), a fast viewer
pays +0.16 ms of frame latency, control RTT is unaffected even over 200 ms of real
RTT, and the relay costs ~2.4% of one core at 2 Mbit/s with flat RSS. The browser
pins only the relay's cert; the agent's fingerprint arrives solely as application
data it may ignore. Origin and route rejections happen before any packet reaches an
agent.

Hard problem (4), backpressure, is **not** the blocker it looked like. The relay holds
one 32 KB chunk per direction, never more, so flow control propagates client → relay →
agent and the agent's own `Write` blocks. A slow viewer gets the same goodput relayed
as direct; the entire cost is one extra QUIC receive window of queueing delay,
predictable to within 4% as (sum of windows on path) ÷ drain rate. That window is a
config value: 64 KB stream windows (initial *and* max) make the relayed path
indistinguishable from
direct. Left at the quic-go 6 MB maximum it produces a 103 s video queue and a 24.7 s
`stop` RTT — so pinning it small is a requirement, not a tuning nicety.

Two library defects, neither fatal: an intermittent 2-goroutine leak in
`handleSessionGoneError` (`stream.go:78` removes the stream from the map that
`streams_map.go:34-42` must iterate to unblock line 112) — avoidable relay-side with a
write deadline; and a hard client abort's application code is lost in the 10 ms
capsule race at `session.go:413` before the relay can see it, so close reasons belong
in application JSON. Application-protocol negotiation cannot be forwarded because
`upgrader.go:134` commits the 200 first.

## Why this is not a solved problem

No reverse proxy does this. Caddy's `reverse_proxy` speaks HTTP/1.1 and 2
upstream; HTTP/3-to-backend landed (caddyserver/caddy#6312, merged 2024-05-20)
but only round-trips request/response pairs. A WebTransport session is a
long-lived extended CONNECT tunnel, so proxying it means owning:

1. **Stream mapping** — every client-initiated bidi and uni stream needs a peer
   stream upstream, and vice versa for server-initiated ones. Distance uses one
   client bidi (JSON control) and server-initiated uni streams (H.264), so at
   minimum both directions must work.
2. **Datagram relay** — must stay unreliable. Buffering to "help" breaks the
   semantics.
3. **Error-code translation** — `webtransport-go`'s `errors.go` maps WT codes
   into HTTP/3 stream codes as `firstErrorCode + n + n/0x1e`. A proxy has to
   unmap and remap faithfully or a clean `stream-ended` becomes
   indistinguishable from a fault.
4. **Backpressure** — two independently congestion-controlled QUIC connections
   spliced together. Naive `io.Copy` means the browser's flow-control window no
   longer reflects the agent's send rate.

(4) is the one that decides whether this is viable, and it is the reason the
spike measures rather than just demos.

## Ground truth about the protocol

Verified against the live agent, not assumed:

- Endpoint `https://<host>:52020/wt`. Control is **one client-initiated bidi
  stream**, newline-delimited JSON. Video is **server-initiated uni streams**
  carrying raw H.264 Annex B.
- The agent pushes `fingerprint-refresh` then an **unsolicited `displays`** right
  after the control stream opens, before any request.
- Control verbs the agent implements: `list-displays`, `start`, `stop`. Anything
  else returns `{"type":"error","message":"unknown type: <t>"}` — there is **no
  `input` and no `ping` handler**.
- The agent's upgrader sets `ApplicationProtocols: ["moq-lite-04"]`, but that is
  a negotiation **allow-list**: a client offering no protocol still upgrades
  (`selectProtocol` on an absent header returns `""`, the `wt-protocol` response
  header is omitted, and `Upgrade` proceeds to 200). Do not treat a missing
  protocol as an error.
- The agent enforces `Origin`: empty, matching `Host`, its own `:52022`, or one
  passed via `--allow-origin`. A relay must send an acceptable `Origin` upstream
  or get **400 before the session opens**.
- `serverCertificateHashes` pinning skips CA *and* hostname validation, which is
  why the agent's self-signed cert works today. Chrome caps such certs at 14
  days validity; the agent issues 13-day ECDSA P-256.

## Layout

- `cmd/relay/` — the relay
- `cmd/fakeagent/` — a stand-in agent speaking the same protocol, so the spike
  runs without the real one
- `cmd/loadclient/` — a Go WebTransport client that measures throughput,
  latency and loss through the relay vs. direct
- `FINDINGS.md` — measurements and the verdict
