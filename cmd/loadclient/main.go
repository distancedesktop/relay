// Command loadclient drives a full Distance session and reports what it cost.
//
// It runs the real sequence — connect, receive the unsolicited fingerprint-refresh
// and displays pushes, list-displays, start, read video for N seconds, stop — and
// reports throughput, first-byte latency, control RTT, total bytes, and any
// stream or session errors. Point it at the agent directly or at the relay; the
// numbers are directly comparable.
//
// --read-rate is the flag that makes this a real test rather than a demo: it
// throttles the video reader so the client is deliberately slower than the agent
// sends, which is the only way to see what two independently congestion-controlled
// QUIC connections do to each other under backpressure.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/certs"
	"github.com/distancedesktop/relay/internal/procstat"
	"github.com/distancedesktop/relay/internal/protocol"
)

func main() {
	var (
		url         = flag.String("url", "https://127.0.0.1:53080/wt", "WebTransport URL (agent directly, or the relay)")
		pin         = flag.String("pin", "", "expected server cert SHA-256; empty means skip verification entirely")
		origin      = flag.String("origin", "", "Origin header to send")
		duration    = flag.Duration("duration", 10*time.Second, "how long to read video after start")
		readRate    = flag.Int("read-rate", 0, "throttle video reads to this many bytes/sec (0 = read as fast as possible)")
		readChunk   = flag.Int("read-chunk", 32*1024, "read buffer size")
		datagrams   = flag.Int("datagrams", 0, "send this many datagrams and count echoes")
		label       = flag.String("label", "", "label for the report line")
		jsonOut     = flag.Bool("json", false, "emit the report as JSON")
		abortAfter  = flag.Duration("abort-after", 0, "if >0, hard-close the session this long after start without a stop (tests upstream teardown)")
		skipStop    = flag.Bool("skip-stop", false, "do not send stop; just close (tests half-open teardown)")
		protocols   = flag.String("protocols", "", "comma-separated WT application protocols to offer; empty offers none")
		connTimeout = flag.Duration("connect-timeout", 10*time.Second, "dial timeout")
	)
	flag.Parse()

	r, err := run(runConfig{
		url:         *url,
		pin:         *pin,
		origin:      *origin,
		duration:    *duration,
		readRate:    *readRate,
		readChunk:   *readChunk,
		datagrams:   *datagrams,
		abortAfter:  *abortAfter,
		skipStop:    *skipStop,
		protocols:   splitNonEmpty(*protocols),
		connTimeout: *connTimeout,
	})
	if r != nil {
		r.Label = *label
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(r)
		} else {
			fmt.Print(r.Text())
		}
	}
	if err != nil {
		log.Printf("loadclient: %v", err)
		os.Exit(1)
	}
}

type runConfig struct {
	url         string
	pin         string
	origin      string
	duration    time.Duration
	readRate    int
	readChunk   int
	datagrams   int
	abortAfter  time.Duration
	skipStop    bool
	protocols   []string
	connTimeout time.Duration
}

// Report is everything one run measured.
type Report struct {
	Label string `json:"label,omitempty"`
	URL   string `json:"url"`

	DialMillis         float64 `json:"dial_ms"`
	ServerFingerprint  string  `json:"server_fingerprint"`
	AgentFingerprint   string  `json:"agent_fingerprint_from_control,omitempty"`
	NegotiatedProtocol string  `json:"negotiated_protocol"`

	// Control-plane timings.
	PushFingerprintMillis float64 `json:"push_fingerprint_ms"`
	PushDisplaysMillis    float64 `json:"push_displays_ms"`
	ListDisplaysRTTMillis float64 `json:"list_displays_rtt_ms"`
	UnknownTypeRTTMillis  float64 `json:"unknown_type_rtt_ms"`
	StartRTTMillis        float64 `json:"start_rtt_ms"`
	StopRTTMillis         float64 `json:"stop_rtt_ms"`
	ControlRTTMedianMs    float64 `json:"control_rtt_median_ms"`
	UnknownTypeEcho       string  `json:"unknown_type_echo"`

	// Video.
	FirstVideoByteMillis float64 `json:"first_video_byte_ms_after_start"`
	VideoBytes           int64   `json:"video_bytes"`
	VideoFrames          int64   `json:"video_frames"`
	VideoSeconds         float64 `json:"video_seconds"`
	ThroughputMbps       float64 `json:"throughput_mbps"`
	FrameLatencyP50Ms    float64 `json:"frame_latency_p50_ms"`
	FrameLatencyP99Ms    float64 `json:"frame_latency_p99_ms"`
	FrameLatencyMaxMs    float64 `json:"frame_latency_max_ms"`
	FramesOutOfOrder     int64   `json:"frames_out_of_order"`
	FrameGaps            int64   `json:"frame_number_gaps"`
	StartCodeOK          bool    `json:"start_code_intact"`
	SawSPSPPS            bool    `json:"saw_sps_pps"`

	// Datagrams.
	DatagramsSent   int `json:"datagrams_sent"`
	DatagramsEchoed int `json:"datagrams_echoed"`

	// Teardown and faults.
	VideoEndKind   string   `json:"video_end_kind"`
	ControlEndKind string   `json:"control_end_kind"`
	SessionError   string   `json:"session_error,omitempty"`
	Errors         []string `json:"errors,omitempty"`

	// Cost.
	CPUSeconds float64 `json:"client_cpu_seconds"`
}

