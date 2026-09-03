package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/certs"
	"github.com/distancedesktop/relay/internal/protocol"
	"github.com/distancedesktop/relay/internal/wterr"
)

// sessionState is everything one WebTransport session owns: the control stream
// writer, and at most one running video pump.
type sessionState struct {
	agent *agent
	sess  *webtransport.Session
	logf  func(string, ...any)

	writeMu sync.Mutex // serialises control writes; the video pump never touches it

	videoMu     sync.Mutex
	videoCancel context.CancelFunc
	videoDone   chan struct{}

	framesSent  atomic.Int64
	bytesSent   atomic.Int64
	framesDrop  atomic.Int64
	stallCount  atomic.Int64
	stallNanos  atomic.Int64
	worstStall  atomic.Int64
	videoActive atomic.Bool
}

func (s *sessionState) videoRunning() bool { return s.videoActive.Load() }

func (s *sessionState) summary() string {
	frames := s.framesSent.Load()
	bytes := s.bytesSent.Load()
	stalls := s.stallCount.Load()
	var avgStall time.Duration
	if stalls > 0 {
		avgStall = time.Duration(s.stallNanos.Load() / stalls)
	}
	return fmt.Sprintf("frames=%d bytes=%d dropped=%d stalls=%d avg_stall=%s worst_stall=%s",
		frames, bytes, s.framesDrop.Load(), stalls, avgStall.Round(time.Microsecond),
		time.Duration(s.worstStall.Load()).Round(time.Microsecond))
}

// sendControl writes one NDJSON message to the control stream.
func (s *sessionState) sendControl(str *webtransport.Stream, v any) error {
	b, err := protocol.Encode(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = str.Write(b)
	return err
}

// handleControl serves one control stream. The first thing it does — before
// reading anything — is push fingerprint-refresh and then an unsolicited
// displays, exactly as the real agent does (SPIKE.md).
func (s *sessionState) handleControl(str *webtransport.Stream) {
	defer func() {
		// EOF on the control stream is how the client says goodbye; make sure the
		// video pump does not outlive it.
		s.stopVideo()
	}()

	if err := s.sendControl(str, protocol.FingerprintRefreshMsg{
		Type:        protocol.TypeFingerprintRefresh,
		Fingerprint: s.agent.fingerprint,
		NotAfter:    time.Now().Add(certs.Validity).UTC().Format(time.RFC3339),
	}); err != nil {
		s.logf("control: fingerprint-refresh push failed: %v", err)
		return
	}
	if err := s.sendControl(str, protocol.DisplaysMsg{
		Type:     protocol.TypeDisplays,
		Displays: fakeDisplays(),
	}); err != nil {
		s.logf("control: unsolicited displays push failed: %v", err)
		return
	}

	// bufio.Scanner over the stream: NDJSON, one message per line. The default
	// 64 KiB token limit is far above any control message the protocol defines.
	sc := bufio.NewScanner(str)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := s.dispatch(str, []byte(line)); err != nil {
			s.logf("control: dispatch failed: %v", err)
			return
		}
	}
	if err := sc.Err(); err != nil {
		c := wterr.Classify(err)
		s.logf("control: read ended kind=%s err=%v", c.Kind, err)
		return
	}
	s.logf("control: client closed write side (clean EOF)")
}

func (s *sessionState) dispatch(str *webtransport.Stream, line []byte) error {
	var env protocol.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		// Malformed JSON has no defined behavior in the real agent; treat the
		// whole line as an unknown type so the response shape stays consistent.
		return s.sendControl(str, protocol.UnknownType(""))
	}

	switch env.Type {
	case protocol.TypeListDisplays:
		return s.sendControl(str, protocol.DisplaysMsg{
			Type:     protocol.TypeDisplays,
			Displays: fakeDisplays(),
		})

	case protocol.TypeStart:
		var msg protocol.StartMsg
		_ = json.Unmarshal(line, &msg)
		fps := msg.FPS
		if fps <= 0 {
			fps = s.agent.fps
		}
		bitrate := msg.Bitrate
		if bitrate <= 0 {
			bitrate = s.agent.bitrate
		}
		if err := s.startVideo(fps, bitrate); err != nil {
			return s.sendControl(str, protocol.ErrorMsg{Type: protocol.TypeError, Message: err.Error()})
		}
		return s.sendControl(str, protocol.StartedMsg{
			Type:      protocol.TypeStarted,
			DisplayID: msg.DisplayID,
			Width:     2560,
			Height:    1440,
			FPS:       fps,
			Bitrate:   bitrate,
		})

	case protocol.TypeStop:
		s.stopVideo()
		return s.sendControl(str, protocol.StoppedMsg{Type: protocol.TypeStopped})

	default:
		// The exact wording matters: the viewer matches on it (SPIKE.md).
		return s.sendControl(str, protocol.UnknownType(env.Type))
	}
}

