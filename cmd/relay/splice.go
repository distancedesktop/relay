package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/wterr"
)

// copyDir moves bytes from src to dst until one end stops, and reports what the
// end meant so the caller can mirror it.
//
// Two deliberate non-choices:
//
//   - There is no buffering beyond one fixed-size chunk. Every byte read is written
//     before the next read, so the relay's own memory is O(chunk) per direction and
//     the destination's flow-control window is the only queue. That is what makes
//     backpressure propagate: when the client stops reading, dst.Write blocks, this
//     loop stops reading from src, src's receive window closes, and the agent's
//     Write blocks. Any buffer added here would break that chain and turn the relay
//     into an unbounded sink.
//   - io.Copy is not used, even though the ground truth says a plain io.Copy is
//     correct for framing. io.Copy would give no per-write timing, and the timing is
//     the measurement the spike exists to produce.
func copyDir(w streamWriter, r streamReader, st *dirStats, chunkSize int, stallThreshold time.Duration,
	log func(string, ...any)) wterr.Classified {

	buf := make([]byte, chunkSize)
	for {
		readStart := time.Now()
		n, rerr := r.Read(buf)
		readBlocked := time.Since(readStart)

		if n > 0 {
			st.recordRead(n, readBlocked)
			writeStart := time.Now()
			wn, werr := writeFull(w, buf[:n])
			writeBlocked := time.Since(writeStart)
			st.recordWrite(wn, writeBlocked, stallThreshold)
			st.releaseInFlight(n)

			if writeBlocked > stallThreshold {
				log("%s: write blocked %s for %d bytes (in flight %d, peak %d)",
					st.name, writeBlocked.Round(time.Millisecond), wn,
					st.inFlight.Load(), st.peakFlight.Load())
			}

			if werr != nil {
				c := wterr.Classify(werr)
				// A write failure is about the destination, not the source. Report it
				// as such so the caller cancels the correct half.
				c.Err = &writeSideError{err: werr}
				return c
			}
		}

		if rerr != nil {
			return wterr.Classify(rerr)
		}
	}
}

// writeSideError marks a Classified as having come from the write half, so the
// caller knows which direction to reset.
type writeSideError struct{ err error }

func (e *writeSideError) Error() string { return "write side: " + e.err.Error() }
func (e *writeSideError) Unwrap() error { return e.err }

func isWriteSide(c wterr.Classified) bool {
	var w *writeSideError
	return errors.As(c.Err, &w)
}

