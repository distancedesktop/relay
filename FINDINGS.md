# FINDINGS — WebTransport terminating relay

**Verdict: termination is viable.** A relay can terminate a browser's WebTransport
session, re-dial the real agent, and splice the two. An unmodified viewer works. The
browser trusts only the relay's certificate and never learns the agent's fingerprint.

Cost, measured rather than assumed:

| | direct | relayed | delta |
|---|---|---|---|
| goodput, 2 Mbit/s stream | 2.010 Mbit/s | 2.010 Mbit/s | **none** |
| goodput, 800 Mbit/s stream | 792.3 Mbit/s | 790.7 Mbit/s | **none** |
| frame latency p50, fast reader @2 Mbit/s | 0.26 ms | 0.42 ms | **+0.16 ms** |
| frame latency p50, fast reader @50 Mbit/s | 0.9 ms | 1.2 ms | **+0.3 ms** |
| frame latency p50, fast reader @800 Mbit/s | 2.4 ms | 2.8 ms | **+0.4 ms** |
| control RTT (`start`/`stop`) | 0.24 ms | 0.44 ms | +0.2 ms |
| control RTT over 200 ms injected RTT | 202.5 ms | 202.9 ms | **+0.4 ms** |
| relay CPU, 285 s @2 Mbit/s | — | 6.81 s | ~2.4% of one core |
| relay RSS, 300 s | — | 16.3 MB, flat | no growth |
| frame integrity, every run | gaps 0, ooo 0 | gaps 0, ooo 0 | none |

The one real cost is **queueing delay for a slow viewer**, and it is a configuration
value, not a wall. See "The central finding" below.

Two defects found, both in `webtransport-go`, both characterised to a line number.
Neither blocks the approach. One is fixable relay-side; the other loses a client's
application close code before the relay can see it.

## Scope and honesty about it

Everything here is localhost loopback on one machine, against `cmd/fakeagent`, a
stand-in that speaks the protocol from `SPIKE.md` but is not the real agent. Loopback
has no loss, no reordering and ~0 RTT, so this measures the splice's own cost, not
the internet's. Two mitigations were applied: a `delayproxy` injecting 200 ms of
symmetric RTT (E7), and bitrates swept to 800 Mbit/s (E5b) so the relay is not
merely idling. Not tested: real packet loss, NAT rebinding, a real browser, more
than 8 concurrent sessions, or the real agent on `:52020`.

## What was built

- `cmd/fakeagent/` (549 lines) — agent stand-in. One client bidi carrying
  newline-delimited JSON; pushes `fingerprint-refresh` then an unsolicited
  `displays` on connect; handles `list-displays`, `start`, `stop`, and answers
  anything else with `{"type":"error","message":"unknown type: X"}`. On `start` it
  opens a server-initiated uni stream of synthetic H.264 Annex B (real
  `00 00 00 01` start codes, fake SPS/PPS/IDR then P-frames) with a 12-byte
  in-payload stamp (frame number + `UnixNano`) so one-way latency is measurable
  rather than inferred. Self-signed ECDSA P-256, prints its SHA-256.
- `cmd/relay/` (1215 lines) — the experiment. Terminates WebTransport on one UDP
  port, dials an upstream per accepted session, splices both.
- `cmd/loadclient/` (846 lines) — Go client driving a full session
  (`list-displays` → `start` → read for N seconds → `stop`), reporting throughput,
  first-byte latency, control RTT, frame gaps, and every stream/session error. Runs
  direct and through the relay so the two are directly comparable.
- `internal/wterr/` (586 lines incl. 295 of tests) — error-code translation.

`go build ./...`, `go vet ./...`, `gofmt -l cmd internal` and
`go test ./internal/... ./cmd/relay/` are all clean.

No dependency beyond the two already in `go.mod`
(`github.com/okdaichi/webtransport-go v0.10.2-okdaichi.1`, `quic-go v0.59.0`) was
needed or added.

## The central finding: the splice costs latency, not throughput

A slow reader gets **identical goodput** direct and relayed, byte for byte. The
entire cost of termination is queueing delay, and the queue is not in the relay — the
relay holds one 32 KB chunk per direction, forever, in every run ever made. The queue
is in the **QUIC receive windows on the path**, and splicing adds one more of them.