func fakeDisplays() []protocol.Display {
	return []protocol.Display{
		{ID: "display-1", Name: "Built-in Retina Display", Width: 2560, Height: 1440, Scale: 2, Primary: true},
		{ID: "display-2", Name: "DELL U2720Q", Width: 3840, Height: 2160, Scale: 1},
	}
}

// startVideo opens a server-initiated uni stream and pumps Annex B frames onto it.
// Only one pump runs at a time; a second start replaces the first, as a real agent
// switching displays would.
func (s *sessionState) startVideo(fps, bitrate int) error {
	s.videoMu.Lock()
	defer s.videoMu.Unlock()
	if s.videoCancel != nil {
		s.videoCancel()
		<-s.videoDone
	}

	// OpenUniStreamSync blocks until the peer's stream credit allows a new stream.
	// Bound it: if the client (or a relay in the middle) is not granting credit we
	// want an error, not a hung control stream.
	ctx, cancel := context.WithTimeout(s.sess.Context(), 5*time.Second)
	defer cancel()
	str, err := s.sess.OpenUniStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open video stream: %w", err)
	}

	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.videoCancel = pumpCancel
	s.videoDone = done
	s.videoActive.Store(true)

	gen := protocol.NewFrameGen(fps, bitrate)
	go func() {
		defer close(done)
		defer s.videoActive.Store(false)
		s.pumpVideo(pumpCtx, str, gen)
	}()
	s.logf("video: started %d fps %d bit/s", fps, bitrate)
	return nil
}

func (s *sessionState) stopVideo() {
	s.videoMu.Lock()
	cancel, done := s.videoCancel, s.videoDone
	s.videoCancel, s.videoDone = nil, nil
	s.videoMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// pumpVideo writes frames at the configured rate until ctx is done or the stream
// fails.
//
// Blocking behavior, which is the whole point of the spike: SendStream.Write
// blocks when the peer's flow-control window is exhausted, i.e. when the reader is
// slower than this pump. A real encoder would drop frames rather than block, so
// --drop-on-block emulates that; with the default 0 the pump blocks, which is the
// harsher and more informative case for measuring a relay.
func (s *sessionState) pumpVideo(ctx context.Context, str *webtransport.SendStream, gen *protocol.FrameGen) {
	defer func() {
		// Clean FIN so the reader sees io.EOF rather than a reset. A relay must
		// carry this distinction; see wterr.KindEOF.
		if err := str.Close(); err != nil {
			s.logf("video: close: %v", err)
		}
	}()

	if _, err := str.Write(gen.Header()); err != nil {
		s.logf("video: SPS/PPS write failed: %v", err)
		return
	}

	interval := time.Second / time.Duration(gen.FPS())
	// A ticker, not a sleep loop: a sleep loop's error accumulates and the measured
	// bitrate drifts below target over a multi-minute run.
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.sess.Context().Done():
			return
		case <-tick.C:
		}

		frame := gen.Next(time.Now().UnixNano())

		if s.agent.dropOnBlock > 0 {
			// Emulate a dropping encoder: give the write a deadline and treat a
			// timeout as a dropped frame rather than an error.
			_ = str.SetWriteDeadline(time.Now().Add(s.agent.dropOnBlock))
		}

		start := time.Now()
		_, err := str.Write(frame)
		blocked := time.Since(start)

		if blocked > s.agent.stallLog {
			s.stallCount.Add(1)
			s.stallNanos.Add(int64(blocked))
			for {
				worst := s.worstStall.Load()
				if int64(blocked) <= worst || s.worstStall.CompareAndSwap(worst, int64(blocked)) {
					break
				}
			}
			s.logf("video: write blocked %s (frame %d, %d bytes)", blocked.Round(time.Millisecond), s.framesSent.Load()+1, len(frame))
		}

		if err != nil {
			c := wterr.Classify(err)
			if c.Kind == wterr.KindTimeout {
				s.framesDrop.Add(1)
				continue
			}
			s.logf("video: write failed kind=%s err=%v", c.Kind, err)
			return
		}
		s.framesSent.Add(1)
		s.bytesSent.Add(int64(len(frame)))
	}
}

// echoDatagrams reflects every datagram back. The real agent does not use
// datagrams, but the relay must carry them, and an echo is the cheapest way to
// prove a round trip end to end.
func (s *sessionState) echoDatagrams() {
	for {
		b, err := s.sess.ReceiveDatagram(s.sess.Context())
		if err != nil {
			return
		}
		if err := s.sess.SendDatagram(b); err != nil {
			// Datagrams are unreliable by contract: a too-large or dropped datagram
			// is not a session failure.
			s.logf("datagram: echo failed (%d bytes): %v", len(b), err)
		}
	}
}
