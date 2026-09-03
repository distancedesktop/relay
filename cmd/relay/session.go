package main

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/wterr"
)

// spliced is one client session joined to one upstream session.
type spliced struct {
	id       int64
	client   *webtransport.Session
	upstream *webtransport.Session
	cfg      *relayConfig
	stats    *sessionStats
	logf     func(string, ...any)

	wg sync.WaitGroup
	// live counts goroutines the splice started and that have not returned. A
	// non-zero value after teardown is a per-session goroutine leak, which under
	// churn is the difference between a relay that runs for a week and one that
	// does not, so it is counted rather than guessed at.
	live atomic.Int64
}

// goSpliced runs fn as a tracked splice goroutine.
func (s *spliced) goSpliced(fn func()) {
	s.wg.Add(1)
	s.live.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.live.Add(-1)
		fn()
	}()
}

// run splices the two sessions until either ends, then mirrors the end onto the
// other. It returns when both legs are done.
//
// ctx is the relay's own lifetime, deliberately not the request context. The
// HTTP/3 server cancels the request context as soon as the CONNECT stream dies
// (http3/server_conn.go:194) — which is the very same event that ends the client
// WebTransport session, since the CONNECT stream *is* the session. Watching it here
// would race the session context and make an ordinary client close look like a
// relay shutdown, discarding the code the client sent.
func (s *spliced) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Six pumps. Each accept loop owns its own goroutine because AcceptStream
	// blocks, and there is no way to poll: the library offers no non-blocking
	// accept and no way to peek at a stream without owning it.
	pumps := []struct {
		name string
		fn   func(context.Context)
	}{
		{"client-bidi", s.pumpClientBidi},
		{"client-uni", s.pumpClientUni},
		{"upstream-bidi", s.pumpUpstreamBidi},
		{"upstream-uni", s.pumpUpstreamUni},
		{"datagrams-up", s.pumpDatagramsUp},
		{"datagrams-down", s.pumpDatagramsDown},
	}
	for _, p := range pumps {
		p := p
		s.goSpliced(func() { p.fn(ctx) })
	}

	// Watch both session contexts. Whichever dies first, mirror its ending onto the
	// other and cancel every pump.
	select {
	case <-s.client.Context().Done():
		s.teardown(cancel, true)
	case <-s.upstream.Context().Done():
		s.teardown(cancel, false)
	case <-ctx.Done():
		// Relay shutdown — but only if a leg has not in fact just died. A session
		// close and a shutdown are indistinguishable by timing alone, and mistaking
		// the former for the latter loses the peer's code, so re-check before
		// deciding.
		switch {
		case s.client.Context().Err() != nil:
			s.teardown(cancel, true)
		case s.upstream.Context().Err() != nil:
			s.teardown(cancel, false)
		default:
			s.logf("relay shutting down; closing both legs")
			// CloseWithError blocks until the session context is done
			// (session.go:420), so neither call may be on the critical path.
			go s.upstream.CloseWithError(0, "relay shutting down")
			go s.client.CloseWithError(0, "relay shutting down")
		}
	}

	// The pumps unblock once the sessions are closed: every blocking call in the
	// library returns the session's closeErr on close (session.go:252-253, 276-277)
	// and ReceiveDatagram returns once its context is cancelled.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Worth reporting rather than hiding: a pump that will not exit is a leak,
		// and in a real relay it would be a per-session goroutine leak under churn.
		// The stack is dumped because "1 goroutine is stuck" is not actionable and
		// "stuck in ReceiveDatagram" is.
		s.logf("WARNING: %d splice goroutines did not exit within 5s after teardown:\n%s",
			s.live.Load(), stuckStacks())
	}
}

// stuckStacks returns the stacks of goroutines still inside relay code, which is
// the only part of a full dump that identifies which pump is wedged.
//
// The filter matches source file paths, not symbol names: the relay is package
// main, so its frames appear as "main.(*spliced).pumpDatagramsUp" with no module
// path anywhere in them, and a filter on the import path silently matches nothing.
func stuckStacks() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var keep []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "/cmd/relay/") {
			keep = append(keep, g)
		}
	}
	if len(keep) == 0 {
		return "  (no goroutine has a relay frame; it exited between the timeout and this dump)"
	}
	return strings.Join(keep, "\n\n")
}

