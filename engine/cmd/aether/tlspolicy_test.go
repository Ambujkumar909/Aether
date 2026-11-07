package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// A real handshake against the policy: a TLS 1.2-only client is refused, a
// 1.3 client connects and gets an AEAD suite. Not a check of the struct's
// fields -- the field could be set and the server built without it.
func TestHardenedTLSRefusesTLS12AndServes13(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := hardenedTLS()
	cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()

	dial := func(max uint16) (*tls.Conn, error) {
		return tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MaxVersion: max, MinVersion: tls.VersionTLS12})
	}
	if c, err := dial(tls.VersionTLS12); err == nil {
		c.Close()
		t.Fatal("a TLS 1.2 client was served")
	}
	c, err := dial(tls.VersionTLS13)
	if err != nil {
		t.Fatalf("a TLS 1.3 client was refused: %v", err)
	}
	defer c.Close()
	st := c.ConnectionState()
	if st.Version != tls.VersionTLS13 {
		t.Fatalf("negotiated %x", st.Version)
	}
	switch st.CipherSuite {
	case tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384, tls.TLS_CHACHA20_POLY1305_SHA256:
	default:
		t.Fatalf("not an AEAD 1.3 suite: %s", tls.CipherSuiteName(st.CipherSuite))
	}
	// A client that offers the post-quantum hybrid gets it (Go's own client,
	// like current browsers, offers it first).
	if st.CurveID != tls.X25519MLKEM768 {
		t.Fatalf("key exchange %v, want the post-quantum X25519MLKEM768", st.CurveID)
	}
	// And a client without it still connects, on classical X25519.
	old, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}})
	if err != nil {
		t.Fatalf("a client without ML-KEM was refused: %v", err)
	}
	defer old.Close()
	if cs := old.ConnectionState(); cs.CurveID != tls.X25519 {
		t.Fatalf("classical client got %v", cs.CurveID)
	}
}
