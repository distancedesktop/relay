package wterr

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/quic-go/quic-go"

	"github.com/okdaichi/webtransport-go"
)

// TestRoundTripExhaustiveLowCodes checks every code in a dense low band, where an
// off-by-one in the 0x1e/0x1f asymmetry would first show up.
func TestRoundTripExhaustiveLowCodes(t *testing.T) {
	for n := uint32(0); n < 4096; n++ {
		code := webtransport.StreamErrorCode(n)
		got, err := FromHTTP(ToHTTP(code))
		if err != nil {
			t.Fatalf("FromHTTP(ToHTTP(%d)) failed: %v", n, err)
		}
		if got != code {
			t.Fatalf("round trip %d -> %#x -> %d", n, uint64(ToHTTP(code)), got)
		}
	}
}

// TestRoundTripBoundaries covers the ends of the uint32 space and the exact
// boundary codes the library's own tests assert on (errors_test.go:23-38).
func TestRoundTripBoundaries(t *testing.T) {
	for _, n := range []webtransport.StreamErrorCode{
		0, 1, 0x1d, 0x1e, 0x1f, 0x20, 0x3b, 0x3c, 0x3d,
		math.MaxUint32 - 1, math.MaxUint32,
	} {
		h := ToHTTP(n)
		got, err := FromHTTP(h)
		if err != nil {
			t.Fatalf("FromHTTP(ToHTTP(%d)=%#x) failed: %v", n, uint64(h), err)
		}
		if got != n {
			t.Errorf("round trip %d -> %#x -> %d", n, uint64(h), got)
		}
	}

	// The mapping must be exactly onto the reserved range: code 0 lands on
	// FirstCode and MaxUint32 lands on LastCode.
	if h := ToHTTP(0); h != FirstCode {
		t.Errorf("ToHTTP(0) = %#x, want FirstCode %#x", uint64(h), uint64(FirstCode))
	}
	if h := ToHTTP(math.MaxUint32); h != LastCode {
		t.Errorf("ToHTTP(MaxUint32) = %#x, want LastCode %#x", uint64(h), uint64(LastCode))
	}
}

// TestRoundTripRandom mirrors the library's own randomized round-trip test
// (errors_test.go:13-21) so a divergence between our copy and theirs shows up.
func TestRoundTripRandom(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200_000; i++ {
		n := webtransport.StreamErrorCode(rnd.Uint32())
		got, err := FromHTTP(ToHTTP(n))
		if err != nil {
			t.Fatalf("FromHTTP(ToHTTP(%d)) failed: %v", n, err)
		}
		if got != n {
			t.Fatalf("round trip %d -> %d", n, got)
		}
	}
}

// TestForwardMappingIsInjective is the property that actually matters for a relay:
// two distinct WebTransport codes must never collide on one HTTP/3 code, or a
// clean stream-ended becomes indistinguishable from a fault after one hop.
func TestForwardMappingIsInjective(t *testing.T) {
	seen := make(map[quic.StreamErrorCode]webtransport.StreamErrorCode, 200_000)
	for n := uint32(0); n < 200_000; n++ {
		code := webtransport.StreamErrorCode(n)
		h := ToHTTP(code)
		if prev, ok := seen[h]; ok {
			t.Fatalf("collision: codes %d and %d both map to %#x", prev, code, uint64(h))
		}
		seen[h] = code
	}
}

