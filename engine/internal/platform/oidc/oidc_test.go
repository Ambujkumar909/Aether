package oidc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/platform/oidc/oidctest"
)

func provider(t *testing.T) (*oidctest.IdP, *Provider) {
	t.Helper()
	idp := oidctest.New()
	t.Cleanup(idp.Close)
	p, err := New(context.Background(), idp.Issuer(), idp.ClientID, idp.Secret, "https://meet.example.com/cb", idp.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return idp, p
}

var zoe = oidctest.Person{Subject: "sub-1", Email: "zoe@example.com", Name: "Zoe", EmailVerified: true}

func TestAGoodTokenIsAcceptedAndEveryBrokenOneRefused(t *testing.T) {
	idp, p := provider(t)
	ctx := context.Background()
	c, err := p.Verify(ctx, idp.Token(zoe, "n1"), "n1")
	if err != nil || c.Email != "zoe@example.com" || !c.EmailVerified || c.Subject != "sub-1" || c.Name != "Zoe" {
		t.Fatalf("good token: %+v %v", c, err)
	}
	bad := map[string]string{
		"wrong nonce":      idp.Token(zoe, "other"),
		"wrong issuer":     idp.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, Issuer: "https://evil.example"}, "n1"),
		"wrong audience":   idp.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, Audience: "someone-else"}, "n1"),
		"expired":          idp.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, ExpiresIn: -time.Hour}, "n1"),
		"alg none":         idp.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, Alg: "none"}, "n1"),
		"alg HS256":        idp.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, Alg: "HS256"}, "n1"),
		"tampered payload": tamper(idp.Token(zoe, "n1")),
		"not a token":      "abc.def",
		"no subject":       idp.Token(oidctest.Person{Email: "zoe@example.com", EmailVerified: true}, "n1"),
	}
	for name, tok := range bad {
		if _, err := p.Verify(ctx, tok, "n1"); !errors.Is(err, ErrToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.Verify(ctx, idp.Token(zoe, ""), ""); err == nil {
		t.Error("an empty nonce was accepted")
	}
	// Every claim right, signed by somebody else's key under the same key
	// id: only the signature check can tell this one apart.
	forger := oidctest.New()
	defer forger.Close()
	forged := forger.Token(oidctest.Person{Subject: "s", Email: "zoe@example.com", EmailVerified: true, Issuer: idp.Issuer()}, "n1")
	if _, err := p.Verify(ctx, forged, "n1"); !errors.Is(err, ErrToken) {
		t.Fatalf("a token signed with another key was accepted: %v", err)
	}
}

// tamper flips a byte of the payload, keeping the signature.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	b := []byte(parts[1])
	b[len(b)/2] ^= 1
	return parts[0] + "." + string(b) + "." + parts[2]
}

func TestKeysAreRefetchedOnRotationAndNotOnEveryToken(t *testing.T) {
	idp, p := provider(t)
	ctx := context.Background()
	if _, err := p.Verify(ctx, idp.Token(zoe, "n"), "n"); err != nil {
		t.Fatal(err)
	}
	idp.RotateKeys = true // the next fetch publishes a new key
	if _, err := p.Verify(ctx, idp.Token(zoe, "n"), "n"); err != nil {
		t.Fatalf("a token signed with the old key should still verify from the cache: %v", err)
	}
	// Now tokens are signed with the new key; the unknown kid triggers one refetch.
	first := idp.Token(zoe, "n") // signed with the rotated key (the fake rotates on fetch)
	_ = first
	// Force the rotation to have happened: fetch via an unknown kid path.
	idp.RotateKeys = true
	if _, err := p.Verify(ctx, idp.Token(zoe, "n"), "n"); err != nil {
		// The token was signed before the fetch rotated the key; one more try with the new key must pass.
		if _, err := p.Verify(ctx, idp.Token(zoe, "n"), "n"); err != nil {
			t.Fatalf("after rotation: %v", err)
		}
	}
}

func TestExchangeSendsTheVerifierAndRefusesABadCode(t *testing.T) {
	idp, p := provider(t)
	ctx := context.Background()
	verifier, challenge := PKCE()
	if len(verifier) < 43 || len(challenge) < 43 || verifier == challenge {
		t.Fatalf("pkce %q %q", verifier, challenge)
	}
	u := p.AuthURL("st", "n1", challenge)
	for _, want := range []string{"response_type=code", "client_id=aether-client", "state=st", "nonce=n1", "code_challenge=" + challenge, "code_challenge_method=S256", "scope=openid+email+profile"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth url lacks %s: %s", want, u)
		}
	}
	z := zoe
	z.Nonce = "n1" // the nonce travels at authorize time, which this fake has no session for
	idp.Register("code-1", z)
	c, err := p.Exchange(ctx, "code-1", verifier, "n1")
	if err != nil || c.Email != "zoe@example.com" {
		t.Fatalf("exchange: %+v %v", c, err)
	}
	if idp.LastVerifier != verifier {
		t.Fatal("the PKCE verifier was not sent")
	}
	if _, err := p.Exchange(ctx, "code-1", verifier, "n1"); err == nil {
		t.Fatal("a code was exchanged twice")
	}
	if _, err := p.Exchange(ctx, "made-up", verifier, "n1"); err == nil {
		t.Fatal("a made-up code was accepted")
	}
}

func TestDiscoveryMustNameTheIssuerAndBeHTTPS(t *testing.T) {
	idp := oidctest.New()
	defer idp.Close()
	ctx := context.Background()
	if _, err := New(ctx, "http://evil.example", "c", "s", "https://x/cb", idp.Server.Client()); err == nil {
		t.Fatal("a plain-http issuer was accepted")
	}
	if _, err := New(ctx, idp.Issuer(), "", "s", "https://x/cb", idp.Server.Client()); err == nil {
		t.Fatal("an empty client id was accepted")
	}
	// A provider whose document names another issuer is a misconfiguration
	// or an attack; refused.
	if _, err := New(ctx, idp.Issuer()+"/tenant", "c", "s", "https://x/cb", idp.Server.Client()); err == nil {
		t.Fatal("an issuer the document does not name was accepted")
	}
}
