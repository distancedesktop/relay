// Command relay terminates a browser's WebTransport session and splices it to a
// freshly dialed upstream agent session.
//
// The question it exists to answer: can a relay terminate WebTransport, re-dial a
// real upstream, and splice the two so an unmodified viewer works — while the
// browser trusts only the relay's cert and never learns the agent's fingerprint?
//
// Shape:
//
//	browser --(relay's cert, pinned by the browser)--> relay --(agent's cert,
//	pinned by the relay)--> agent
//
// Two independently congestion-controlled QUIC connections. The relay owns no
// buffer beyond one chunk per direction, so the client's receive window is the only
// queue and pressure propagates to the agent. Whether that actually holds is
// measured, not assumed: every write that blocks past --stall-threshold is logged
// with the in-flight byte count.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/certs"
	"github.com/distancedesktop/relay/internal/procstat"
)

// tooLarge reports whether a SendDatagram failure was "this datagram does not fit
// on this leg" rather than a real error. The two legs have independent MTU
// estimates and independent quarter-stream-ID prefix lengths, so a size accepted
// on one is not guaranteed on the other, and quic-go exposes no way to ask in
// advance (connection.go:3033-3037).
func tooLarge(err error) bool {
	var e *quic.DatagramTooLargeError
	return errors.As(err, &e)
}

type relayConfig struct {
	chunkSize       int
	stallThreshold  time.Duration
	upstreamOrigin  string
	upstreamTimeout time.Duration
	insecureUpstrm  bool

	// quicConf is the config both legs share. The upstream leg needs it as much as
	// the client-facing one: the relay is the *receiver* on the upstream leg, so its
	// own receive window is exactly the buffer the agent can fill before its Write
	// blocks. Configuring only the server side would leave the video path's real
	// queue at the default and make any window experiment meaningless.
	quicConf *quic.Config
}

// legConfig returns a fresh copy of the shared QUIC config. quic-go mutates the
// config it is handed (server.go:115-146 clones for exactly this reason), so each
// leg gets its own.
func (c *relayConfig) legConfig() *quic.Config {
	conf := *c.quicConf
	return &conf
}

type relay struct {
	cfg   *relayConfig
	book  *phonebook
	logf  func(string, ...any)
	seq   atomic.Int64
	stats sync.Map // int64 session id -> *sessionStats

	// ctx is cancelled when the relay is shutting down. Splices watch this, never
	// the request context: the request context dies with the CONNECT stream, which
	// is the same event as the client session ending.
	ctx context.Context
}