// teardown mirrors the ending of the leg that died onto the one that has not.
func (s *spliced) teardown(cancel context.CancelFunc, clientDied bool) {
	cancel()
	dead, alive, label := s.client, s.upstream, "client->upstream"
	if !clientDied {
		dead, alive, label = s.upstream, s.client, "upstream->client"
	}
	c := sessionEndCause(dead)
	s.logf("%s leg ended (%s); tearing down the other", strings.SplitN(label, "->", 2)[0], c.Kind)
	mirrorSessionClose(alive, c, label, s.logf)
}

// pumpClientBidi mirrors client-initiated bidirectional streams upstream. This is
// the control stream in the Distance protocol, but the relay does not know or care:
// it is a byte pipe in both directions.
func (s *spliced) pumpClientBidi(ctx context.Context) {
	for {
		down, err := s.client.AcceptStream(ctx)
		if err != nil {
			s.logAcceptEnd("client bidi accept", err, ctx)
			return
		}
		up, err := s.upstream.OpenStreamSync(ctx)
		if err != nil {
			// No upstream stream credit, or the upstream is gone. Refuse the client's
			// stream with a code rather than leaving it hanging.
			c := wterr.Classify(err)
			s.logf("client bidi: cannot open upstream stream: kind=%s err=%v", c.Kind, err)
			down.CancelRead(wterr.SubstituteResetCode)
			down.CancelWrite(wterr.SubstituteResetCode)
			if c.Kind == wterr.KindSessionClosed {
				return
			}
			continue
		}
		s.stats.StreamsBidiUp.Add(1)
		s.goSpliced(func() { s.spliceBidi(down, up, "client->upstream") })
	}
}

// pumpUpstreamBidi mirrors upstream-initiated bidirectional streams down to the
// client. The Distance agent does not open these, but a relay that only handled one
// direction would be a relay that breaks the moment the protocol grows.
func (s *spliced) pumpUpstreamBidi(ctx context.Context) {
	for {
		up, err := s.upstream.AcceptStream(ctx)
		if err != nil {
			s.logAcceptEnd("upstream bidi accept", err, ctx)
			return
		}
		down, err := s.client.OpenStreamSync(ctx)
		if err != nil {
			c := wterr.Classify(err)
			s.logf("upstream bidi: cannot open client stream: kind=%s err=%v", c.Kind, err)
			up.CancelRead(wterr.SubstituteResetCode)
			up.CancelWrite(wterr.SubstituteResetCode)
			if c.Kind == wterr.KindSessionClosed {
				return
			}
			continue
		}
		s.stats.StreamsBidiDown.Add(1)
		s.goSpliced(func() { s.spliceBidi(up, down, "upstream->client") })
	}
}

// spliceBidi joins two bidirectional streams. `a` is the stream the peer opened,
// `b` the one the relay opened toward the other side.
func (s *spliced) spliceBidi(a, b *webtransport.Stream, label string) {
	var wg sync.WaitGroup
	wg.Add(2)

	// a -> b
	go func() {
		defer wg.Done()
		st := s.dirFor(label, true)
		c := copyDir(b, a, st, s.cfg.chunkSize, s.cfg.stallThreshold, s.logf)
		mirrorEnd(c, b, a, s.stats, label+" bidi a->b", s.logf)
	}()

	// b -> a
	go func() {
		defer wg.Done()
		st := s.dirFor(label, false)
		c := copyDir(a, b, st, s.cfg.chunkSize, s.cfg.stallThreshold, s.logf)
		mirrorEnd(c, a, b, s.stats, label+" bidi b->a", s.logf)
	}()

	wg.Wait()
}

// pumpClientUni mirrors client-initiated unidirectional streams upstream.
func (s *spliced) pumpClientUni(ctx context.Context) {
	for {
		down, err := s.client.AcceptUniStream(ctx)
		if err != nil {
			s.logAcceptEnd("client uni accept", err, ctx)
			return
		}
		up, err := s.upstream.OpenUniStreamSync(ctx)
		if err != nil {
			c := wterr.Classify(err)
			s.logf("client uni: cannot open upstream uni stream: kind=%s err=%v", c.Kind, err)
			down.CancelRead(wterr.SubstituteResetCode)
			if c.Kind == wterr.KindSessionClosed {
				return
			}
			continue
		}
		s.stats.StreamsUniUp.Add(1)
		s.goSpliced(func() {
			st := &s.stats.ClientToUpstream
			c := copyDir(up, down, st, s.cfg.chunkSize, s.cfg.stallThreshold, s.logf)
			mirrorEnd(c, up, down, s.stats, "client->upstream uni", s.logf)
		})
	}
}

