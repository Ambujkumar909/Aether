// Package oidctest is an identity provider for tests: discovery, a key
// set, a token endpoint, and tokens it signs itself -- with the knobs a test
// needs to make each one wrong.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IdP is a running fake provider.
type IdP struct {
	Server   *httptest.Server
	ClientID string
	Secret   string
	key      *rsa.PrivateKey
	kid      string

	mu    sync.Mutex
	codes map[string]Person // code -> who signs in
	// Tries counts token-endpoint calls, so a test can see a code was not
	// exchanged twice.
	Tries int
	// LastVerifier is the PKCE verifier the client sent.
	LastVerifier string
	// RotateKeys, when set, publishes a new key set on the next fetch.
	RotateKeys bool
}

// Person is who the provider says signed in.
type Person struct {
	Subject, Email, Name string
	EmailVerified        bool
	// Overrides, when set, replace the token's claims (to make them wrong).
	Issuer, Audience, Nonce string
	ExpiresIn               time.Duration
	// Alg forces a header algorithm; signature stays RS256 (so "none" and
	// "HS256" are rejected by the client, not accepted by accident).
	Alg string
}

// New starts a provider; close it with Close.
func New() *IdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &IdP{ClientID: "aether-client", Secret: "s3cret-s3cret", key: key, kid: "k1", codes: map[string]Person{}}
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 p.Server.URL,
			"authorization_endpoint": p.Server.URL + "/authorize",
			"token_endpoint":         p.Server.URL + "/token",
			"jwks_uri":               p.Server.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if p.RotateKeys {
			p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
			p.kid = "k" + base64.RawURLEncoding.EncodeToString([]byte(time.Now().String()))[:6]
			p.RotateKeys = false
		}
		k := p.key.PublicKey
		kid := p.kid
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	})
	// The real provider would show a sign-in page here; this one signs in
	// whoever the test registered for the code it was given, straight away.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code") // tests pass the code they registered
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.Tries++
		p.LastVerifier = r.PostForm.Get("code_verifier")
		who, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code")) // a code is single use
		p.mu.Unlock()
		id, pw, auth := r.BasicAuth()
		if !auth || id != p.ClientID || pw != p.Secret || !ok || r.PostForm.Get("grant_type") != "authorization_code" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		// PKCE: the verifier must hash to the challenge the client sent at
		// authorize time. This fake has no session; a test that cares checks
		// LastVerifier against the challenge itself.
		nonce := r.PostForm.Get("nonce")
		if who.Nonce != "" {
			nonce = who.Nonce
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": p.Token(who, nonce), "token_type": "Bearer"})
	})
	return p
}

// Register makes a code that signs in the given person.
func (p *IdP) Register(code string, who Person) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[code] = who
}

// Token signs an identity token for who, with the given nonce.
func (p *IdP) Token(who Person, nonce string) string {
	iss, aud := p.Server.URL, p.ClientID
	if who.Issuer != "" {
		iss = who.Issuer
	}
	if who.Audience != "" {
		aud = who.Audience
	}
	exp := time.Hour
	if who.ExpiresIn != 0 {
		exp = who.ExpiresIn
	}
	alg := "RS256"
	if who.Alg != "" {
		alg = who.Alg
	}
	p.mu.Lock()
	kid, key := p.kid, p.key
	p.mu.Unlock()
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"iss": iss, "sub": who.Subject, "aud": aud, "exp": now.Add(exp).Unix(), "iat": now.Unix(),
		"nonce": nonce, "email": who.Email, "email_verified": who.EmailVerified, "name": who.Name,
	})
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Close stops the provider.
func (p *IdP) Close() { p.Server.Close() }

// IssuerFor is the issuer URL a client is configured with.
func (p *IdP) Issuer() string { return strings.TrimSuffix(p.Server.URL, "/") }