func main() {
	var (
		listen       = flag.String("listen", "127.0.0.1:53090", "UDP address to terminate client WebTransport on")
		routesFile   = flag.String("routes", "", "JSON phonebook file: {\"/wt/desktop-1\": {\"url\": \"...\", \"fingerprint\": \"...\"}}")
		defaultUp    = flag.String("upstream", "", "shorthand for -route /wt=<url>")
		defaultFP    = flag.String("upstream-fingerprint", "", "expected SHA-256 of the -upstream cert")
		certFile     = flag.String("cert", "", "TLS certificate for the client-facing side; empty means self-signed")
		keyFile      = flag.String("key", "", "TLS key matching -cert")
		origin       = flag.String("upstream-origin", "", "Origin header to send upstream (default: https://<upstream host>)")
		allowOrigin  = flag.String("allow-origin", "", "comma-separated Origins the relay accepts from clients; empty means same-origin or absent")
		chunk        = flag.Int("chunk", 32*1024, "splice copy chunk size in bytes")
		stall        = flag.Duration("stall-threshold", 5*time.Millisecond, "log a splice write that blocks longer than this")
		upTimeout    = flag.Duration("upstream-timeout", 10*time.Second, "upstream dial timeout")
		insecureUp   = flag.Bool("insecure-upstream", false, "accept any upstream cert when no fingerprint is configured")
		statsEvery   = flag.Duration("stats", 5*time.Second, "how often to log per-session splice stats; 0 disables")
		streamWindow = flag.Uint64("stream-window", 0, "override InitialStreamReceiveWindow on both legs (0 = quic-go default 512 KB)")
		connWindow   = flag.Uint64("conn-window", 0, "override InitialConnectionReceiveWindow on both legs (0 = quic-go default 768 KB)")
		maxStreamWin = flag.Uint64("max-stream-window", 0, "override MaxStreamReceiveWindow on both legs (0 = quic-go default 6 MB)")
		maxConnWin   = flag.Uint64("max-conn-window", 0, "override MaxConnectionReceiveWindow on both legs (0 = quic-go default 15 MB)")
		maxIdle      = flag.Duration("max-idle", 30*time.Second, "QUIC MaxIdleTimeout on both legs; this is the upper bound on how long a vanished client holds its upstream session open")
	)
	book := newPhonebook()
	flag.Var(routeFlag{book: book}, "route", "path=url[,fingerprint=...][,origin=...]; repeatable")
	flag.Parse()

	if *defaultUp != "" {
		spec := "/wt=" + *defaultUp
		if *defaultFP != "" {
			spec += ",fingerprint=" + *defaultFP
		}
		if err := book.parseRoute(spec); err != nil {
			log.Fatalf("relay: %v", err)
		}
	}
	if *routesFile != "" {
		if err := book.loadFile(*routesFile); err != nil {
			log.Fatalf("relay: %v", err)
		}
	}
	if book.empty() {
		log.Fatalf("relay: no upstreams configured; pass -upstream or -route or -routes")
	}

	tlsConf, fingerprint, err := clientFacingTLS(*certFile, *keyFile, *listen)
	if err != nil {
		log.Fatalf("relay: %v", err)
	}

	shutdownCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()

	quicConf := &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		// The idle timeout is the backstop that tears down a session nobody is
		// speaking for. It matters to the measurement because it is the *upper bound*
		// on how long a vanished client's upstream session survives: when a client
		// aborts while the relay is blocked writing video down to it, the abort is not
		// what ends the upstream leg — this timer is. See FINDINGS.md.
		MaxIdleTimeout: *maxIdle,
		// Windows left at quic-go defaults unless overridden: 512 KB initial stream,
		// 6 MB max stream, 768 KB initial conn, 15 MB max conn. The defaults are what
		// a real deployment would run, so they are what the measurement should use.
		//
		// These matter more on the relay than anywhere else. On the upstream leg the
		// relay is the receiver, so its own receive window is precisely the amount of
		// video the agent may hand over before its Write blocks — i.e. the buffer the
		// splice inserts whether it wants to or not. --stream-window is how that
		// buffer is measured against latency.
		InitialStreamReceiveWindow:     *streamWindow,
		MaxStreamReceiveWindow:         *maxStreamWin,
		InitialConnectionReceiveWindow: *connWindow,
		MaxConnectionReceiveWindow:     *maxConnWin,
	}

	r := &relay{
		cfg: &relayConfig{
			chunkSize:       *chunk,
			stallThreshold:  *stall,
			upstreamOrigin:  *origin,
			upstreamTimeout: *upTimeout,
			insecureUpstrm:  *insecureUp,
			quicConf:        quicConf,
		},
		book: book,
		logf: func(format string, args ...any) { log.Printf("relay: "+format, args...) },
		ctx:  shutdownCtx,
	}

	up := &webtransport.Upgrader{
		// Deliberately empty: the relay must not negotiate a protocol the upstream
		// has not agreed to. Upgrade commits the response header before the relay can
		// dial (upgrader.go:96,114-120), so the honest behavior for a spike is to
		// negotiate nothing and let the client fall back — see FINDINGS.md.
		CheckOrigin: r.checkOrigin(*allowOrigin),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		r.handle(up, w, req)
	})

	server := &webtransport.Server{
		H3: &http3.Server{
			Addr:       *listen,
			TLSConfig:  tlsConf,
			Handler:    mux,
			QUICConfig: quicConf,
		},
	}

	fmt.Printf("relay: listening on udp %s\n", *listen)
	if fingerprint != "" {
		fmt.Printf("relay: cert SHA-256 %s\n", fingerprint)
	} else {
		fmt.Printf("relay: using cert %s (fingerprint not printed for a supplied cert)\n", *certFile)
	}
	fmt.Printf("relay: phonebook:%s\n", book.describe())
	fmt.Printf("relay: chunk %d bytes, stall threshold %s\n", *chunk, *stall)

	if *statsEvery > 0 {
		go r.statsLoop(*statsEvery)
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		r.logf("shutting down")
		shutdown()
		_ = server.Close()
	}()

	if err := server.ListenAndServe(); err != nil {
		r.logf("server exited: %v", err)
	}
}

