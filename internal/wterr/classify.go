package wterr

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/quic-go/quic-go"

	"github.com/okdaichi/webtransport-go"
)

// Kind classifies what a Read or Write on a WebTransport stream actually meant,
// so the relay can decide what to do to the peer stream on the other session.
type Kind int

const (
	// KindOK is a non-error.
	KindOK Kind = iota
	// KindEOF is a clean FIN: the sender closed its write side. Mirror by calling
	// Close() on the corresponding send stream.
	KindEOF
	// KindStreamReset is a peer RESET_STREAM / STOP_SENDING with a recoverable
	// WebTransport application code. Mirror with CancelWrite/CancelRead(Code).
	KindStreamReset
	// KindSessionClosed means the whole session went away. Mirror by closing the
	// other session with the same code and message.
	KindSessionClosed
	// KindStreamResetUnmappable is a stream reset whose HTTP/3 code the library
	// could not map back to a WebTransport code (greased, or outside the reserved
	// range). See stream.go:460-463: the code is discarded and only a text error
	// survives, so the relay must substitute a code of its own.
	KindStreamResetUnmappable
	// KindTimeout is a deadline expiry from SetReadDeadline/SetWriteDeadline. Not
	// a peer event; the relay uses deadlines for backpressure probing and must not
	// treat one as a stream failure.
	KindTimeout
	// KindIdleTimeout is a QUIC connection idle timeout: the peer stopped sending
	// packets for MaxIdleTimeout and the whole connection is gone. It reports
	// Timeout() == true exactly like a local deadline, which is why it needs its own
	// kind — the two mean opposite things. A local deadline is the relay's own
	// instrumentation and must be ignored; an idle timeout means every stream on
	// that leg is dead and the peer leg must be torn down.
	KindIdleTimeout
	// KindOther is anything else: local misuse, transport error, closed conn.
	KindOther
)

func (k Kind) String() string {
	switch k {
	case KindOK:
		return "ok"
	case KindEOF:
		return "eof"
	case KindStreamReset:
		return "stream-reset"
	case KindSessionClosed:
		return "session-closed"
	case KindStreamResetUnmappable:
		return "stream-reset-unmappable"
	case KindTimeout:
		return "timeout"
	case KindIdleTimeout:
		return "idle-timeout"
	default:
		return "other"
	}
}

// Classified is the decomposition of an error into something the relay can act on.
type Classified struct {
	Kind Kind
	// StreamCode is set when Kind is KindStreamReset.
	StreamCode webtransport.StreamErrorCode
	// SessionCode and SessionMessage are set when Kind is KindSessionClosed.
	SessionCode    webtransport.SessionErrorCode
	SessionMessage string
	// Remote reports whether the peer caused this, as opposed to a local call.
	// Only meaningful for KindStreamReset and KindSessionClosed.
	Remote bool
	// Err is the original error, kept for logging.
	Err error
}

// SubstituteResetCode is what the relay sends onward when the upstream reset
// carried a code that cannot be represented as a WebTransport code. It is an
// arbitrary but documented choice: 0 is the most neutral "the stream ended
// abnormally" signal available, and reusing a real code would be a lie.
const SubstituteResetCode webtransport.StreamErrorCode = 0

