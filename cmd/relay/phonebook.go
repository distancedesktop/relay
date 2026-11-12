package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
)

// upstream is one entry in the phonebook: where to dial, and what to trust there.
type upstream struct {
	// URL is the full WebTransport URL to dial, e.g. https://127.0.0.1:53080/wt.
	URL string `json:"url"`
	// Fingerprint, if set, is the expected SHA-256 of the upstream's leaf cert. This
	// is the relay standing in for the browser's serverCertificateHashes: the relay
	// pins the agent, and the browser pins only the relay.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Origin overrides the relay-wide default Origin for this upstream.
	Origin string `json:"origin,omitempty"`
	// Token, if set, is the agent's viewer token, sent upstream as
	// `Authorization: Bearer`. It lets the relay authorize viewers on the agent's
	// behalf, so only set it when the relay itself is access-controlled. Unset,
	// the relay forwards the token the viewer presented.
	Token string `json:"token,omitempty"`
}

// phonebook maps a client request path to an upstream agent. In production this
// would be a lookup against the real device registry; here it is a static map from
// a flag or a small JSON file, which is enough to prove the routing shape works.
type phonebook struct {
	mu      sync.RWMutex
	entries map[string]upstream // keyed by request path, e.g. "/wt/desktop-1"
	def     *upstream           // the "/wt" default, if configured
}

func newPhonebook() *phonebook {
	return &phonebook{entries: map[string]upstream{}}
}

// parseRoutes accepts repeated -route path=url[,fingerprint][,origin=...][,token=...] values.
func (p *phonebook) parseRoute(spec string) error {
	path, rest, ok := strings.Cut(spec, "=")
	if !ok {
		return fmt.Errorf("route: want path=url[,fingerprint=...][,origin=...][,token=...]")
	}
	path = normalizePath(path)
	var up upstream
	for i, field := range strings.Split(rest, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		k, v, hasEq := strings.Cut(field, "=")
		switch {
		case i == 0 && !hasEq:
			up.URL = field
		case strings.EqualFold(k, "fingerprint"), strings.EqualFold(k, "fp"):
			up.Fingerprint = v
		case strings.EqualFold(k, "origin"):
			up.Origin = v
		case strings.EqualFold(k, "token"):
			up.Token = v
		case strings.EqualFold(k, "url"):
			up.URL = v
		default:
			return fmt.Errorf("route %s: unknown field %q", path, k)
		}
	}
	if up.URL == "" {
		return fmt.Errorf("route %s: no url", path)
	}
	if _, err := url.Parse(up.URL); err != nil {
		return fmt.Errorf("route %s: bad url", path)
	}
	p.add(path, up)
	return nil
}

// loadFile reads a JSON phonebook: {"/wt/desktop-1": {"url": "...", ...}, ...}.
func (p *phonebook) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]upstream
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range raw {
		if v.URL == "" {
			return fmt.Errorf("%s: route %q has no url", path, k)
		}
		p.add(normalizePath(k), v)
	}
	return nil
}

func (p *phonebook) add(path string, up upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[path] = up
	if path == "/wt" {
		cp := up
		p.def = &cp
	}
}

// lookup resolves a request path. An exact match wins. A request that names no
// device at all ("/") falls back to the "/wt" entry, which is the documented
// default for a viewer that does not pick one.
//
// An unrecognised device name deliberately does NOT fall back. A relay that
// answered /wt/desktop-7 with whatever the default happens to be would hand a
// viewer the wrong machine's screen on a typo — silently, and with the relay's own
// cert vouching for it. A 404 is the safe answer.
func (p *phonebook) lookup(path string) (upstream, bool) {
	path = normalizePath(path)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if up, ok := p.entries[path]; ok {
		return up, true
	}
	if path == "/" && p.def != nil {
		return *p.def, true
	}
	return upstream{}, false
}

func (p *phonebook) describe() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.entries) == 0 {
		return "(empty)"
	}
	var b strings.Builder
	for path, up := range p.entries {
		fmt.Fprintf(&b, "\n  %-20s -> %s", path, up.URL)
		if up.Fingerprint != "" {
			fmt.Fprintf(&b, " (pinned %s…)", firstN(up.Fingerprint, 16))
		}
		if up.Origin != "" {
			fmt.Fprintf(&b, " origin=%s", up.Origin)
		}
		if up.Token != "" {
			b.WriteString(" token=<set>")
		}
	}
	return b.String()
}

func (p *phonebook) empty() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries) == 0
}

func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// Trailing slashes are not meaningful for routing, but "/" itself must survive.
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// routeFlag collects repeated -route values.
type routeFlag struct{ book *phonebook }

func (r routeFlag) String() string { return "" }

func (r routeFlag) Set(v string) error { return r.book.parseRoute(v) }