```
$ ./bin/loadclient -url https://127.0.0.1:53080/wt -pin <agent> -read-rate 125000 -duration 30s   # S1
$ ./bin/loadclient -url https://127.0.0.1:53090/wt -pin <relay> -read-rate 125000 -duration 30s   # S2
$ ./bin/loadclient -url https://127.0.0.1:53091/wt -pin <relay-64k> -read-rate 125000 -duration 30s   # S3
```

| | goodput | frames | p50 | p99 | max |
|---|---|---|---|---|---|
| S1 direct @125 kB/s | 1.008 Mbit/s | 958 | 3481.99 ms | 3900.93 ms | 4039.61 ms |
| S2 relay, default windows | 1.008 Mbit/s | 953 | **7522.08 ms** | 8125.80 ms | 8255.20 ms |
| S3 relay, 64 KB windows | 1.008 Mbit/s | 953 | **3964.52 ms** | 4413.84 ms | 4560.45 ms |
| V1 direct @40 kB/s | 0.326 Mbit/s | 447 | 11240.86 ms | 12821.94 ms | 13388.61 ms |
| V2 relay, default windows | 0.326 Mbit/s | 447 | **19206.86 ms** | 25807.10 ms | 26509.56 ms |
| V3 relay, 64 KB windows | 0.326 Mbit/s | 447 | 12872.65 ms | 14453.77 ms | 15076.14 ms |

The "64 KB windows" rows are
`-stream-window 65536 -max-stream-window 65536 -conn-window 131072 -max-conn-window 131072`.
Both the initial *and* max stream window must be set: leaving `-max-stream-window` at
the 6 MB default lets quic-go auto-tune straight back up under load.

Doubling the windows on the path doubles the latency; shrinking the relay's window
back to 64 KB recovers direct-equivalent latency. **64 KB relay windows make the
relayed path indistinguishable from direct for a slow viewer.**

### The model, and it predicts to within 4%

Steady-state queue = sum of receive windows on the path. End-to-end frame delay =
that sum ÷ the rate the viewer actually drains. Using max observed frame latency as
the steady-state estimate (p50 over a 30–45 s run is still climbing while the queue
fills):

| run | drain B/s | windows on path | predicted bytes | max latency | implied buffer | ratio |
|---|---|---|---|---|---|---|
| S1 direct | 125000 | 512K (client) | 524288 | 4039.61 ms | 504951 | 0.963 |
| S2 relay default | 125000 | 512K + 512K | 1048576 | 8255.20 ms | 1031900 | 0.984 |
| S3 relay 64k | 125000 | 64K + 512K | 589824 | 4560.45 ms | 570056 | 0.966 |
| V1 direct | 40000 | 512K (client) | 524288 | 13388.61 ms | 535544 | 1.021 |
| V2 relay default | 40000 | 512K + 512K | 1048576 | 26509.56 ms | 1060382 | 1.011 |
| V3 relay 64k | 40000 | 64K + 512K | 589824 | 15076.14 ms | 603046 | 1.022 |

Every point within 4%. This matters because it means the cost is **predictable and
tunable**, not emergent from splicing two congestion controllers.

### Independently confirmed by byte accounting

A window sweep at 50 kB/s / 60 s reconciled three counters — what the agent handed to
QUIC, what the client read, and where the difference sat:

| relay window | p50 | buffered bytes | in relay's upstream window | in client window + wire |
|---|---|---|---|---|
| direct (no relay) | 9014 ms | 448405 | — | 448405 |
| default (512K) | 18965 ms | 977211 | 404354 | 458752 |
| 64 KB | 10298 ms | 450607 | **0** | 450607 |
| 16 KB | 9529 ms | 368339 | **0** | 368339 |
| 6 MB | **29758 ms** | 5189632 (103.79 s) | 4727033 | 458752 |

Throughput was 0.404 Mbit/s in all five. At 64 KB and below the relay's contribution
to the queue is *zero* — the whole queue sits back in the client, exactly as direct.

**The 6 MB case is the cautionary one.** With a fully auto-tuned window,
`stop_rtt=24691.8 ms`: the control channel becomes unusable because the client's
`stop` cannot be answered until ~100 s of queued video drains. If Distance ships this
relay, **do not leave the stream window at the quic-go maximum.**

