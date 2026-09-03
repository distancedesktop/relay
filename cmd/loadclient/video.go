package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/protocol"
	"github.com/distancedesktop/relay/internal/wterr"
)

// videoReader accepts the agent's server-initiated uni stream and consumes it,
// optionally throttled.
//
// The throttle is the point of this type. If the client always reads as fast as
// the network delivers, a relay looks fine no matter how it handles flow control:
// nothing ever backs up. Setting --read-rate below the agent's bitrate forces the
// downstream flow-control window shut, and the question the spike exists to answer
// is whether that pressure reaches the agent or gets absorbed (i.e. buffered
// without bound) by the relay.
type videoReader struct {
	chunk  int
	rate   int // bytes/sec, 0 = unthrottled
	addErr func(string, ...any)

	acquired  chan struct{} // closed once the uni stream is accepted
	doneCh    chan struct{} // closed when reading finishes
	closeOnce sync.Once

	mu        sync.Mutex
	startedAt time.Time // when `started` came back, for first-byte latency
	firstByte time.Time
	begunAt   time.Time
	endedAt   time.Time
	bytes     int64
	frames    int64
	latencies []float64
	outOfOrd  int64
	gaps      int64
	lastFrame uint32
	haveFrame bool
	sawHeader bool
	startCode bool
	endKind   string
}

func (v *videoReader) done() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.doneCh == nil {
		v.doneCh = make(chan struct{})
	}
	return v.doneCh
}

func (v *videoReader) markDone() {
	v.mu.Lock()
	if v.doneCh == nil {
		v.doneCh = make(chan struct{})
	}
	ch := v.doneCh
	v.mu.Unlock()
	v.closeOnce.Do(func() { close(ch) })
}

// markStart records when the `started` reply landed, which is the zero point for
// first-video-byte latency.
func (v *videoReader) markStart(t time.Time) {
	v.mu.Lock()
	v.startedAt = t
	v.mu.Unlock()
}

func (v *videoReader) acceptAndRead(ctx context.Context, sess *webtransport.Session) {
	defer v.markDone()

	str, err := sess.AcceptUniStream(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c := wterr.Classify(err)
			v.addErr("AcceptUniStream: kind=%s err=%v", c.Kind, err)
			v.setEndKind("accept-failed:" + c.Kind.String())
		} else {
			v.setEndKind("no-video-stream")
		}
		return
	}
	close(v.acquired)
	v.read(ctx, str)
}

func (v *videoReader) setEndKind(k string) {
	v.mu.Lock()
	if v.endKind == "" {
		v.endKind = k
	}
	v.mu.Unlock()
}

func (v *videoReader) read(ctx context.Context, str *webtransport.ReceiveStream) {
	buf := make([]byte, v.chunk)
	// A carry buffer for frame-boundary parsing: a chunk read can split a frame
	// header across two reads, so keep up to one header's worth of tail bytes.
	var carry []byte

	// Throttling by budget rather than by sleep-per-read: sleeping a fixed amount
	// per read makes the effective rate depend on the chunk size, which would make
	// runs at different --read-chunk values incomparable.
	var (
		budgetStart time.Time
		consumed    int64
	)

	for {
		if ctx.Err() != nil {
			v.setEndKind("cancelled")
			return
		}

		if v.rate > 0 {
			if budgetStart.IsZero() {
				budgetStart = time.Now()
			}
			// How far ahead of the allowed rate are we?
			elapsed := time.Since(budgetStart).Seconds()
			allowed := elapsed * float64(v.rate)
			if float64(consumed) > allowed {
				excess := float64(consumed) - allowed
				sleep := time.Duration(excess / float64(v.rate) * float64(time.Second))
				if sleep > 0 {
					// A read deadline is not used here: the point is to stop reading
					// entirely so the receive window closes. Sleeping with no read
					// outstanding is exactly what a slow consumer does.
					select {
					case <-time.After(sleep):
					case <-ctx.Done():
						v.setEndKind("cancelled")
						return
					}
				}
			}
		}

		// A generous read deadline so a stalled path is reported rather than hanging
		// the whole run. 30s is well past any legitimate stall at these bitrates.
		_ = str.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := str.Read(buf)
		now := time.Now()

		if n > 0 {
			v.mu.Lock()
			if v.firstByte.IsZero() {
				v.firstByte = now
				v.begunAt = now
			}
			v.bytes += int64(n)
			v.endedAt = now
			v.mu.Unlock()
			consumed += int64(n)
			carry = v.scan(append(carry, buf[:n]...), now)
		}

		if err != nil {
			c := wterr.Classify(err)
			switch c.Kind {
			case wterr.KindEOF:
				v.setEndKind("clean-eof")
			case wterr.KindStreamReset:
				v.setEndKind("reset:" + itoa(uint64(c.StreamCode)))
				v.addErr("video stream reset with code %d (remote=%v)", c.StreamCode, c.Remote)
			case wterr.KindStreamResetUnmappable:
				v.setEndKind("reset-unmappable")
				v.addErr("video stream reset with an unmappable code: %v", err)
			case wterr.KindSessionClosed:
				v.setEndKind("session-closed:" + itoa(uint64(c.SessionCode)))
			case wterr.KindTimeout:
				v.setEndKind("read-timeout")
				v.addErr("video read timed out with no data for 30s")
			case wterr.KindIdleTimeout:
				// Distinct from the local deadline above: the connection itself went
				// away. Reporting both as "read-timeout" would hide whether the sender
				// went quiet or the transport died, which is exactly the difference that
				// matters when judging a relay.
				v.setEndKind("idle-timeout")
				v.addErr("video read ended on a QUIC idle timeout: %v", err)
			default:
				v.setEndKind("error:" + c.Kind.String())
				v.addErr("video read: %v", err)
			}
			return
		}
	}
}

