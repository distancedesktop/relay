// Package wterr reimplements webtransport-go's WebTransport <-> HTTP/3 stream
// error-code mapping, which the library keeps unexported, and gives the relay the
// helpers it needs to move a peer's error code across a session boundary without
// losing it.
//
// Ground truth: github.com/okdaichi/webtransport-go@v0.10.2-okdaichi.1/errors.go.
// Lines 16-34 of that file are the whole mapping:
//
//	const (
//		firstErrorCode = 0x52e4a40fa8db
//		lastErrorCode  = 0x52e5ac983162
//	)
//
//	func webtransportCodeToHTTPCode(n StreamErrorCode) quic.StreamErrorCode {
//		return quic.StreamErrorCode(firstErrorCode) + quic.StreamErrorCode(n) + quic.StreamErrorCode(n/0x1e)
//	}
//
//	func httpCodeToWebtransportCode(h quic.StreamErrorCode) (StreamErrorCode, error) {
//		if h < firstErrorCode || h > lastErrorCode {
//			return 0, errors.New("error code outside of expected range")
//		}
//		if (h-0x21)%0x1f == 0 {
//			return 0, errors.New("invalid error code")
//		}
//		shifted := h - firstErrorCode
//		return StreamErrorCode(shifted - shifted/0x1f), nil
//	}
//
// Note the asymmetry the SPIKE.md summary elides: the forward direction divides by
// 0x1e, the reverse by 0x1f. Both are correct — forward inserts one greased hole
// every 0x1e codes, so the reverse must divide by the resulting stride of 0x1f.
// Getting this backwards silently corrupts one code in every 30.
//
// Why the relay needs this at all: the library converts codes for us on the way
// in (stream.go:454 maybeConvertStreamError) and on the way out (stream.go:330,
// stream.go:179 CancelRead/CancelWrite call webtransportCodeToHTTPCode). So in the
// happy path the relay handles only *webtransport.StreamError values and never
// touches the wire encoding. The mapping is still needed because a StreamError is
// not the only thing Read/Write can return, and because the library gives up when
// conversion fails: stream.go:462 turns an out-of-range reset into an opaque
// fmt.Errorf, dropping the code. This package lets the relay recognise that case
// and pick a deliberate substitute rather than inventing a random code.
package wterr

import (
	"errors"
	"fmt"

	"github.com/quic-go/quic-go"

	"github.com/okdaichi/webtransport-go"
)

// FirstCode and LastCode bound the HTTP/3 stream error codes reserved for
// WebTransport. Values verified against errors.go:17-18.
const (
	FirstCode quic.StreamErrorCode = 0x52e4a40fa8db
	LastCode  quic.StreamErrorCode = 0x52e5ac983162
)

// ErrOutOfRange is returned when an HTTP/3 stream error code is not in the
// WebTransport range at all — a genuine HTTP/3-level reset, not a WT one.
var ErrOutOfRange = errors.New("error code outside of expected range")

// ErrGreased is returned for the reserved holes in the mapping. The HTTP/3 spec
// reserves 0x1f * N + 0x21 for greasing, so those codes carry no WT meaning.
var ErrGreased = errors.New("invalid error code")

// ToHTTP maps a WebTransport stream error code to its HTTP/3 stream error code.
// Mirrors webtransportCodeToHTTPCode (errors.go:21).
func ToHTTP(n webtransport.StreamErrorCode) quic.StreamErrorCode {
	return FirstCode + quic.StreamErrorCode(n) + quic.StreamErrorCode(n/0x1e)
}

// FromHTTP maps an HTTP/3 stream error code back to a WebTransport stream error
// code. Mirrors httpCodeToWebtransportCode (errors.go:25). Returns ErrOutOfRange
// for codes outside [FirstCode, LastCode] and ErrGreased for the reserved holes.
func FromHTTP(h quic.StreamErrorCode) (webtransport.StreamErrorCode, error) {
	if h < FirstCode || h > LastCode {
		return 0, fmt.Errorf("%w: %#x", ErrOutOfRange, uint64(h))
	}
	if (h-0x21)%0x1f == 0 {
		return 0, fmt.Errorf("%w: %#x", ErrGreased, uint64(h))
	}
	shifted := h - FirstCode
	return webtransport.StreamErrorCode(shifted - shifted/0x1f), nil
}