func (r *Report) Text() string {
	var b strings.Builder
	name := r.Label
	if name == "" {
		name = r.URL
	}
	fmt.Fprintf(&b, "=== %s ===\n", name)
	fmt.Fprintf(&b, "url                     %s\n", r.URL)
	fmt.Fprintf(&b, "dial                    %.2f ms\n", r.DialMillis)
	fmt.Fprintf(&b, "server cert sha256      %s\n", r.ServerFingerprint)
	if r.AgentFingerprint != "" {
		fmt.Fprintf(&b, "fingerprint in control  %s\n", r.AgentFingerprint)
	}
	fmt.Fprintf(&b, "negotiated protocol     %q\n", r.NegotiatedProtocol)
	fmt.Fprintf(&b, "push fingerprint-refresh %.2f ms after connect\n", r.PushFingerprintMillis)
	fmt.Fprintf(&b, "push displays            %.2f ms after connect\n", r.PushDisplaysMillis)
	fmt.Fprintf(&b, "control RTT  list-displays %.2f ms | unknown-type %.2f ms | start %.2f ms | stop %.2f ms | median %.2f ms\n",
		r.ListDisplaysRTTMillis, r.UnknownTypeRTTMillis, r.StartRTTMillis, r.StopRTTMillis, r.ControlRTTMedianMs)
	fmt.Fprintf(&b, "unknown-type echo       %q\n", r.UnknownTypeEcho)
	fmt.Fprintf(&b, "first video byte        %.2f ms after start\n", r.FirstVideoByteMillis)
	fmt.Fprintf(&b, "video                   %d bytes in %.2f s = %.3f Mbit/s\n", r.VideoBytes, r.VideoSeconds, r.ThroughputMbps)
	fmt.Fprintf(&b, "frames                  %d (gaps %d, out-of-order %d)\n", r.VideoFrames, r.FrameGaps, r.FramesOutOfOrder)
	fmt.Fprintf(&b, "frame latency           p50 %.2f ms | p99 %.2f ms | max %.2f ms\n",
		r.FrameLatencyP50Ms, r.FrameLatencyP99Ms, r.FrameLatencyMaxMs)
	fmt.Fprintf(&b, "annex-b integrity       start_code_intact=%v saw_sps_pps=%v\n", r.StartCodeOK, r.SawSPSPPS)
	if r.DatagramsSent > 0 {
		fmt.Fprintf(&b, "datagrams               sent %d, echoed %d\n", r.DatagramsSent, r.DatagramsEchoed)
	}
	fmt.Fprintf(&b, "teardown                video=%s control=%s\n", r.VideoEndKind, r.ControlEndKind)
	if r.SessionError != "" {
		fmt.Fprintf(&b, "session error           %s\n", r.SessionError)
	}
	fmt.Fprintf(&b, "client cpu              %.2f s\n", r.CPUSeconds)
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "  ! %s\n", e)
	}
	return b.String()
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func run(cfg runConfig) (*Report, error) {
	rep := &Report{URL: cfg.url}
	var errsMu sync.Mutex
	addErr := func(format string, args ...any) {
		errsMu.Lock()
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
		errsMu.Unlock()
	}

	tlsConf := &tls.Config{
		NextProtos: []string{http3.NextProtoH3},
		// serverCertificateHashes semantics: pinning skips CA *and* hostname
		// validation (SPIKE.md). InsecureSkipVerify plus a fingerprint check in
		// VerifyPeerCertificate is the Go equivalent.
		InsecureSkipVerify: true,
	}
	if cfg.pin != "" {
		tlsConf.VerifyPeerCertificate = certs.PinVerifier(cfg.pin)
	}
	// Record whatever cert we were actually served, pinned or not: proving the
	// browser never sees the agent's fingerprint is half the point of the spike.
	var servedFingerprint atomic.Value
	userVerify := tlsConf.VerifyPeerCertificate
	tlsConf.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if len(raw) > 0 {
			servedFingerprint.Store(certs.FingerprintOf(raw[0]))
		}
		if userVerify != nil {
			return userVerify(raw, chains)
		}
		return nil
	}

	d := &webtransport.Dialer{
		TLSClientConfig: tlsConf,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   30 * time.Second,
		},
		ApplicationProtocols: cfg.protocols,
	}
	defer d.Close()

	hdr := http.Header{}
	if cfg.origin != "" {
		hdr.Set("Origin", cfg.origin)
	}

	dialCtx, cancelDial := context.WithTimeout(context.Background(), cfg.connTimeout)
	defer cancelDial()
	dialStart := time.Now()
	rsp, sess, err := d.Dial(dialCtx, cfg.url, hdr)
	rep.DialMillis = msSince(dialStart)
	if fp, ok := servedFingerprint.Load().(string); ok {
		rep.ServerFingerprint = fp
	}
	if err != nil {
		status := 0
		if rsp != nil {
			status = rsp.StatusCode
		}
		return rep, fmt.Errorf("dial %s (status %d): %w", cfg.url, status, err)
	}
	defer sess.CloseWithError(0, "loadclient done")
	rep.NegotiatedProtocol = sess.SessionState().ApplicationProtocol

	// Video reader must be armed before `start`: the agent opens the uni stream as
	// soon as it handles the message, and a late AcceptUniStream would inflate the
	// first-byte number with our own scheduling delay.
	vid := &videoReader{
		chunk:    cfg.readChunk,
		rate:     cfg.readRate,
		addErr:   addErr,
		acquired: make(chan struct{}),
	}
	vidCtx, vidCancel := context.WithCancel(context.Background())
	defer vidCancel()
	var vidWG sync.WaitGroup
	vidWG.Add(1)
	go func() {
		defer vidWG.Done()
		vid.acceptAndRead(vidCtx, sess)
	}()

	var dgWG sync.WaitGroup
	var dgEchoed atomic.Int64
	if cfg.datagrams > 0 {
		dgWG.Add(1)
		go func() {
			defer dgWG.Done()
			for {
				b, err := sess.ReceiveDatagram(vidCtx)
				if err != nil {
					return
				}
				if len(b) > 0 {
					dgEchoed.Add(1)
				}
			}
		}()
	}

	ctl, err := sess.OpenStreamSync(dialCtx)
	if err != nil {
		return rep, fmt.Errorf("open control stream: %w", err)
	}
	connectedAt := time.Now()
	ctlReader := bufio.NewScanner(ctl)
	ctlReader.Buffer(make([]byte, 0, 4096), 1<<20)

	// A client-initiated bidi stream is not actually created on the wire until the
	// first byte is written, so nudge it with an empty line the agent's scanner
	// skips. Without this the agent never sees the stream and never pushes.
	if _, err := ctl.Write([]byte("\n")); err != nil {
		return rep, fmt.Errorf("nudge control stream: %w", err)
	}

	// The two unsolicited pushes, in order, before any request.
	msg, err := readControl(ctlReader)
	if err != nil {
		return rep, fmt.Errorf("reading fingerprint-refresh push: %w", err)
	}
	rep.PushFingerprintMillis = msSince(connectedAt)
	if msg.env.Type != protocol.TypeFingerprintRefresh {
		addErr("expected first push %q, got %q", protocol.TypeFingerprintRefresh, msg.env.Type)
	} else {
		var fr protocol.FingerprintRefreshMsg
		if err := json.Unmarshal(msg.raw, &fr); err == nil {
			rep.AgentFingerprint = fr.Fingerprint
		}
	}

	msg, err = readControl(ctlReader)
	if err != nil {
		return rep, fmt.Errorf("reading unsolicited displays push: %w", err)
	}
	rep.PushDisplaysMillis = msSince(connectedAt)
	if msg.env.Type != protocol.TypeDisplays {
		addErr("expected second push %q, got %q", protocol.TypeDisplays, msg.env.Type)
	}

	var rtts []float64
	roundTrip := func(req any, wantType string) (controlMsg, float64, error) {
		b, err := protocol.Encode(req)
		if err != nil {
			return controlMsg{}, 0, err
		}
		start := time.Now()
		if _, err := ctl.Write(b); err != nil {
			return controlMsg{}, 0, err
		}
		m, err := readControl(ctlReader)
		rtt := msSince(start)
		if err != nil {
			return controlMsg{}, rtt, err
		}
		rtts = append(rtts, rtt)
		if wantType != "" && m.env.Type != wantType {
			addErr("expected reply %q, got %q (%s)", wantType, m.env.Type, truncate(string(m.raw), 160))
		}
		return m, rtt, nil
	}

	if _, rtt, err := roundTrip(map[string]string{"type": protocol.TypeListDisplays}, protocol.TypeDisplays); err != nil {
		return rep, fmt.Errorf("list-displays: %w", err)
	} else {
		rep.ListDisplaysRTTMillis = rtt
	}

	// An unknown verb: the error wording must survive the relay verbatim.
	if m, rtt, err := roundTrip(map[string]string{"type": "relay-probe"}, protocol.TypeError); err != nil {
		addErr("unknown-type probe failed: %v", err)
	} else {
		rep.UnknownTypeRTTMillis = rtt
		var em protocol.ErrorMsg
		if err := json.Unmarshal(m.raw, &em); err == nil {
			rep.UnknownTypeEcho = em.Message
		}
		if em.Message != "unknown type: relay-probe" {
			addErr("unknown-type wording changed: %q", em.Message)
		}
	}

	startedAt := time.Now()
	if _, rtt, err := roundTrip(protocol.StartMsg{Type: protocol.TypeStart, DisplayID: "display-1"}, protocol.TypeStarted); err != nil {
		return rep, fmt.Errorf("start: %w", err)
	} else {
		rep.StartRTTMillis = rtt
	}
	vid.markStart(startedAt)

	if cfg.datagrams > 0 {
		// Send at a modest pace: quic-go's send queue holds 32 datagrams
		// (quic-go/datagram_queue.go:13) and Add blocks past that, so a tight loop
		// would measure our own queueing, not the path.
		for i := 0; i < cfg.datagrams; i++ {
			payload := fmt.Appendf(nil, "loadclient-datagram-%06d", i)
			if err := sess.SendDatagram(payload); err != nil {
				addErr("SendDatagram %d: %v", i, err)
				break
			}
			rep.DatagramsSent++
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Read video for the requested wall-clock duration.
	readDeadline := time.NewTimer(cfg.duration)
	defer readDeadline.Stop()
	var aborted bool
	var abortCh <-chan time.Time
	if cfg.abortAfter > 0 {
		t := time.NewTimer(cfg.abortAfter)
		defer t.Stop()
		abortCh = t.C
	}
	select {
	case <-readDeadline.C:
	case <-abortCh:
		aborted = true
	case <-sess.Context().Done():
		addErr("session ended during video read: %v", context.Cause(sess.Context()))
	}

	if aborted {
		// Hard close without a stop: does the relay tear the upstream down?
		vidCancel()
		_ = sess.CloseWithError(7, "loadclient abort")
		vidWG.Wait()
		dgWG.Wait()
		vid.finish(rep)
		rep.ControlEndKind = "aborted-by-client"
		rep.DatagramsEchoed = int(dgEchoed.Load())
		rep.CPUSeconds = cpuSeconds()
		return rep, nil
	}

	if !cfg.skipStop {
		if _, rtt, err := roundTrip(protocol.StopMsg{Type: protocol.TypeStop}, protocol.TypeStopped); err != nil {
			addErr("stop: %v", err)
			rep.ControlEndKind = "stop-failed"
		} else {
			rep.StopRTTMillis = rtt
			rep.ControlEndKind = "stopped"
		}
	} else {
		rep.ControlEndKind = "skipped-stop"
	}

	// After stop the agent closes the video stream; give the reader a moment to
	// observe the clean FIN rather than cancelling it ourselves, because whether
	// EOF survives the relay is exactly what we are testing.
	drain := time.NewTimer(1500 * time.Millisecond)
	defer drain.Stop()
	select {
	case <-vid.done():
	case <-drain.C:
		vidCancel()
	}
	vidWG.Wait()
	dgWG.Wait()

	if err := ctl.Close(); err != nil {
		addErr("control close: %v", err)
	}

	vid.finish(rep)
	if len(rtts) > 0 {
		sort.Float64s(rtts)
		rep.ControlRTTMedianMs = rtts[len(rtts)/2]
	}
	rep.DatagramsEchoed = int(dgEchoed.Load())
	if cfg.datagrams > 0 && rep.DatagramsEchoed == 0 {
		addErr("no datagrams echoed back (sent %d)", rep.DatagramsSent)
	}
	// Session.Context() is a plain WithCancel child (session.go:112), so its cause
	// is always context.Canceled once the session ends for ANY reason — the real
	// closeErr is unexported. Only report a cause that carries information.
	if cause := context.Cause(sess.Context()); cause != nil && !errors.Is(cause, context.Canceled) {
		rep.SessionError = cause.Error()
	}
	rep.CPUSeconds = cpuSeconds()
	return rep, nil
}

type controlMsg struct {
	env protocol.Envelope
	raw []byte
}

func readControl(sc *bufio.Scanner) (controlMsg, error) {
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return controlMsg{}, fmt.Errorf("malformed control JSON %q: %w", truncate(line, 120), err)
		}
		return controlMsg{env: env, raw: []byte(line)}, nil
	}
	if err := sc.Err(); err != nil {
		return controlMsg{}, err
	}
	return controlMsg{}, io.EOF
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Nanoseconds()) / 1e6
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func cpuSeconds() float64 {
	return procstat.TotalSeconds()
}
