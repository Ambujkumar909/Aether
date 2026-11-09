//go:build windows

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"
)

const (
	updateFirstCheck = 45 * time.Second
	updateEvery      = 6 * time.Hour
)

// pinnedClient talks to the server with the trust the window has: a chain
// that validates normally, or the one pinned key.
func pinnedClient(server string, pin CertPin) *http.Client {
	host := hostOf(server)
	if i := indexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is done below, with the pin as the one exception
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if pin != "" && SPKIPin(leaf) == pin {
				return nil
			}
			inter := x509.NewCertPool()
			for _, b := range raw[1:] {
				if c, err := x509.ParseCertificate(b); err == nil {
					inter.AddCert(c)
				}
			}
			_, err = leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter})
			return err
		},
	}
	return &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment}}
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// watchUpdates asks the server for its app version shortly after start and
// then every few hours, downloads a newer one beside this program, and
// offers the restart in the menu. Quiet about everything but the result.
func (a *Agent) watchUpdates(ctx context.Context, pin CertPin) {
	if version == "dev" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	client := pinnedClient(a.cfg.Server, pin)
	wait := updateFirstCheck
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = updateEvery
		a.mu.Lock()
		ready := a.updateReady
		a.mu.Unlock()
		if ready != "" {
			continue
		}
		m, err := fetchManifest(ctx, client, a.cfg.Server)
		if err != nil {
			a.log.Debug("update check", "err", err)
			continue
		}
		if m == nil || !m.newerThan(version) {
			continue
		}
		if err := downloadUpdate(ctx, client, a.cfg.Server, m, exe+updateNewSuffix); err != nil {
			a.log.Warn("update download", "version", m.Version, "err", err)
			continue
		}
		a.log.Info("update downloaded; restart to apply", "version", m.Version)
		a.mu.Lock()
		a.updateReady = m.Version
		a.mu.Unlock()
	}
}