// scan walks Annex B frame boundaries, pulls the embedded timestamp out of each
// frame, and returns any trailing bytes that could still be the start of a header.
func (v *videoReader) scan(b []byte, now time.Time) []byte {
	v.mu.Lock()
	defer v.mu.Unlock()

	i := 0
	for {
		// Find the next start code at or after i.
		j := indexStartCode(b, i)
		if j < 0 {
			break
		}
		if j+protocol.FrameHeaderLen > len(b) {
			// Header straddles the chunk boundary; keep it for next time.
			i = j
			break
		}
		v.startCode = true
		nalType := b[j+4] & 0x1f
		switch nalType {
		case 7, 8: // SPS, PPS: no stamp, they are the header pair
			v.sawHeader = true
			i = j + 5
			continue
		}
		// ParseStamp wants the slice to begin at the start code, not past the NAL
		// header. Passing b[j+5:] instead silently "parses" only frame 1, whose
		// big-endian frame number is itself 00 00 00 01.
		frameNum, sendNanos, ok := protocol.ParseStamp(b[j:])
		if !ok {
			i = j + 5
			continue
		}
		v.frames++
		lat := float64(now.UnixNano()-sendNanos) / 1e6
		if lat >= 0 && lat < 60_000 {
			v.latencies = append(v.latencies, lat)
		}
		if v.haveFrame {
			switch {
			case frameNum == v.lastFrame+1:
			case frameNum > v.lastFrame+1:
				v.gaps += int64(frameNum - v.lastFrame - 1)
			default:
				v.outOfOrd++
			}
		}
		v.lastFrame = frameNum
		v.haveFrame = true
		i = j + protocol.FrameHeaderLen
	}

	// Retain at most 3 bytes: a start code is 4 bytes, so only a 1-3 byte prefix
	// can be ambiguous — unless we stopped on a straddling header, in which case
	// keep the whole tail from there.
	if i < len(b) {
		tail := b[i:]
		if len(tail) > protocol.FrameHeaderLen+3 {
			tail = tail[len(tail)-(protocol.FrameHeaderLen+3):]
		}
		out := make([]byte, len(tail))
		copy(out, tail)
		return out
	}
	return nil
}

func indexStartCode(b []byte, from int) int {
	for i := from; i+4 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i
		}
	}
	return -1
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for u > 0 {
		i--
		d[i] = byte('0' + u%10)
		u /= 10
	}
	return string(d[i:])
}

// finish folds the reader's measurements into the report.
func (v *videoReader) finish(rep *Report) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.firstByte.IsZero() && !v.startedAt.IsZero() {
		rep.FirstVideoByteMillis = float64(v.firstByte.Sub(v.startedAt).Nanoseconds()) / 1e6
	}
	rep.VideoBytes = v.bytes
	rep.VideoFrames = v.frames
	if !v.begunAt.IsZero() && v.endedAt.After(v.begunAt) {
		secs := v.endedAt.Sub(v.begunAt).Seconds()
		rep.VideoSeconds = secs
		if secs > 0 {
			rep.ThroughputMbps = float64(v.bytes) * 8 / secs / 1e6
		}
	}
	if len(v.latencies) > 0 {
		s := make([]float64, len(v.latencies))
		copy(s, v.latencies)
		sort.Float64s(s)
		rep.FrameLatencyP50Ms = s[len(s)*50/100]
		rep.FrameLatencyP99Ms = s[min(len(s)*99/100, len(s)-1)]
		rep.FrameLatencyMaxMs = s[len(s)-1]
	}
	rep.FramesOutOfOrder = v.outOfOrd
	rep.FrameGaps = v.gaps
	rep.StartCodeOK = v.startCode
	rep.SawSPSPPS = v.sawHeader
	if v.endKind == "" {
		v.endKind = "still-open"
	}
	rep.VideoEndKind = v.endKind
}