// TestGreasedHolesRejected confirms the reserved 0x1f*N+0x21 holes are refused,
// and — the part that matters for a relay — that the forward mapping never
// produces one, so a legitimate WT code can never be mistaken for grease.
func TestGreasedHolesRejected(t *testing.T) {
	var checked int
	for i := uint64(0); i < 200_000; i++ {
		c := quic.StreamErrorCode(uint64(FirstCode) + i)
		if (c-0x21)%0x1f != 0 {
			continue
		}
		checked++
		if _, err := FromHTTP(c); !errors.Is(err, ErrGreased) {
			t.Fatalf("FromHTTP(%#x): got %v, want ErrGreased", uint64(c), err)
		}
	}
	if checked == 0 {
		t.Fatal("no greased values in scanned range; test is not exercising the branch")
	}

	for n := uint32(0); n < 200_000; n++ {
		h := ToHTTP(webtransport.StreamErrorCode(n))
		if (h-0x21)%0x1f == 0 {
			t.Fatalf("ToHTTP(%d) = %#x lands on a greased hole", n, uint64(h))
		}
	}
}

func TestOutOfRangeRejected(t *testing.T) {
	for _, h := range []quic.StreamErrorCode{
		0, 1, FirstCode - 1, LastCode + 1, math.MaxUint32,
		quic.StreamErrorCode(webtransport.WTSessionGoneErrorCode),
		quic.StreamErrorCode(webtransport.WTBufferedStreamRejectedErrorCode),
	} {
		if _, err := FromHTTP(h); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("FromHTTP(%#x): got %v, want ErrOutOfRange", uint64(h), err)
		}
	}
}

// TestWrongDivisorIsDetectable documents why the SPIKE.md shorthand
// "firstErrorCode + n + n/0x1e" is only half the story: using 0x1e in BOTH
// directions corrupts one code in every thirty. If this test ever fails, the
// asymmetry has been "cleaned up" and the relay is silently lossy.
func TestWrongDivisorIsDetectable(t *testing.T) {
	wrongFrom := func(h quic.StreamErrorCode) webtransport.StreamErrorCode {
		shifted := h - FirstCode
		return webtransport.StreamErrorCode(shifted - shifted/0x1e) // 0x1e, not 0x1f
	}
	var mismatches int
	for n := uint32(0); n < 1000; n++ {
		code := webtransport.StreamErrorCode(n)
		if wrongFrom(ToHTTP(code)) != code {
			mismatches++
		}
	}
	if mismatches == 0 {
		t.Fatal("the wrong divisor round-trips cleanly; the asymmetry claim is wrong")
	}
	t.Logf("using 0x1e in both directions corrupts %d of the first 1000 codes", mismatches)
}