## What happens under backpressure, concretely

Backpressure genuinely reaches the agent — this is not a relay that quietly absorbs.

- `peak_inflight=32768` (one chunk) in every u→c direction of every run ever made;
  `peak_inflight=41` c→u. Stall lines read `in flight 0, peak 32768`.
- Relay stall log, slow reader: `u->c: write blocked 2.62s for 32768 bytes`.
- Agent stalls, direct slow reader: 26 stalls, avg 539.003 ms, worst 644.14 ms.
  Relayed: 22 stalls, avg 535.353 ms, worst 649.946 ms. **Nearly identical** — the
  agent's own `Write` blocks either way.
- Frozen viewer (`-read-rate 500`, 45 s): the agent stopped at frame 248 and stayed
  there, `worst_stall=27.856s`. Relay RSS 13.3 MB, flat. Relay CPU 0.07 s for the
  whole 45 s. The chain client→relay→agent held all the way back.
- SIGSTOP probe (E12b, fresh processes per probe, deterministic): freezing the client
  process mid-stream let the agent push **306983 bytes (1.23 s of video) direct** vs
  **722077 bytes (2.89 s) relayed**, then both plateaued. Relay RSS 14.6 MB while
  both legs were wedged. That ratio is the extra window, visible as absorbed bytes
  rather than inferred from latency.

The load-bearing design choice, from `cmd/relay/splice.go`:

> There is no buffering beyond one fixed-size chunk. Every byte read is written
> before the next read, so the relay's own memory is O(chunk) per direction and the
> destination's flow-control window is the only queue. That is what makes
> backpressure propagate: when the client stops reading, `dst.Write` blocks, this
> loop stops reading from `src`, `src`'s receive window closes, and the agent's
> `Write` blocks. Any buffer added here would break that chain and turn the relay
> into an unbounded sink.

A **dropping encoder does not rescue latency.** With `-drop-on-block 50ms` the agent
discarded frames rather than blocking, and p50 went 8986 ms direct → 18966 ms
relayed anyway (dropped 909 vs 873; the client read exactly 3080222 bytes in both).
The queue still fills; it just drops different frames. Latency is a window problem,
not an encoder problem.

## Fast reader: the splice is nearly free

```
$ ./bin/loadclient -url https://127.0.0.1:53080/wt -pin <agent> -duration 20s -datagrams 25   # F1
$ ./bin/loadclient -url https://127.0.0.1:53090/wt -pin <relay> -duration 20s -datagrams 25   # F2
```

| | F1 direct | F2 relayed |
|---|---|---|
| video | 5037391 B in 20.05 s = 2.010 Mbit/s | 5037391 B in 20.05 s = 2.010 Mbit/s |
| frames | 1203 (gaps 0, ooo 0) | 1203 (gaps 0, ooo 0) |
| frame latency | p50 0.26 / p99 0.51 / max 0.86 ms | p50 0.42 / p99 0.83 / max 3.00 ms |
| first video byte after `start` | 0.28 ms | 0.83 ms |
| control RTT median | 0.33 ms | 0.44 ms |
| `fingerprint-refresh` push arrives | 0.41 ms after connect | 0.56 ms after connect |
| datagrams | 25 sent, 25 echoed | 25 sent, 25 echoed |
| Annex B integrity | `start_code_intact=true saw_sps_pps=true` | same |

Byte counts are **identical**, not merely close. Both report
`resets(mirrored=0 substituted=0)` and `datagrams(too_big=0 dropped=0)`.

Sustained, 300 s at 2 Mbit/s, sampling `/proc/<pid>/stat` and RSS every 15 s:

```
     t   relayRSS  rThr  rFDs   rCPU
    0s    10004kB     8     8     0s
   60s    15080kB     9     8  1.43s
  150s    16428kB     9     8  3.54s
  285s    16340kB     9     8  6.81s
sustained-relayed  bytes=75103947  frames=18000  secs=300.00  mbps=2.003
                   p50=0.4  p99=0.8  max=10.8  gaps=0  ooo=0  end=clean-eof/stopped
--- goroutine WARNING count: 0 ---
```

