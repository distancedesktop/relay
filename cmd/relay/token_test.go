package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okdaichi/webtransport-go"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/distancedesktop/relay/internal/certs"
)

const agentToken = "agent-viewer-token-0123456789"

func TestParseRouteToken(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt/d1=https://b.example:443/wt,fp=ABCDEF,token=s3cret")
	got, _ := p.lookup("/wt/d1")
	if got.Token != "s3cret" {
		t.Fatalf("Token = %q", got.Token)
	}
}

func TestUpstreamTokenFor(t *testing.T) {
	req := func(target string, hdr http.Header) *http.Request {
		r := httptest.NewRequest(http.MethodConnect, target, nil)
		for k, v := range hdr {
			r.Header[k] = v
		}
		return r
	}
	for name, tc := range map[string]struct {
		up   upstream
		req  *http.Request
		want string
	}{
		"route token wins": {upstream{Token: "route"}, req("https://relay/wt?token=viewer", nil), "route"},
		"viewer query":     {upstream{}, req("https://relay/wt?token=viewer", nil), "viewer"},
		"viewer bearer":    {upstream{}, req("https://relay/wt", http.Header{"Authorization": {"Bearer viewer"}}), "viewer"},
		"none":             {upstream{}, req("https://relay/wt", nil), ""},
	} {
		if got := upstreamTokenFor(tc.up, tc.req); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// startTokenAgent serves a WebTransport endpoint that, like the real agent,
// refuses upgrades without the expected bearer token.
func startTokenAgent(t *testing.T) (url, fp string) {
	t.Helper()
	gen, err := certs.SelfSigned("agent", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	up := &webtransport.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+agentToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, err := up.Upgrade(w, r); err != nil {
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := &webtransport.Server{H3: &http3.Server{
		TLSConfig:  &tls.Config{Certificates: []tls.Certificate{gen.TLS}, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
		Handler:    mux,
	}}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(conn)
	t.Cleanup(func() { srv.Close(); conn.Close() })
	return "https://" + conn.LocalAddr().String() + "/wt", gen.Fingerprint
}

func TestDialUpstreamPresentsToken(t *testing.T) {
	agentURL, fp := startTokenAgent(t)
	r := &relay{cfg: &relayConfig{quicConf: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}}}

	for name, tc := range map[string]struct {
		up     upstream
		client string
		ok     bool
	}{
		"forwards viewer token":   {upstream{URL: agentURL, Fingerprint: fp}, "https://relay/wt?token=" + agentToken, true},
		"route token":             {upstream{URL: agentURL, Fingerprint: fp, Token: agentToken}, "https://relay/wt", true},
		"viewer without token":    {upstream{URL: agentURL, Fingerprint: fp}, "https://relay/wt", false},
		"viewer with wrong token": {upstream{URL: agentURL, Fingerprint: fp}, "https://relay/wt?token=nope", false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sess, d, _, rsp, err := r.dialUpstream(ctx, tc.up, httptest.NewRequest(http.MethodConnect, tc.client, nil))
			if tc.ok {
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				sess.CloseWithError(0, "")
				d.Close()
				return
			}
			if err == nil {
				sess.CloseWithError(0, "")
				d.Close()
				t.Fatal("agent accepted a viewer without a valid token")
			}
			if rsp == nil || rsp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401 to forward, got rsp=%v err=%v", rsp, err)
			}
		})
	}
}

func TestRouteErrorsHideToken(t *testing.T) {
	for _, spec := range []string{
		"/wt=https://agent/wt,token=SECRETSECRET,unknown=x",
		"/wt=https://agent/wt,token SECRETSECRET",
		"/wt=token=SECRETSECRET",
		"/wt=%zz,token=SECRETSECRET",
	} {
		err := newPhonebook().parseRoute(spec)
		if err == nil {
			t.Fatalf("parseRoute(%q) accepted a bad spec", spec)
		}
		if strings.Contains(err.Error(), "SECRETSECRET") {
			t.Errorf("route error leaks the token: %v", err)
		}
	}
}

func TestRouteFlagHidesToken(t *testing.T) {
	var out strings.Builder
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(&out)
	routes := &routeFlag{book: newPhonebook()}
	fs.Var(routes, "route", "")
	err := fs.Parse([]string{"-route", "/wt=https://agent/wt,token=SECRETSECRET,unknown=x"})
	if err == nil && routes.err == nil {
		t.Fatal("bad -route accepted")
	}
	for _, s := range []string{out.String(), fmt.Sprint(err), fmt.Sprint(routes.err)} {
		if strings.Contains(s, "SECRETSECRET") {
			t.Errorf("-route error leaks the token: %s", s)
		}
	}
}
