package protocol

import (
	"encoding/binary"
	"math/rand/v2"
)

// H.264 NAL unit types we emit. Only the shapes matter here — the bytes after the
// header are filler. A viewer that only demuxes on start codes (which is what the
// relay's byte pipe has to survive) cannot tell the difference, and a real decoder
// will reject the payload, which is fine for a transport spike.
const (
	nalTypeSPS = 7
	nalTypePPS = 8
	nalTypeIDR = 5
	nalTypeP   = 1
)

// startCode is the 4-byte Annex B start code. Real NAL start codes matter because
// the relay must not reframe, coalesce, or split the stream in a way a demuxer
// would notice — a corrupted start code is the first thing that would break.
var startCode = []byte{0x00, 0x00, 0x00, 0x01}

// FrameGen produces synthetic H.264-shaped Annex B frames at a target rate and
// bitrate. It is deterministic in size (so throughput math is checkable) and
// carries a monotonic frame counter plus a send timestamp in the payload so a
// reader can compute per-frame latency without a side channel.
type FrameGen struct {
	fps         int
	bitrate     int // bits per second, target
	frameNum    uint32
	keyInterval int // emit an IDR every N frames
	rnd         *rand.Rand
}

// NewFrameGen builds a generator. fps and bitrate must be positive.
func NewFrameGen(fps, bitrate int) *FrameGen {
	if fps <= 0 {
		fps = 60
	}
	if bitrate <= 0 {
		bitrate = 2_000_000
	}
	return &FrameGen{
		fps:         fps,
		bitrate:     bitrate,
		keyInterval: fps * 2, // a keyframe every 2s, like a typical screen encoder
		rnd:         rand.New(rand.NewPCG(0x5EED, 0xD157)),
	}
}

// FPS reports the configured frame rate.
func (g *FrameGen) FPS() int { return g.fps }

// Bitrate reports the configured target bitrate in bits per second.
func (g *FrameGen) Bitrate() int { return g.bitrate }

// AvgFrameBytes is the target payload size per frame implied by the bitrate.
func (g *FrameGen) AvgFrameBytes() int {
	n := g.bitrate / 8 / g.fps
	if n < 64 {
		n = 64
	}
	return n
}

// Header writes the SPS and PPS that a real agent sends before the first frame.
// These go out once at the head of the video stream.
func (g *FrameGen) Header() []byte {
	// Fixed-size fake parameter sets. A real SPS encodes resolution and profile;
	// ours is only shaped like one.
	sps := []byte{0x67, 0x64, 0x00, 0x28, 0xac, 0xd9, 0x40, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0xe8}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
	out := make([]byte, 0, len(startCode)*2+len(sps)+len(pps))
	out = append(out, startCode...)
	out = append(out, sps...)
	out = append(out, startCode...)
	out = append(out, pps...)
	return out
}

// Next returns the next Annex B frame. sendUnixNano is stamped into the payload
// at a fixed offset so the reader can measure one-way latency per frame; see
// ParseStamp.
func (g *FrameGen) Next(sendUnixNano int64) []byte {
	g.frameNum++
	isKey := g.frameNum == 1 || int(g.frameNum)%g.keyInterval == 0

	// Keyframes are much larger than P-frames in a real stream. Keep the running
	// average near the configured bitrate: an IDR costs ~8x a P-frame, so shrink
	// the P-frames enough that the mean over a GOP still lands on target.
	avg := g.AvgFrameBytes()
	var size int
	var nalType byte
	if isKey {
		nalType = nalTypeIDR
		size = avg * 8
	} else {
		nalType = nalTypeP
		// keyInterval frames per GOP: one IDR of 8*avg plus (k-1) P-frames must
		// total k*avg, so a P-frame is avg*(k-8)/(k-1).
		k := g.keyInterval
		num := avg * (k - 8)
		den := k - 1
		if den <= 0 || num <= 0 {
			size = avg
		} else {
			size = num / den
		}
		// Jitter ±12% so the stream is not perfectly periodic; a constant frame
		// size can hide pacing bugs that a variable one exposes.
		jitter := size / 8
		if jitter > 0 {
			size += g.rnd.IntN(2*jitter) - jitter
		}
	}
	if size < stampLen+1 {
		size = stampLen + 1
	}

	buf := make([]byte, 0, len(startCode)+1+size)
	buf = append(buf, startCode...)
	// NAL header: forbidden_zero_bit=0, nal_ref_idc=3 for IDR/param sets, type.
	refIdc := byte(2)
	if isKey {
		refIdc = 3
	}
	buf = append(buf, refIdc<<5|nalType)

	// Payload: frame number + send timestamp, then filler. Emulation-prevention
	// bytes are not inserted, so in principle the filler could contain a false
	// start code. We avoid that by keeping filler bytes non-zero.
	stamp := make([]byte, stampLen)
	binary.BigEndian.PutUint32(stamp[0:4], g.frameNum)
	binary.BigEndian.PutUint64(stamp[4:12], uint64(sendUnixNano))
	buf = append(buf, stamp...)

	for len(buf) < len(startCode)+1+size {
		// 0x01..0xff only: never emit a run of zeros that a demuxer would read as
		// a start code inside the payload.
		buf = append(buf, byte(1+g.rnd.IntN(255)))
	}
	return buf
}

// stampLen is the number of payload bytes reserved for the frame counter and
// send timestamp.
const stampLen = 12

// FrameHeaderLen is the number of bytes from the beginning of a frame up to and
// including the stamp: start code + NAL header + stamp.
const FrameHeaderLen = 4 + 1 + stampLen

// ParseStamp extracts the frame number and send timestamp from a frame that
// starts with an Annex B start code. ok is false if b is too short or does not
// begin with a start code.
func ParseStamp(b []byte) (frameNum uint32, sendUnixNano int64, ok bool) {
	if len(b) < FrameHeaderLen {
		return 0, 0, false
	}
	if b[0] != 0x00 || b[1] != 0x00 || b[2] != 0x00 || b[3] != 0x01 {
		return 0, 0, false
	}
	p := b[5:]
	return binary.BigEndian.Uint32(p[0:4]), int64(binary.BigEndian.Uint64(p[4:12])), true
}

// IsStartCode reports whether b begins with the 4-byte Annex B start code.
func IsStartCode(b []byte) bool {
	return len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1
}