RSS flat after ~60 s, FDs constant at 8, threads constant at 9, CPU linear at
**6.81 s / 285 s ≈ 2.4% of one core**. An earlier 180 s run measured client CPU
2.51 s direct vs 2.48 s relayed — the relay costs the *client* nothing.

### Bitrate ceiling

Swept 8 → 800 Mbit/s, 20 s each, direct and relayed:

| target | direct | relayed | direct p50 | relayed p50 | relay CPU |
|---|---|---|---|---|---|
| 8 Mbit/s | 8.065 | 8.065 | 0.4 ms | 0.6 ms | 0.56 s / 20 s |
| 50 Mbit/s | 50.159 | 50.155 | 0.9 ms | 1.2 ms | 1.13 s / 20 s |
| 200 Mbit/s | 200.351 | 199.704 | 1.4 ms | 1.8 ms | 3.35 s / 20 s |
| 800 Mbit/s | 792.319 | 790.747 | 2.4 ms | 2.8 ms | 11.05 s / 20 s |

At 50 Mbit/s all four runs delivered exactly 125442425 bytes / 1200 frames / gaps 0.
`peak_inflight` stayed at 32768 throughout — even at 800 Mbit/s the relay never
buffers more than a chunk. The splice adds ~0.4 ms at 400× the design bitrate.

### 8 concurrent sessions

`up=8 down=8 warnings=0`, every session 2.006 Mbit/s, p50 0.3–0.4 ms, gaps 0. Relay
went 8 → 10 threads and 7 → 15 FDs at peak, returning to 7 FDs after. RSS 10.1 → 20.0 MB.

### Over 200 ms of injected RTT

A `delayproxy` split 200 ms symmetrically (client→proxy→relay→proxy→agent):

| | direct | relayed |
|---|---|---|
| goodput | 0.404 Mbit/s | 0.404 Mbit/s |
| `start_rtt` | 202.5 ms | 202.9 ms |
| `stop_rtt` | 202.0 ms | 202.8 ms |
| frame p50 | 8908 ms | 17359 ms |

**The relay adds no measurable RTT to the control path.** The frame-latency doubling
is the same window effect as at RTT 0, unchanged in character by real RTT.

## Cert isolation: the actual question, directly evidenced

Every relayed `loadclient` report shows two different hashes:

```
server cert sha256      494adcc3fbb6239f8e34b3fa847e2dd12947acb7b721deaae63f79582fc83189   <- the RELAY's
fingerprint in control  4c882161ae91d807c6c5dc0b70154506d368c5757aecd1357732aab76864c82b   <- the AGENT's
```

The client pinned only the first. The agent's fingerprint reached the client solely
inside the application-level `fingerprint-refresh` message — i.e. as data the viewer
may ignore, not as something it must trust to connect. Pinning the agent's hash
against the relay fails closed:

```
CRYPTO_ERROR 0x12a (local): certificate fingerprint mismatch:
  got [494adcc3...], want one of [4c882161...]
```

`serverCertificateHashes` skips CA *and* hostname validation, so the relay performs
the same check one hop back with `-upstream-fingerprint`, and refuses to run without
one unless `-insecure-upstream` is given.

## Rejection paths (R1–R8): all fail closed, all before touching the agent

| | case | result |
|---|---|---|
| R1 | unknown path `/wt/nosuchdesktop` | **404**, routing decided before any upstream work |
| R2 | phonebook hit `/wt/desktop-1` | 200, 2.140 Mbit/s, gaps 0 |
| R3 | client pins the agent's cert at the relay | `CRYPTO_ERROR 0x12a (local): certificate fingerprint mismatch` |
| R4 | `Origin: https://evil.example` | **403** before dialling upstream |
| R5 | relay's upstream pin is wrong | **502**; relay logs the mismatch |
| R6 | upstream not running | **502** (`context deadline exceeded`) |
| R7 | no upstream fingerprint, no `-insecure-upstream` | **502** (`relay: upstream has no configured fingerprint`) |
| R8 | client offers `distance/1` | session works, but `negotiated protocol ""` — see limitation below |

R1 and R4 are the ones that matter for the Distance story: **auth and routing are
enforced before a single packet reaches any agent.**

