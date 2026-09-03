package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The phonebook is what decides which machine's screen a viewer gets. Routing it
// wrong is not a cosmetic bug: the relay's own certificate vouches for whatever
// upstream it picks, so a viewer has no way to notice it was handed the wrong
// desktop. That makes this pure logic worth testing.

func TestLookupExactMatchWins(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt=https://a.example:443/wt")
	mustRoute(t, p, "/wt/desktop-1=https://b.example:443/wt")

	got, ok := p.lookup("/wt/desktop-1")
	if !ok {
		t.Fatal("/wt/desktop-1 not found")
	}
	if got.URL != "https://b.example:443/wt" {
		t.Fatalf("got %q, want the desktop-1 entry", got.URL)
	}
}

// An unrecognised device name must 404 rather than silently resolve to the
// default. Falling back would hand a viewer someone else's screen on a typo.
func TestLookupUnknownDeviceDoesNotFallBack(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt=https://a.example:443/wt")

	for _, path := range []string{"/wt/desktop-7", "/wt/nope", "/other", "/wt/desktop-1/extra"} {
		if up, ok := p.lookup(path); ok {
			t.Errorf("lookup(%q) resolved to %q; an unknown device must not fall back", path, up.URL)
		}
	}
}

// A viewer that names no device at all is the one case the default exists for.
func TestLookupRootUsesDefault(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt=https://a.example:443/wt")

	got, ok := p.lookup("/")
	if !ok {
		t.Fatal("/ did not resolve to the default")
	}
	if got.URL != "https://a.example:443/wt" {
		t.Fatalf("got %q, want the /wt entry", got.URL)
	}

	// With no /wt configured there is no default, and / must 404.
	q := newPhonebook()
	mustRoute(t, q, "/wt/desktop-1=https://b.example:443/wt")
	if up, ok := q.lookup("/"); ok {
		t.Errorf("lookup(/) resolved to %q with no default configured", up.URL)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":               "/",
		"/":              "/",
		"//":             "/",
		"wt":             "/wt",
		"/wt":            "/wt",
		"/wt/":           "/wt",
		"/wt///":         "/wt",
		"/wt/desktop-1/": "/wt/desktop-1",
		"wt/desktop-1":   "/wt/desktop-1",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Trailing slashes are not meaningful for routing, so a route registered with one
// must be reachable without it and vice versa.
func TestLookupIgnoresTrailingSlash(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt/desktop-1/=https://b.example:443/wt")
	if _, ok := p.lookup("/wt/desktop-1"); !ok {
		t.Error("route added with a trailing slash is unreachable without one")
	}
	if _, ok := p.lookup("/wt/desktop-1/"); !ok {
		t.Error("route added with a trailing slash is unreachable with one")
	}
}

func TestParseRouteFields(t *testing.T) {
	p := newPhonebook()
	mustRoute(t, p, "/wt/d1=https://b.example:443/wt,fingerprint=ABCDEF,origin=https://viewer.example")

	got, ok := p.lookup("/wt/d1")
	if !ok {
		t.Fatal("route not registered")
	}
	if got.URL != "https://b.example:443/wt" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.Fingerprint != "ABCDEF" {
		t.Errorf("Fingerprint = %q", got.Fingerprint)
	}
	if got.Origin != "https://viewer.example" {
		t.Errorf("Origin = %q", got.Origin)
	}

	// fp= is accepted as a shorthand, and url= as an explicit key.
	mustRoute(t, p, "/wt/d2=url=https://c.example:443/wt,fp=123456")
	got, _ = p.lookup("/wt/d2")
	if got.URL != "https://c.example:443/wt" || got.Fingerprint != "123456" {
		t.Errorf("shorthand form parsed as %+v", got)
	}
}

func TestParseRouteRejectsBadInput(t *testing.T) {
	for _, spec := range []string{
		"no-equals-sign",
		"/wt=",
		"/wt=https://a.example/wt,bogus=1",
	} {
		p := newPhonebook()
		if err := p.parseRoute(spec); err == nil {
			t.Errorf("parseRoute(%q) accepted a bad spec", spec)
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "phonebook.json")
	body := map[string]upstream{
		"/wt":            {URL: "https://a.example:443/wt", Fingerprint: "aa"},
		"/wt/desktop-1/": {URL: "https://b.example:443/wt"},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	p := newPhonebook()
	if err := p.loadFile(path); err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if up, ok := p.lookup("/wt/desktop-1"); !ok || up.URL != "https://b.example:443/wt" {
		t.Errorf("desktop-1 = %+v ok=%v; keys must be normalized on load", up, ok)
	}
	// The /wt entry from a file must also become the default, exactly as a -route
	// would: otherwise the two configuration paths disagree.
	if up, ok := p.lookup("/"); !ok || up.URL != "https://a.example:443/wt" {
		t.Errorf("/ = %+v ok=%v; a file-loaded /wt must become the default", up, ok)
	}
}

func TestLoadFileRejectsEntryWithoutURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"/wt": {"fingerprint": "aa"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newPhonebook()
	if err := p.loadFile(path); err == nil {
		t.Error("loadFile accepted an entry with no url")
	}
}

func mustRoute(t *testing.T, p *phonebook, spec string) {
	t.Helper()
	if err := p.parseRoute(spec); err != nil {
		t.Fatalf("parseRoute(%q): %v", spec, err)
	}
}
