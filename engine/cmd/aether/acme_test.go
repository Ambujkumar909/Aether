package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
)

func TestACMEDomainsAreHostnamesOnly(t *testing.T) {
	got, err := parseDomains(" Meet.Example.com , chat.example.com,")
	if err != nil || !slices.Equal(got, []string{"meet.example.com", "chat.example.com"}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"203.0.113.7", "*.example.com", "https://meet.example.com", "localhost", "meet.example.com:443", ".example.com", "a b.com"} {
		if _, err := parseDomains(bad); err == nil {
			t.Errorf("%q accepted as an ACME domain", bad)
		}
	}
}

// Only the named hosts are ever asked for: a crafted SNI for any other name
// is refused before the authority is contacted, so nobody can spend the
// account's rate limit by pointing DNS at the server.
func TestACMERequestsOnlyTheNamedHosts(t *testing.T) {
	m, err := newACME(acmeOptions{Domains: []string{"meet.example.com"}, CacheDir: filepath.Join(t.TempDir(), "acme"),
		Directory: "http://127.0.0.1:1/directory"}) // nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	if err := m.HostPolicy(context.Background(), "meet.example.com"); err != nil {
		t.Fatalf("the named host was refused: %v", err)
	}
	for _, h := range []string{"evil.example.com", "example.com", "meet.example.com.attacker.net", "127.0.0.1"} {
		if err := m.HostPolicy(context.Background(), h); err == nil {
			t.Errorf("host policy allowed %q", h)
		}
		if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: h}); err == nil {
			t.Errorf("a certificate was produced for %q", h)
		}
	}
	cfg := acmeTLS(m)
	if cfg.MinVersion != tls.VersionTLS13 || !slices.Contains(cfg.NextProtos, "acme-tls/1") || cfg.GetCertificate == nil {
		t.Fatalf("acme TLS config lost the policy or the challenge protocol: %+v", cfg.NextProtos)
	}
}

func TestACMENeedsADomainAndACache(t *testing.T) {
	if _, err := newACME(acmeOptions{CacheDir: t.TempDir()}); err == nil {
		t.Fatal("no domain accepted")
	}
	if _, err := newACME(acmeOptions{Domains: []string{"meet.example.com"}}); err == nil {
		t.Fatal("no cache accepted")
	}
}

// Plain HTTP sends everybody to HTTPS on a named host, never to a host the
// request made up.
func TestPlainHTTPRedirectsOnlyToANamedHost(t *testing.T) {
	h := httpsRedirect(nil, []string{"meet.example.com"})
	for host, want := range map[string]string{
		"meet.example.com":      "https://meet.example.com/room?x=1",
		"MEET.example.com:80":   "https://meet.example.com/room?x=1",
		"attacker.example":      "https://meet.example.com/room?x=1",
		"meet.example.com.evil": "https://meet.example.com/room?x=1",
	} {
		r := httptest.NewRequest(http.MethodGet, "http://x/room?x=1", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != want {
			t.Errorf("Host %q: %d %q, want 301 %q", host, w.Code, w.Header().Get("Location"), want)
		}
	}
}