The relay dials upstream *first* and only then calls `Upgrade`
(`cmd/relay/main.go:322-391`), which is what lets an upstream rejection surface as a
real status code (404/502/403) instead of a session that opens and immediately dies.

## What broke

### Limitation: application protocols cannot be negotiated through the relay

`Upgrade` commits the 200 response at
`webtransport-go/upgrader.go:134` before the relay has any opportunity to consult the
upstream about `wt-protocol`. So the relay advertises nothing and R8 reports
`negotiated protocol ""`. Harmless for Distance today — `SPIKE.md` establishes that
the agent's `ApplicationProtocols: ["moq-lite-04"]` is an allow-list and a client
offering nothing still upgrades — but it would block a future protocol handshake. A
fix requires either a two-phase upgrade API or the relay pre-dialling and caching the
upstream's protocol set per route.

### Defect (a): goroutine leak in `SendStream.Write` after session close

**Intermittent, 2 goroutines when it fires, permanent once leaked, does not
accumulate per session.** Four consecutive slow-reader-plus-abort sessions: baseline
8 goroutines; after s1 = 8 (0 warnings); after s2 = 10 (1 warning); after s3 = 10;
after s4 = 10, still 1 warning total. A 300 s clean run and an 8-session concurrent
run both showed 0 warnings; the 64 KB-window relay showed 0 across its whole series.

The relay's own watchdog caught it — `cmd/relay/session.go` waits 5 s after
`wg.Wait()` and dumps filtered stacks:

```
pumpUpstreamUni.func1 (cmd/relay/session.go:280)
  -> copyDir (cmd/relay/splice.go:41)
    -> writeFull (cmd/relay/splice.go:85)
      -> webtransport.(*SendStream).Write (stream.go:82)
        -> handleSessionGoneError (stream.go:112)   <- blocked forever
```

Root cause, read from `webtransport-go@v0.10.2-okdaichi.1`:

1. `SendStream.Write` (`stream.go:75-85`) calls `s.onClose()` at **line 78**, which is
   `func() { s.streams.RemoveStream(qstr.StreamID()) }` (`session.go:184`).
2. It *then* calls `handleSessionGoneError()` at **line 82**, which blocks at
   **`stream.go:112`** waiting on `s.closed`.
3. `s.closed` is closed only by `closeWithSession` (`stream.go:183-189`), reachable
   only from `streamsMap.CloseSession` (`streams_map.go:34-42`) iterating `s.m` —
   **the map the stream was just removed from at step 1.**
4. With no write deadline set, `timerCh` is nil, so the wait is unbounded.

`ReceiveStream.Read` → `handleSessionGoneError` (`stream.go:278-323`) has the
identical shape: `s.onClose()` at line 281, then the unbounded wait at line 316.

**Avoidable relay-side.** `SetWriteDeadline` (`stream.go:238-250`) pokes
`deadlineNotifyCh`, and `handleSessionGoneError` re-reads the deadline on every loop
iteration, so setting a deadline before the final write bounds the wait. The same
applies to `SetReadDeadline` (`stream.go:345-357`) for the read side. A production
relay should set both unconditionally; this spike deliberately did not, so the defect
would be observable rather than papered over.

### Defect (b): a client's application close code is lost before the relay sees it

Repeated 6× relayed + 6× direct, fast reader, abort 3 s into a 20 s session:

- **6/6 relayed: the relay observed `client leg ended (session-closed)`** — the close
  is not missed.
- **6/6 mirrored `code=256` upstream** — not the client's 7, and not 0. The agent
  logged `Application error 0x100 (remote)`.
- Session lifetimes `3.003s 8.028s 3.003s 3.003s 8.028s 3.004s` — **4 of 6 torn down
  in ~3 ms, 2 of 6 waited out the (6 s) idle timeout.**
- **Direct controls delivered `loadclient abort` (code 7 + message)** to the agent.

Mechanism: the client's `CloseWithError(7, …)` writes the `WT_CLOSE_SESSION` capsule
under a **10 ms deadline** (`session.go:413`), then cancels the CONNECT stream and the
process exits, tearing down its QUIC connection with application error 0x100. The
relay therefore classifies the end via the `*quic.ApplicationError` branch of
`internal/wterr/classify.go` (→ 256) rather than the `*webtransport.SessionError`
branch (→ 7). **The relay is faithfully mirroring what it received**; the code was
lost one hop earlier, in that 10 ms race. Direct works because the agent's local
capsule read wins the same race.