// clientFacingTLS builds the TLS config the browser sees. The whole point of the
// relay is that this is the only cert the browser ever validates.
func clientFacingTLS(certFile, keyFile, listen string) (*tls.Config, string, error) {
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, "", errors.New("both -cert and -key are required together")
		}
		pair, err := certs.LoadPair(certFile, keyFile)
		if err != nil {
			return nil, "", err
		}
		return &tls.Config{
			Certificates: []tls.Certificate{pair.TLS},
			NextProtos:   []string{http3.NextProtoH3},
		}, pair.Fingerprint, nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	gen, err := certs.SelfSigned("distance-relay", host)
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{gen.TLS},
		NextProtos:   []string{http3.NextProtoH3},
	}, gen.Fingerprint, nil
}

// checkOrigin mirrors the agent's policy: absent Origin is accepted, an Origin
// matching the request Host is accepted, and anything in --allow-origin is
// accepted. This is the relay's own policy, independent of what it later sends
// upstream.
func (r *relay) checkOrigin(extra string) func(*http.Request) bool {
	allowed := map[string]struct{}{}
	for _, o := range strings.Split(extra, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.ToLower(o)] = struct{}{}
		}
	}
	return func(req *http.Request) bool {
		origin := req.Header.Get("Origin")
		if origin == "" {
			return true
		}
		if _, ok := allowed[strings.ToLower(origin)]; ok {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if strings.EqualFold(u.Host, req.Host) {
			return true
		}
		r.logf("rejecting client Origin %q (Host %q)", origin, req.Host)
		return false
	}
}

func (r *relay) statsLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		var live bool
		r.stats.Range(func(k, v any) bool {
			live = true
			r.logf("[s%v] %s", k, v.(*sessionStats).summary())
			return true
		})
		// Goroutines are logged unconditionally, including with no session live: a
		// count that does not return to its idle baseline after every session has
		// ended is a leak, and that is only visible in the gaps between sessions.
		if c, err := procstat.Read(); err == nil {
			r.logf("process cpu=%.2fs (user %.2f sys %.2f) threads=%d goroutines=%d sessions_live=%v",
				c.Total(), c.UserSeconds, c.SystemSeconds, c.Threads, runtime.NumGoroutine(), live)
		} else {
			r.logf("process goroutines=%d sessions_live=%v", runtime.NumGoroutine(), live)
		}
	}
}