// Classify decomposes an error from Read, Write, Close, AcceptStream, or
// AcceptUniStream.
//
// Ordering matters. A *webtransport.SessionError is checked before
// *webtransport.StreamError because when a session dies the library resets each
// of its streams with WTSessionGoneErrorCode and then substitutes the session
// error (stream.go:81-83, stream.go:284-286 -> handleSessionGoneError), so the
// session error is the more specific fact.
func Classify(err error) Classified {
	if err == nil {
		return Classified{Kind: KindOK}
	}
	if errors.Is(err, io.EOF) {
		return Classified{Kind: KindEOF, Err: err}
	}

	var sessErr *webtransport.SessionError
	if errors.As(err, &sessErr) {
		return Classified{
			Kind:           KindSessionClosed,
			SessionCode:    sessErr.ErrorCode,
			SessionMessage: sessErr.Message,
			Remote:         sessErr.Remote,
			Err:            err,
		}
	}

	var strErr *webtransport.StreamError
	if errors.As(err, &strErr) {
		return Classified{
			Kind:       KindStreamReset,
			StreamCode: strErr.ErrorCode,
			Remote:     strErr.Remote,
			Err:        err,
		}
	}

	// A QUIC idle or handshake timeout is checked before the generic Timeout() test
	// below, because it satisfies that test while meaning the opposite thing: the
	// whole connection is gone, not one operation's deadline. errors.As, not
	// errors.Is: both are empty structs (qerr/errors.go:86,95) with no Is method, so
	// comparing a fresh &IdleTimeoutError{} by pointer equality is not reliable for
	// a zero-size type.
	var idle *quic.IdleTimeoutError
	if errors.As(err, &idle) {
		return Classified{Kind: KindIdleTimeout, Err: err}
	}
	var hsTimeout *quic.HandshakeTimeoutError
	if errors.As(err, &hsTimeout) {
		return Classified{Kind: KindIdleTimeout, Err: err}
	}

	if errors.Is(err, os.ErrDeadlineExceeded) {
		return Classified{Kind: KindTimeout, Err: err}
	}
	var nerr interface{ Timeout() bool }
	if errors.As(err, &nerr) && nerr.Timeout() {
		return Classified{Kind: KindTimeout, Err: err}
	}

	// A raw *quic.StreamError can still surface: the WebTransport wrappers convert
	// it, but a caller holding a stream after a session teardown, or an HTTP/3-level
	// reset on the CONNECT stream, can leak one through.
	var qErr *quic.StreamError
	if errors.As(err, &qErr) {
		if code, cerr := FromHTTP(qErr.ErrorCode); cerr == nil {
			return Classified{Kind: KindStreamReset, StreamCode: code, Remote: qErr.Remote, Err: err}
		}
		return Classified{Kind: KindStreamResetUnmappable, Remote: qErr.Remote, Err: err}
	}

	// stream.go:462 formats unmappable resets as
	//   "stream reset, but failed to convert stream error %d: %w"
	// with no typed error to match on, so string matching is the only way to
	// recognise it. Verified against the literal in
	// webtransport-go@v0.10.2-okdaichi.1/stream.go:462.
	if strings.Contains(err.Error(), "stream reset, but failed to convert stream error") {
		return Classified{Kind: KindStreamResetUnmappable, Remote: true, Err: err}
	}

	var appErr *quic.ApplicationError
	if errors.As(err, &appErr) {
		return Classified{
			Kind:           KindSessionClosed,
			SessionCode:    webtransport.SessionErrorCode(appErr.ErrorCode),
			SessionMessage: appErr.ErrorMessage,
			Remote:         appErr.Remote,
			Err:            err,
		}
	}

	return Classified{Kind: KindOther, Err: err}
}

// ResetCodeFor returns the WebTransport stream code the relay should use when
// mirroring c onto a peer stream, plus whether a code was actually recovered.
// For an unmappable reset it returns SubstituteResetCode and false, which the
// caller should log: it is the one place where the splice is provably lossy.
func ResetCodeFor(c Classified) (webtransport.StreamErrorCode, bool) {
	switch c.Kind {
	case KindStreamReset:
		return c.StreamCode, true
	case KindStreamResetUnmappable:
		return SubstituteResetCode, false
	case KindSessionClosed:
		// The library will reset the streams itself when we close the session, but
		// if a caller mirrors this onto a single stream, WT_SESSION_GONE's own WT
		// code is not representable (it is an HTTP/3-level code), so substitute.
		return SubstituteResetCode, false
	default:
		return SubstituteResetCode, false
	}
}