This is a *fidelity* limitation, not a correctness bug in the splice, and the relay
cannot fix it: the information never arrives. If Distance needs reliable close reasons
it should carry them in the application JSON, not in the session code. A clean
`stop`-then-close is unaffected — `end=cancelled/skipped-stop` is clean both ways,
with the agent logging `control: client closed write side (clean EOF)`.

### Teardown detection latency: mostly not the relay's fault

When the **agent** vanishes, how fast does the viewer find out? Measured both ways,
with the relay's `-max-idle` varied to prove what the timer actually is:

| case | relay `-max-idle` | client unblocked after | client's verdict |
|---|---|---|---|
| K1 agent SIGTERM, relayed | 8 s | 30 s | `read-timeout` |
| K2 agent SIGKILL, relayed | 8 s | 8 s | `idle-timeout` |
| K3 agent SIGKILL, relayed | 25 s | 25 s | `idle-timeout` |
| D1 agent SIGTERM, **direct** | — | **30 s** | `read-timeout` |
| D2 agent SIGKILL, **direct** | — | **30 s** | `read-timeout` |

K2/K3 show detection latency tracks `-max-idle` exactly, so for a hard kill the relay
is the *faster* path: it notices at its own idle timeout and closes the client
session, where a direct client waits out its own 30 s read deadline.

D1/D2 are the important controls. **A direct client stalls the same 30 s**, so the
stall is not a splice cost — it is the read-side twin of defect (a):
`ReceiveStream.Read` → `handleSessionGoneError` blocking at `stream.go:316` after the
stream was already removed from the map at `stream.go:281`. In K1 the relay observed
the upstream end in 5.995 s and closed the client session immediately — the client
even printed the relay's message,
`stop: relay: peer leg ended without a session close capsule` — and *still* sat in
`Read` for 30 s. The relay did its job; the client library did not unblock.

Killing the **relay** mid-session is symmetric: the client sees
`session-closed:0` promptly when the graceful close flushes, or waits out its idle
timeout when it does not — the same 10 ms capsule race as defect (b), seen from the
other side. The agent always learns, at worst at its own idle timeout
(`stalls=1 worst_stall=29.825s` then `session down`). No orphaned upstream sessions
were observed in any teardown case.

Graceful agent shutdown direct (5/5 reps) reported `video=session-closed:0` — the
close code propagates when the process lingers long enough to flush it.

## Residual risks

1. **Window sizing is now a deployment concern.** Leaving the stream window at the
   quic-go maximum (6 MB) produced a 103 s video queue and a 24.7 s `stop` RTT. The
   relay must pin a small stream window (64 KB measured good; 16 KB slightly better
   for latency but doubles write syscalls and raises stop RTT to ~15 ms). This is a
   config value someone has to *keep* right, and nothing in the library stops it
   drifting back to auto-tuned.
2. **The `handleSessionGoneError` leak.** Fixable relay-side with deadlines before
   final reads/writes, but it is a library defect and an unpatched relay leaks 2
   goroutines per affected session, permanently. At 1-in-4 sessions and no upper
   bound, a long-lived relay needs the fix, not just monitoring.
3. **Close-code fidelity is unachievable through a splice** for hard aborts (defect
   b). Design around it: carry reasons in application messages.
4. **Untested at scale and on real networks.** 8 concurrent sessions, localhost, no
   loss, one machine, a fake agent. Real loss and reordering interact with two
   independent congestion controllers in ways loopback cannot show. The 200 ms RTT
   injection is a partial mitigation, not a substitute.
5. **No application-protocol negotiation** through the relay (`upgrader.go:134`).
6. **Datagram queue depth is small and blocking.** `maxDatagramSendQueueLen = 32` in
   quic-go; the relay never buffers them, so a burst wider than 32 while the socket
   is busy will block a pump rather than drop. All measured runs showed
   `too_big=0 dropped=0` with 20–60 datagrams, including while the video splice was
   fully stalled, so this was never hit — but Distance's real datagram rate is
   unknown to this spike.

## Recommendation

