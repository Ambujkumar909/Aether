package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Certificates from an ACME authority (Let's Encrypt by default), obtained and
// renewed by the server itself: -acme-domain names the host(s) it serves.
//
// Only the named hosts are ever requested. Without that whitelist anyone who
// points a DNS name at this server, or sends a crafted SNI, makes it ask the
// authority for a certificate in that name -- burning the account's rate limit
// until real renewals fail.

type acmeOptions struct {
	Domains   []string // exact hostnames
	Email     string   // the authority's contact for expiry notices
	CacheDir  string   // where the account key and certificates persist
	Directory string   // ACME directory URL; empty is Let's Encrypt production
	CAFile    string   // extra root to trust for Directory (a private or test authority)
}

func parseDomains(s string) ([]string, error) {
	var out []string
	for _, d := range strings.Split(s, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		// A hostname, not an address, a wildcard or a URL: the HTTP-01 and
		// TLS-ALPN-01 challenges this uses cannot prove any of those.
		if net.ParseIP(d) != nil || strings.ContainsAny(d, "*/: ") || !strings.Contains(d, ".") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
			return nil, fmt.Errorf("acme domain %q: want a hostname like meet.example.com", d)
		}
		out = append(out, d)
	}
	return out, nil
}

func newACME(o acmeOptions) (*autocert.Manager, error) {
	if len(o.Domains) == 0 {
		return nil, errors.New("acme: no domain")
	}
	if o.CacheDir == "" {
		return nil, errors.New("acme: no cache directory (certificates would be re-issued on every restart)")
	}
	if err := os.MkdirAll(o.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("acme cache: %w", err)
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(o.Domains...),
		Cache:      autocert.DirCache(o.CacheDir),
		Email:      o.Email,
	}
	if o.Directory != "" {
		hc := &http.Client{Timeout: 30 * time.Second}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("acme ca: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("acme ca %s: no certificate in it", o.CAFile)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		m.Client = &acme.Client{DirectoryURL: o.Directory, HTTPClient: hc}
	}
	return m, nil
}

// acmeTLS is the hardened policy with certificates from the manager, and the
// ALPN protocol the TLS-ALPN-01 challenge arrives on.
func acmeTLS(m *autocert.Manager) *tls.Config {
	cfg := hardenedTLS()
	cfg.GetCertificate = m.GetCertificate
	cfg.NextProtos = append([]string{"h2", "http/1.1"}, acme.ALPNProto)
	return cfg
}

// httpsRedirect answers plain HTTP: the HTTP-01 challenge through the manager,
// and everything else with a permanent redirect to the same path over HTTPS
// on the standard port. Only a whitelisted host is redirected to, so the
// redirect cannot be pointed at somebody else's site with a forged Host.
func httpsRedirect(m *autocert.Manager, domains []string) http.Handler {
	ok := map[string]bool{}
	for _, d := range domains {
		ok[d] = true
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !ok[host] {
			host = domains[0]
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	if m == nil {
		return fallback
	}
	return m.HTTPHandler(fallback)
}
