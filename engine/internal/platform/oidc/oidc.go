// Package oidc signs people in through an OpenID Connect provider -- an
// organisation's Google Workspace, Microsoft Entra, Okta, Keycloak or any
// other -- with the authorization code flow and PKCE, on the standard
// library alone. The provider is configured by its issuer URL; everything
// else comes from its discovery document.
//
// What is checked on the identity token: the signature against the
// provider's published keys (RS256 or ES256; nothing else, never "none"),
// the issuer, that this client is an audience, expiry and issue time with a
// little skew, and the nonce this server chose for this sign-in.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Claims is what a verified identity token says about the person.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Provider is one configured identity provider.
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTP         *http.Client

	authEndpoint  string
	tokenEndpoint string
	jwksURI       string

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey // by kid
	fetched time.Time
}

const (
	maxBody   = 1 << 20
	keysTTL   = time.Hour
	clockSkew = 5 * time.Minute
)

var ErrToken = errors.New("oidc: identity token rejected")

// New reads the provider's discovery document. The issuer must be https
// (localhost excepted, for tests) and must name itself in the document.
func New(ctx context.Context, issuer, clientID, clientSecret, redirectURL string, hc *http.Client) (*Provider, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !isLocal(u.Hostname())) || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("oidc: issuer must be an https URL, got %q", issuer)
	}
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("oidc: client id, client secret and redirect URL are required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	p := &Provider{Issuer: strings.TrimSuffix(issuer, "/"), ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL, HTTP: hc}
	var disc struct {
		Issuer string `json:"issuer"`
		Auth   string `json:"authorization_endpoint"`
		Token  string `json:"token_endpoint"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := p.getJSON(ctx, p.Issuer+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	if strings.TrimSuffix(disc.Issuer, "/") != p.Issuer {
		return nil, fmt.Errorf("oidc: the discovery document names issuer %q, configured %q", disc.Issuer, p.Issuer)
	}
	for _, e := range []string{disc.Auth, disc.Token, disc.JWKS} {
		if eu, err := url.Parse(e); err != nil || eu.Host == "" || (eu.Scheme != "https" && !isLocal(eu.Hostname())) {
			return nil, fmt.Errorf("oidc: discovery endpoint %q is not https", e)
		}
	}
	p.authEndpoint, p.tokenEndpoint, p.jwksURI = disc.Auth, disc.Token, disc.JWKS
	return p, nil
}

func isLocal(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// PKCE makes a code verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// Random is a URL-safe random string for state and nonce.
func Random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL is where the browser is sent to sign in.
func (p *Provider) AuthURL(state, nonce, challenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {p.RedirectURL},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(p.authEndpoint, "?") {
		sep = "&"
	}
	return p.authEndpoint + sep + q.Encode()
}

// Exchange trades the code for the identity token, then verifies it.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.RedirectURL},
		"client_id":     {p.ClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))
	res, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: token endpoint: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: token endpoint answered %d", res.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return nil, errors.New("oidc: token endpoint returned no identity token")
	}
	return p.Verify(ctx, tok.IDToken, nonce)
}

// Verify checks an identity token and returns its claims.
func (p *Provider) Verify(ctx context.Context, token, nonce string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrToken
	}
	hdrB, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	bodyB, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, ErrToken
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrB, &hdr); err != nil {
		return nil, ErrToken
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return nil, fmt.Errorf("%w: algorithm %q", ErrToken, hdr.Alg)
	}
	key, err := p.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, signed[:], sig) != nil {
			return nil, fmt.Errorf("%w: bad signature", ErrToken)
		}
	case *ecdsa.PublicKey:
		if hdr.Alg != "ES256" || len(sig) != 64 {
			return nil, fmt.Errorf("%w: bad signature", ErrToken)
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, signed[:], r, s) {
			return nil, fmt.Errorf("%w: bad signature", ErrToken)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported key", ErrToken)
	}

	var c struct {
		Iss           string          `json:"iss"`
		Sub           string          `json:"sub"`
		Aud           json.RawMessage `json:"aud"`
		Exp           int64           `json:"exp"`
		Iat           int64           `json:"iat"`
		Nonce         string          `json:"nonce"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		Name          string          `json:"name"`
	}
	if err := json.Unmarshal(bodyB, &c); err != nil {
		return nil, ErrToken
	}
	now := time.Now()
	switch {
	case strings.TrimSuffix(c.Iss, "/") != p.Issuer:
		return nil, fmt.Errorf("%w: issuer %q", ErrToken, c.Iss)
	case !audienceHas(c.Aud, p.ClientID):
		return nil, fmt.Errorf("%w: not for this client", ErrToken)
	case c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)):
		return nil, fmt.Errorf("%w: expired", ErrToken)
	case c.Iat != 0 && time.Unix(c.Iat, 0).After(now.Add(clockSkew)):
		return nil, fmt.Errorf("%w: issued in the future", ErrToken)
	case nonce == "" || c.Nonce != nonce:
		return nil, fmt.Errorf("%w: nonce", ErrToken)
	case c.Sub == "":
		return nil, fmt.Errorf("%w: no subject", ErrToken)
	}
	return &Claims{Subject: c.Sub, Email: c.Email, EmailVerified: boolish(c.EmailVerified), Name: c.Name}, nil
}

func audienceHas(raw json.RawMessage, id string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == id
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == id {
				return true
			}
		}
	}
	return false
}

// boolish reads email_verified, which some providers send as the string
// "true".
func boolish(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "true"
}

// key returns the provider's key named kid, refreshing the set when it is
// old or the kid is unknown (a rotation), at most once per call.
func (p *Provider) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[kid]; ok && time.Since(p.fetched) < keysTTL {
		return k, nil
	}
	if err := p.refreshKeys(ctx); err != nil {
		return nil, err
	}
	k, ok := p.keys[kid]
	if !ok {
		return nil, fmt.Errorf("%w: signed with an unknown key", ErrToken)
	}
	return k, nil
}

func (p *Provider) refreshKeys(ctx context.Context) error {
	var set struct {
		Keys []struct {
			Kty, Kid, Use, Alg, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := p.getJSON(ctx, p.jwksURI, &set); err != nil {
		return fmt.Errorf("oidc: keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) < 256 {
				continue // smaller than 2048 bits is refused
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			if len(x) != 32 || len(y) != 32 {
				continue
			}
			// The uncompressed point encoding; the parser checks it is on the curve.
			pk, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
			if err != nil {
				continue
			}
			keys[k.Kid] = pk
		}
	}
	if len(keys) == 0 {
		return errors.New("oidc: the provider published no usable signing key")
	}
	p.keys, p.fetched = keys, time.Now()
	return nil
}

func (p *Provider) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", u, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(v)
}