Ship the termination model. It answers the question it was built to answer: the
browser trusts one cert, the agent's fingerprint never becomes something a user must
handle, auth and routing happen before any packet reaches an agent, and the viewer
needs no changes. Goodput is free and control latency is free.

Two things must be done for real use, both cheap:

1. **Pin a small stream receive window** — both the initial and the maximum:
   `-stream-window 65536 -max-stream-window 65536 -conn-window 131072 -max-conn-window 131072`.
   Non-negotiable — this is the difference between "indistinguishable from direct" and
   "unusable control channel", and setting only the initial window lets quic-go
   auto-tune back up.
2. **Set a write deadline before final writes and a read deadline before final reads**
   in the splice, to bound `handleSessionGoneError`. Or patch the library: moving
   `s.onClose()` to *after* `handleSessionGoneError()` in both `stream.go:75-85` and
   `stream.go:278-288` would fix it upstream, since the stream would still be in the
   map when `CloseSession` iterates.

Everything else on the risk list is either a known-and-bounded fidelity limit or work
that belongs in a staging environment rather than a spike.

## Reproducing

Ports: fakeagent 53080/53085–53087, relay 53090–53098. Nothing below 53000 was
touched; the live agent on 52020/52022 and the viewer on 8792 were never contacted.

```
go build -o bin/fakeagent ./cmd/fakeagent
go build -o bin/relay     ./cmd/relay
go build -o bin/loadclient ./cmd/loadclient

# agent, prints its cert SHA-256
./bin/fakeagent -listen 127.0.0.1:53080

# relay, terminating with its own cert, pinning the agent one hop back
./bin/relay -listen 127.0.0.1:53090 \
  -route "/wt=https://127.0.0.1:53080/wt,fingerprint=$AGENT_FP" \
  -stream-window 65536 -max-stream-window 65536 \
  -conn-window 131072 -max-conn-window 131072 -stats 10s

# the measurement that matters: same client, direct then relayed, deliberately slow
./bin/loadclient -url https://127.0.0.1:53080/wt -pin $AGENT_FP -read-rate 125000 -duration 30s
./bin/loadclient -url https://127.0.0.1:53090/wt -pin $RELAY_FP -read-rate 125000 -duration 30s
```

The 32 experiment scripts are committed under `exp/`. They write their output to
`$OUT`, which defaults to a job-scratch directory that is *not* durable — set `OUT`
to somewhere real before re-running. The raw output from this session lived there and
is gone with the job; everything load-bearing is transcribed above.

## Choices made unattended

- **Added a `-max-idle` flag** to `cmd/relay` mid-experiment. The idle timeout is
  itself a measured quantity (it is what tore down the sessions in K2/K3 and defect
  (b)'s slow cases), and making it configurable turned a 35 s experiment into an 8 s
  one. Default unchanged at 30 s.
- **64 KB windows are the recommendation** rather than 16 KB, even though 16 KB
  measured marginally better on latency (9529 ms vs 10298 ms at 50 kB/s). 64 KB
  reaches direct-equivalent latency with half the write syscalls (4683 vs 8323 writes
  at 2 Mbit/s) and a ~15× lower stop RTT.
- **The spike deliberately does not set write/read deadlines** in the splice, so
  defect (a) stays observable. A shipping relay should set them.
- **`fakeagent` frames are synthetic**, not a real encoder: correct Annex B start
  codes and plausible SPS/PPS/IDR/P structure, sized to hit the target bitrate, with
  a 12-byte stamp so latency is measured rather than modelled. Nothing here validates
  real H.264 payloads survive — only that byte streams and frame boundaries do.

---

## Preservation note (added by Hermet, 2026-09-02)

This directory is a copy of the Claude Code worktree
(`distance-relay/.claude/worktrees/relay-spike`) taken after the run finished,
with `bin/` and `.git` stripped. The worktree itself is not durable — the job
scratch directory holding the raw experiment output is already gone, exactly as
the "Reproducing" section warns. Everything load-bearing is transcribed in this
file; the 34 scripts under `exp/` are intact but need `OUT` pointed somewhere real.

Rebuild with:

```sh
go build -o bin/fakeagent  ./cmd/fakeagent
go build -o bin/relay      ./cmd/relay
go build -o bin/loadclient ./cmd/loadclient
```