// TestSessionErrorTakesPrecedence pins the ordering in Classify. When a session
// dies, the library resets each stream with WTSessionGoneErrorCode and then
// substitutes the session error (stream.go:81-83, 284-286). A relay that reported
// this as a stream reset would mirror the wrong event downstream.
func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantKind Kind
		check    func(*testing.T, Classified)
	}{
		{name: "nil", err: nil, wantKind: KindOK},
		{name: "eof", err: io.EOF, wantKind: KindEOF},
		{
			name:     "wrapped eof",
			err:      fmt.Errorf("reading control: %w", io.EOF),
			wantKind: KindEOF,
		},
		{
			name:     "stream reset remote",
			err:      &webtransport.StreamError{ErrorCode: 42, Remote: true},
			wantKind: KindStreamReset,
			check: func(t *testing.T, c Classified) {
				if c.StreamCode != 42 || !c.Remote {
					t.Errorf("got code=%d remote=%v, want 42/true", c.StreamCode, c.Remote)
				}
			},
		},
		{
			name:     "stream reset local",
			err:      &webtransport.StreamError{ErrorCode: 7, Remote: false},
			wantKind: KindStreamReset,
			check: func(t *testing.T, c Classified) {
				if c.StreamCode != 7 || c.Remote {
					t.Errorf("got code=%d remote=%v, want 7/false", c.StreamCode, c.Remote)
				}
			},
		},
		{
			name:     "session error wins over stream error",
			err:      &webtransport.SessionError{ErrorCode: 9, Message: "bye", Remote: true},
			wantKind: KindSessionClosed,
			check: func(t *testing.T, c Classified) {
				if c.SessionCode != 9 || c.SessionMessage != "bye" || !c.Remote {
					t.Errorf("got %+v", c)
				}
			},
		},
		{
			name:     "deadline exceeded",
			err:      os.ErrDeadlineExceeded,
			wantKind: KindTimeout,
		},
		{
			name:     "raw quic stream error in WT range",
			err:      &quic.StreamError{ErrorCode: ToHTTP(1234), Remote: true},
			wantKind: KindStreamReset,
			check: func(t *testing.T, c Classified) {
				if c.StreamCode != 1234 {
					t.Errorf("got code=%d, want 1234", c.StreamCode)
				}
			},
		},
		{
			name:     "raw quic stream error outside WT range",
			err:      &quic.StreamError{ErrorCode: 0x1234, Remote: true},
			wantKind: KindStreamResetUnmappable,
		},
		{
			name:     "session gone code is not a WT stream code",
			err:      &quic.StreamError{ErrorCode: webtransport.WTSessionGoneErrorCode, Remote: true},
			wantKind: KindStreamResetUnmappable,
		},
		{
			// The exact wording the library produces at stream.go:462.
			name:     "library conversion failure string",
			err:      fmt.Errorf("stream reset, but failed to convert stream error %d: %w", 1234, ErrOutOfRange),
			wantKind: KindStreamResetUnmappable,
		},
		{
			name:     "unrelated error",
			err:      errors.New("connection refused"),
			wantKind: KindOther,
		},
		{
			// The bug this guards: IdleTimeoutError.Timeout() returns true
			// (qerr/errors.go:90), so a generic Timeout() test classifies a dead
			// connection as a local deadline — the one kind the relay is required to
			// ignore. The two must not share a Kind.
			name:     "quic idle timeout is not a local deadline",
			err:      &quic.IdleTimeoutError{},
			wantKind: KindIdleTimeout,
		},
		{
			name:     "quic idle timeout wrapped",
			err:      fmt.Errorf("reading video stream: %w", &quic.IdleTimeoutError{}),
			wantKind: KindIdleTimeout,
		},
		{
			name:     "quic handshake timeout",
			err:      &quic.HandshakeTimeoutError{},
			wantKind: KindIdleTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Kind != tc.wantKind {
				t.Fatalf("Classify(%v).Kind = %v, want %v", tc.err, got.Kind, tc.wantKind)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestResetCodeFor(t *testing.T) {
	if code, ok := ResetCodeFor(Classified{Kind: KindStreamReset, StreamCode: 99}); !ok || code != 99 {
		t.Errorf("got %d,%v want 99,true", code, ok)
	}
	if code, ok := ResetCodeFor(Classified{Kind: KindStreamResetUnmappable}); ok || code != SubstituteResetCode {
		t.Errorf("got %d,%v want %d,false", code, ok, SubstituteResetCode)
	}
	if _, ok := ResetCodeFor(Classified{Kind: KindEOF}); ok {
		t.Error("EOF should not yield a reset code")
	}
}

// TestLibraryAgreement is the real guard against drift: push a code through the
// library's own public API (CancelWrite converts WT->HTTP internally) is not
// observable from outside, so instead assert our mapping against the library's
// documented boundary values, which its own tests assert (errors_test.go:26-37).
func TestLibraryAgreement(t *testing.T) {
	// errors_test.go:26-27: httpCodeToWebtransportCode(firstErrorCode) == 0.
	got, err := FromHTTP(FirstCode)
	if err != nil || got != 0 {
		t.Errorf("FromHTTP(FirstCode) = %d, %v; want 0, nil", got, err)
	}
	// errors_test.go:33-35: httpCodeToWebtransportCode(lastErrorCode) == MaxUint32.
	got, err = FromHTTP(LastCode)
	if err != nil || got != math.MaxUint32 {
		t.Errorf("FromHTTP(LastCode) = %d, %v; want MaxUint32, nil", got, err)
	}
}