// handle is the whole termination decision, in order:
//
//  1. Route the request path to an upstream. Unknown path -> 404, before any
//     upstream work.
//  2. Dial the upstream FIRST. If the agent refuses (bad Origin -> its own 400,
//     wrong fingerprint -> TLS failure, not running -> dial timeout), the client
//     gets a status code and never sees a half-open session. Doing this after
//     Upgrade would be unrecoverable: Upgrade has already written 200 and flushed
//     (upgrader.go:134-135), so there is no status left to send.
//  3. Upgrade the client.
//  4. Splice.
func (r *relay) handle(up *webtransport.Upgrader, w http.ResponseWriter, req *http.Request) {
	id := r.seq.Add(1)
	logf := func(format string, args ...any) {
		log.Printf("relay[s%d]: "+format, append([]any{id}, args...)...)
	}

	target, ok := r.book.lookup(req.URL.Path)
	if !ok {
		logf("no upstream for path %q", req.URL.Path)
		http.NotFound(w, req)
		return
	}

	// The client's protocol offer, read before Upgrade so it can be forwarded. See
	// FINDINGS.md on why the relay cannot faithfully proxy the negotiated protocol.
	clientProtocols := req.Header.Values("WT-Available-Protocols")

	dialCtx, cancelDial := context.WithTimeout(req.Context(), r.cfg.upstreamTimeout)
	defer cancelDial()

	upSess, upDialer, upFP, rsp, err := r.dialUpstream(dialCtx, target, req)
	if err != nil {
		status := http.StatusBadGateway
		if rsp != nil {
			// The agent's own rejection: forward its status verbatim so the viewer
			// sees the same thing it would see talking directly to the agent.
			status = rsp.StatusCode
		}
		logf("upstream dial %s failed (forwarding %d): %v", target.URL, status, err)
		w.WriteHeader(status)
		return
	}
	defer upDialer.Close()

	clientSess, err := up.Upgrade(w, req)
	if err != nil {
		// Upgrade writes no status of its own on failure — the only WriteHeader in
		// the library is the 200 at upgrader.go:134 — so the relay must produce one.
		logf("client upgrade rejected: %v", err)
		w.WriteHeader(http.StatusForbidden)
		_ = upSess.CloseWithError(0, "relay: client upgrade rejected")
		return
	}

	logf("spliced %s%s -> %s (upstream cert %s, client offered protocols %v, negotiated %q)",
		req.RemoteAddr, req.URL.Path, target.URL, firstN(upFP, 16), clientProtocols,
		clientSess.SessionState().ApplicationProtocol)

	st := &sessionStats{}
	st.ClientToUpstream.name = "c->u"
	st.UpstreamToClient.name = "u->c"
	r.stats.Store(id, st)
	defer r.stats.Delete(id)

	sp := &spliced{
		id:       id,
		client:   clientSess,
		upstream: upSess,
		cfg:      r.cfg,
		stats:    st,
		logf:     logf,
	}
	start := time.Now()
	// r.ctx, not req.Context(): see spliced.run.
	sp.run(r.ctx)
	logf("session down after %s: %s", time.Since(start).Round(time.Millisecond), st.summary())
	if st.firstErr != nil {
		logf("first unclassified error was: %v", st.firstErr)
	}
}

// dialUpstream opens a fresh WebTransport session to the agent.
//
// Every Dial builds its own QUIC connection and http3.Transport (client.go:110-120)
// with no pooling, so per-session upstream isolation is free — two clients never
// share a connection to the agent, and one client's flow control cannot affect
// another's.
func (r *relay) dialUpstream(ctx context.Context, target upstream, req *http.Request) (
	*webtransport.Session, *webtransport.Dialer, string, *http.Response, error) {

	tlsConf := &tls.Config{
		NextProtos: []string{http3.NextProtoH3},
		// The relay is the party that pins the agent. This is exactly the browser's
		// serverCertificateHashes check moved one hop back: hostname and CA
		// validation are skipped, the SHA-256 of the leaf is what matters.
		InsecureSkipVerify: true,
	}
	var seen atomic.Value
	pinned := target.Fingerprint != ""
	verify := certs.PinVerifier(target.Fingerprint)
	tlsConf.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if len(raw) > 0 {
			seen.Store(certs.FingerprintOf(raw[0]))
		}
		if pinned {
			return verify(raw, chains)
		}
		if !r.cfg.insecureUpstrm {
			return errors.New("relay: upstream has no configured fingerprint and -insecure-upstream is not set")
		}
		return nil
	}

	d := &webtransport.Dialer{
		TLSClientConfig: tlsConf,
		// The same windows as the client-facing leg. On this leg the relay is the
		// receiver of video, so this window — not anything in the splice — is the
		// buffer the agent is allowed to fill ahead of a slow viewer.
		QUICConfig: r.cfg.legConfig(),
		// Not set: forwarding the client's offer here would let the upstream select a
		// protocol the relay has already failed to advertise downstream.
	}

	hdr := http.Header{}
	if o := r.upstreamOriginFor(target); o != "" {
		hdr.Set("Origin", o)
	}

	rsp, sess, err := d.Dial(ctx, target.URL, hdr)
	fp, _ := seen.Load().(string)
	if err != nil {
		d.Close()
		return nil, nil, fp, rsp, err
	}
	return sess, d, fp, rsp, nil
}

// upstreamOriginFor picks the Origin to present to the agent. The agent accepts an
// absent Origin, one matching its own Host, or its sibling HTTP port; the default
// here is the scheme+host of the upstream URL, which satisfies the Host rule.
func (r *relay) upstreamOriginFor(target upstream) string {
	if target.Origin != "" {
		return target.Origin
	}
	if r.cfg.upstreamOrigin != "" {
		return r.cfg.upstreamOrigin
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		return ""
	}
	return "https://" + u.Host
}