// pumpUpstreamUni mirrors upstream-initiated unidirectional streams down. This is
// the video path: the agent opens a server-initiated uni stream per `start`.
func (s *spliced) pumpUpstreamUni(ctx context.Context) {
	for {
		up, err := s.upstream.AcceptUniStream(ctx)
		if err != nil {
			s.logAcceptEnd("upstream uni accept", err, ctx)
			return
		}
		down, err := s.client.OpenUniStreamSync(ctx)
		if err != nil {
			c := wterr.Classify(err)
			s.logf("upstream uni: cannot open client uni stream: kind=%s err=%v", c.Kind, err)
			up.CancelRead(wterr.SubstituteResetCode)
			if c.Kind == wterr.KindSessionClosed {
				return
			}
			continue
		}
		s.stats.StreamsUniDown.Add(1)
		s.goSpliced(func() {
			st := &s.stats.UpstreamToClient
			c := copyDir(down, up, st, s.cfg.chunkSize, s.cfg.stallThreshold, s.logf)
			mirrorEnd(c, down, up, s.stats, "upstream->client uni", s.logf)
		})
	}
}

// pumpDatagramsUp forwards client datagrams to the upstream agent.
//
// Unreliability is preserved by never queueing. quic-go already holds a 32-slot
// send queue whose Add blocks when full (datagram_queue.go:13,44-46), so the
// backpressure a relay would otherwise have to invent already exists; adding a
// queue here would only add latency to data whose entire point is to arrive late or
// not at all. A datagram too large for the other leg is dropped and counted: the
// two legs have independent MTU estimates and independent quarter-stream-ID prefix
// lengths, so a size accepted on one is not guaranteed on the other, and no
// accessor exists to ask in advance.
func (s *spliced) pumpDatagramsUp(ctx context.Context) {
	s.pumpDatagrams(ctx, s.client, s.upstream, &s.stats.DatagramsUp, "datagram up")
}

func (s *spliced) pumpDatagramsDown(ctx context.Context) {
	s.pumpDatagrams(ctx, s.upstream, s.client, &s.stats.DatagramsDown, "datagram down")
}

func (s *spliced) pumpDatagrams(ctx context.Context, from, to *webtransport.Session,
	counter interface{ Add(int64) int64 }, label string) {

	for {
		b, err := from.ReceiveDatagram(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c := wterr.Classify(err)
				s.logf("%s: receive ended kind=%s err=%v", label, c.Kind, err)
			}
			return
		}
		if err := to.SendDatagram(b); err != nil {
			if tooLarge(err) {
				s.stats.DatagramsTooBig.Add(1)
				s.logf("%s: dropped a %d-byte datagram: %v", label, len(b), err)
				continue
			}
			c := wterr.Classify(err)
			if c.Kind == wterr.KindSessionClosed || ctx.Err() != nil {
				return
			}
			s.stats.DatagramDropped.Add(1)
			s.logf("%s: send failed (%d bytes): %v", label, len(b), err)
			continue
		}
		counter.Add(1)
	}
}

func (s *spliced) dirFor(label string, aToB bool) *dirStats {
	// "client->upstream" a->b is client to upstream; b->a is the reverse. For an
	// upstream-initiated stream the labels invert.
	clientFirst := label == "client->upstream"
	if clientFirst == aToB {
		return &s.stats.ClientToUpstream
	}
	return &s.stats.UpstreamToClient
}

func (s *spliced) logAcceptEnd(what string, err error, ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c := wterr.Classify(err)
	switch c.Kind {
	case wterr.KindSessionClosed:
		s.logf("%s ended: session closed code=%d msg=%q", what, c.SessionCode, truncate(c.SessionMessage, 120))
	default:
		s.logf("%s ended: kind=%s err=%v", what, c.Kind, err)
	}
}
