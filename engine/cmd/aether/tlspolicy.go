package main

import "crypto/tls"

// Transport security policy for everything that crosses the network to
// this server: pages, the REST API, the chat push socket and the meeting
// signalling socket.
//
// TLS 1.3 only. Every 1.3 cipher suite is an AEAD (AES-GCM or ChaCha20-
// Poly1305) with forward-secret key exchange; there is no configuration in
// which a 1.3 connection negotiates RSA key transport, CBC, RC4 or SHA-1.
// Refusing 1.2 outright removes the whole downgrade surface rather than
// curating a list of acceptable 1.2 suites that must be kept current. Every
// browser this product runs in, WebView2, and Node have spoken 1.3 for
// years, so nothing that can use the product is turned away.
//
// Media does not pass through here: WebRTC encrypts it with DTLS-SRTP,
// keyed per call, and the SFU prefers the AES-GCM SRTP profiles (see
// internal/sfu/peer.go).
func hardenedTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The hybrid post-quantum exchange first (ML-KEM-768 with X25519,
		// what Chrome, Edge and Firefox offer): traffic recorded today stays
		// sealed against a future quantum computer. Naming curves at all
		// removes the ones not named, and this list once read X25519 and
		// P-256 alone, which turned the post-quantum exchange off for every
		// browser. Then X25519 (constant-time, nothing to validate) and P-256
		// for the rare client without it.
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519, tls.CurveP256},
	}
}
