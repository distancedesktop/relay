// Command fakeagent stands in for the real Distance agent so the relay spike is
// self-contained. It speaks the protocol documented in SPIKE.md exactly:
//
//   - endpoint /wt, one accepted client-initiated bidi stream carrying NDJSON
//   - on connect it pushes `fingerprint-refresh` then an unsolicited `displays`
//   - it implements list-displays, start, stop; anything else returns
//     {"type":"error","message":"unknown type: X"}
//   - `start` opens a server-initiated uni stream carrying H.264 Annex B
//   - the upgrader advertises ApplicationProtocols ["moq-lite-04"] as an
//     allow-list, so a client offering no protocol still upgrades
//   - Origin is enforced: empty, matching Host, the agent's own :52022 sibling,
//     or one passed via --allow-origin
//
// Self-signed ECDSA P-256 cert; the SHA-256 fingerprint is printed at startup so
// a Go client can pin it the way a browser pins serverCertificateHashes.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/okdaichi/webtransport-go"

	"github.com/distancedesktop/relay/internal/certs"
	"github.com/distancedesktop/relay/internal/protocol"
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:53080", "UDP address to listen on")
		fps         = flag.Int("fps", 60, "video frame rate")
		bitrate     = flag.Int("bitrate", 2_000_000, "target video bitrate in bits/sec")
		allowOrigin = flag.String("allow-origin", "", "additional acceptable Origin (comma-separated)")
		siblingPort = flag.Int("sibling-port", 52022, "the agent's own HTTP port, accepted as an Origin")
		stallLog    = flag.Duration("stall-log", 20*time.Millisecond, "log a video write that blocks longer than this")
		dropOnBlock = flag.Duration("drop-on-block", 0, "if >0, drop a frame rather than block longer than this (emulates a dropping encoder)")
		statsEvery  = flag.Duration("stats", 5*time.Second, "how often to log per-session stats; 0 disables")
	)
	flag.Parse()

	cert, err := certs.SelfSigned("distance-fakeagent", hostOf(*listen))
	if err != nil {
		log.Fatalf("fakeagent: %v", err)
	}

	a := &agent{
		fps:         *fps,
		bitrate:     *bitrate,
		fingerprint: cert.Fingerprint,
		stallLog:    *stallLog,
		dropOnBlock: *dropOnBlock,
		statsEvery:  *statsEvery,
	}

	up := &webtransport.Upgrader{
		// An allow-list, not a requirement: selectProtocol returns "" for a client
		// that offers nothing and Upgrade still proceeds to 200
		// (webtransport-go/upgrader.go:96,114-120).
		ApplicationProtocols: []string{"moq-lite-04"},
		CheckOrigin:          a.checkOrigin(*allowOrigin, *siblingPort),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := up.Upgrade(w, r)
		if err != nil {
			log.Printf("fakeagent: upgrade from %s rejected: %v", r.RemoteAddr, err)
			// Upgrade returns before writing a status for an origin failure, so we
			// have to produce the 400 ourselves — this is the "400 before the session
			// opens" that a relay sending a bad Origin would hit.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.serve(sess, r)
	})

	server := &webtransport.Server{
		H3: &http3.Server{
			Addr: *listen,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert.TLS},
				NextProtos:   []string{http3.NextProtoH3},
			},
			Handler: mux,
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   30 * time.Second,
			},
		},
	}

	fmt.Printf("fakeagent: listening on udp %s (path /wt)\n", *listen)
	fmt.Printf("fakeagent: cert SHA-256 %s\n", cert.Fingerprint)
	fmt.Printf("fakeagent: video %d fps, %d bit/s (~%d bytes/frame avg)\n",
		*fps, *bitrate, protocol.NewFrameGen(*fps, *bitrate).AvgFrameBytes())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("fakeagent: shutting down")
		_ = server.Close()
	}()

	if err := server.ListenAndServe(); err != nil {
		log.Printf("fakeagent: server exited: %v", err)
	}
}

// tlsConfig wraps a tls.Config so the h3 ALPN is always set.
type agent struct {
	fps         int
	bitrate     int
	fingerprint string
	stallLog    time.Duration
	dropOnBlock time.Duration
	statsEvery  time.Duration

	sessionSeq atomic.Int64
}

// checkOrigin reproduces the agent's rules from SPIKE.md: an empty Origin is
// accepted, an Origin whose host matches the request Host is accepted, the
// agent's own sibling HTTP port is accepted, and anything passed via
// --allow-origin is accepted. Everything else is a 400 before the session opens.
func (a *agent) checkOrigin(extra string, siblingPort int) func(*http.Request) bool {
	allowed := map[string]struct{}{}
	for _, o := range strings.Split(extra, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.ToLower(o)] = struct{}{}
		}
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		if _, ok := allowed[strings.ToLower(origin)]; ok {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			log.Printf("fakeagent: unparseable Origin %q", origin)
			return false
		}
		if strings.EqualFold(u.Host, r.Host) {
			return true
		}
		// The agent's own sibling HTTP server, e.g. https://127.0.0.1:52022.
		reqHost, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			reqHost = r.Host
		}
		oHost, oPort, err := net.SplitHostPort(u.Host)
		if err == nil && oPort == strconv.Itoa(siblingPort) && strings.EqualFold(oHost, reqHost) {
			return true
		}
		log.Printf("fakeagent: rejecting Origin %q (Host %q)", origin, r.Host)
		return false
	}
}

func (a *agent) serve(sess *webtransport.Session, r *http.Request) {
	id := a.sessionSeq.Add(1)
	logf := func(format string, args ...any) {
		log.Printf("fakeagent[s%d]: "+format, append([]any{id}, args...)...)
	}
	logf("session up from %s path=%s origin=%q protocol=%q",
		sess.RemoteAddr(), r.URL.Path, r.Header.Get("Origin"), sess.SessionState().ApplicationProtocol)

	st := &sessionState{agent: a, sess: sess, logf: logf}
	defer func() {
		st.stopVideo()
		logf("session down: %s", st.summary())
	}()

	if a.statsEvery > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(a.statsEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-sess.Context().Done():
					return
				case <-t.C:
					if st.videoRunning() {
						logf("stats %s", st.summary())
					}
				}
			}
		}()
	}

	// Datagram echo: the real agent does not use datagrams, but the relay must
	// prove it can carry them, so the fake agent reflects whatever it receives.
	go st.echoDatagrams()

	// The protocol is one client-initiated bidi control stream. Accept streams in a
	// loop anyway: a relay bug that opens a second one should be visible, not
	// silently swallowed.
	for {
		str, err := sess.AcceptStream(context.Background())
		if err != nil {
			logf("AcceptStream ended: %v", err)
			return
		}
		go st.handleControl(str)
	}
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}
