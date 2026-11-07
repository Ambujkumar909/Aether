//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"time"
)

// Trusting the one server this app was pointed at, and nothing else.
//
// A meeting server on a local network answers with a certificate it signed
// itself. A browser shows a full-page interstitial for that, and inside an
// application window -- with no address bar, no padlock to inspect, and no
// context -- it reads as the product being broken. Which is what happened: the
// app's first real launch showed "Your connection isn't private" instead of a
// sign-in screen.
//
// The tempting fix is --ignore-certificate-errors, and it is the wrong one: it
// turns off certificate checking for every host the window ever reaches, which
// is a browser with its safety removed. What this does instead is pin. The
// server's public key is fetched once, hashed, and passed to the runtime as
// the single exception it may make. Every other host still validates normally,
// and if this server's key ever changes the exception stops applying -- which
// is the property that makes it a pin rather than a hole.
//
// Nothing is pinned when the certificate already validates. A deployment with
// a real certificate gets ordinary, unmodified verification.

// CertPin is a base64 SHA-256 of a certificate's SubjectPublicKeyInfo, in the
// form Chromium's switch expects.
type CertPin string

// SPKIPin computes the pin for a certificate.
//
// The hash is over the public key, not the certificate. A server that reissues
// its certificate -- which this one does whenever the machine's address
// changes -- keeps its key and therefore keeps its pin, so the exception
// survives a reissue without being widened.
func SPKIPin(cert *x509.Certificate) CertPin {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return CertPin(base64.StdEncoding.EncodeToString(sum[:]))
}

// ErrNoPinNeeded means the server's certificate verifies on its own.
var ErrNoPinNeeded = errors.New("the server's certificate is already trusted")

// PinFor returns the exception this server needs, or ErrNoPinNeeded.
func PinFor(ctx context.Context, server string) (CertPin, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" {
		// Plain HTTP has no certificate to trust or distrust.
		return "", ErrNoPinNeeded
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}

	dialer := &net.Dialer{Timeout: ProbeTimeout}

	// First, honestly: if it validates, nothing is pinned.
	strict, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{
		ServerName: hostnameOnly(u.Host),
	})
	if err == nil {
		strict.Close()
		return "", ErrNoPinNeeded
	}

	// It did not. Fetch the certificate it actually presents, so the exception
	// names that key and no other.
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         hostnameOnly(u.Host),
	})
	if err != nil {
		return "", err
	}
	defer conn.Close()

	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return "", errors.New("the server presented no certificate")
	}
	return SPKIPin(chain[0]), nil
}

// BrowserArgsFor is what the runtime is told, given a pin.
//
// Empty when nothing needs pinning, so the ordinary case passes no switches at
// all and the runtime behaves exactly as it would with no interference.
func BrowserArgsFor(pin CertPin) string {
	if pin == "" {
		return ""
	}
	return "--ignore-certificate-errors-spki-list=" + string(pin)
}

func hostnameOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

var _ = time.Second