// writeFull loops until all of b is written or an error occurs. A WebTransport
// Write can return a short count with a nil error only on a deadline, but looping
// costs nothing and removes a class of silent truncation.
func writeFull(w streamWriter, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := w.Write(b[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

type streamReader interface {
	Read([]byte) (int, error)
}

type streamWriter interface {
	Write([]byte) (int, error)
}

// mirrorEnd applies the meaning of a finished copy direction to the destination
// stream, and to the source when the destination was the one that failed.
//
// The mapping:
//
//	clean EOF          -> Close() the destination's send half (FIN)
//	stream reset       -> CancelWrite/CancelRead with the same WebTransport code
//	unmappable reset   -> same, with wterr.SubstituteResetCode, and log it loudly
//	session closed     -> handled one level up; the whole session is going away
func mirrorEnd(c wterr.Classified, dstSend sendCanceller, srcRecv recvCanceller, stats *sessionStats,
	label string, log func(string, ...any)) {

	switch c.Kind {
	case wterr.KindEOF:
		if dstSend != nil {
			if err := dstSend.Close(); err != nil {
				log("%s: mirroring EOF via Close failed: %v", label, err)
			}
		}

	case wterr.KindStreamReset, wterr.KindStreamResetUnmappable:
		code, exact := wterr.ResetCodeFor(c)
		if exact {
			stats.ResetsMirrored.Add(1)
		} else {
			stats.ResetsSubstituted.Add(1)
			log("%s: LOSSY: upstream reset carried a code we cannot represent (%v); "+
				"substituting %d. See webtransport-go stream.go:460-463.", label, c.Err, code)
		}
		if isWriteSide(c) {
			// The destination refused the data: tell the source to stop sending.
			if srcRecv != nil {
				srcRecv.CancelRead(code)
			}
		} else {
			// The source reset: propagate onto the destination's send half.
			if dstSend != nil {
				dstSend.CancelWrite(code)
			}
		}

	case wterr.KindSessionClosed:
		// Nothing to do per-stream: closing the peer session resets its streams.

	case wterr.KindTimeout:
		log("%s: copy ended on a deadline, which the relay never sets on a data path: %v", label, c.Err)

	case wterr.KindIdleTimeout:
		// The whole leg is gone, not one stream. Per-stream cancellation would be
		// pointless — there is no connection left to carry a RESET_STREAM — so this is
		// left to the session-level teardown in spliced.run, which is watching the same
		// event via the session context.
		log("%s: leg died of a QUIC idle timeout; leaving teardown to the session watcher: %v", label, c.Err)

	default:
		log("%s: copy ended: kind=%s err=%v", label, c.Kind, c.Err)
		stats.noteErr(c.Err)
		// An unclassifiable failure still has to be signalled, or the peer waits
		// forever for bytes that will never come.
		if dstSend != nil {
			dstSend.CancelWrite(wterr.SubstituteResetCode)
		}
	}
}

type sendCanceller interface {
	Close() error
	CancelWrite(webtransport.StreamErrorCode)
}

type recvCanceller interface {
	CancelRead(webtransport.StreamErrorCode)
}

// mirrorSessionClose propagates a session-level close from one leg to the other.
//
// Session error codes are a flat uint32 on both sides with no greasing, so unlike
// stream codes they round-trip exactly. The message does too, up to the library's
// 1024-byte cap (session.go:23, 402-404).
func mirrorSessionClose(dst *webtransport.Session, c wterr.Classified, label string, log func(string, ...any)) {
	code := c.SessionCode
	msg := c.SessionMessage
	if c.Kind != wterr.KindSessionClosed {
		// The leg died without a WT_CLOSE_SESSION capsule: a QUIC-level failure, a
		// dropped capsule (session.go:413-416 writes it under a 10ms deadline and
		// resets the CONNECT stream on failure), or an idle timeout. There is no
		// faithful code to forward, so say so rather than inventing one.
		code = 0
		msg = "relay: peer leg ended without a session close capsule"
		log("%s: no session code to mirror (kind=%s err=%v); closing peer with 0", label, c.Kind, c.Err)
	} else {
		log("%s: mirroring session close code=%d msg=%q remote=%v", label, code, truncate(msg, 120), c.Remote)
	}
	// CloseWithError blocks until the session context is done (session.go:420), so
	// never call it on the path that must stay responsive.
	if err := dst.CloseWithError(code, msg); err != nil {
		log("%s: CloseWithError: %v", label, err)
	}
}

// sessionEndCause turns a finished session into something classifiable. The
// library's closeErr is unexported and Session.Context() is a plain WithCancel
// child (session.go:112), so a context cause is always context.Canceled and
// carries nothing. The only way to learn why a session ended is to observe the
// error a stream operation returns, which the copy loops already do — this is the
// fallback for the case where no stream was open.
func sessionEndCause(sess *webtransport.Session) wterr.Classified {
	select {
	case <-sess.Context().Done():
	default:
		return wterr.Classified{Kind: wterr.KindOK}
	}
	// AcceptStream on a closed session returns the session's closeErr verbatim
	// (session.go:241-243), which is the one accessor for it that exists.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := sess.AcceptStream(ctx)
	if err == nil {
		return wterr.Classified{Kind: wterr.KindOK}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wterr.Classified{Kind: wterr.KindOther, Err: err}
	}
	return wterr.Classify(err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
