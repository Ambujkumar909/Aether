package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/platform/oidc"
)

// Single sign-on through the organisation's identity provider (OpenID
// Connect, internal/platform/oidc).
//
// The browser is sent to the provider with a state, a nonce and a PKCE
// challenge that are kept, sealed, in a short-lived cookie; the provider
// sends it back with a code; the code is exchanged for an identity token,
// which is verified against the provider's keys and this sign-in's nonce.
// The person is then the account with that email -- only an address the
// provider marks verified, and only in a domain the deployment allows
// (-sso-domains, or the organisation's own domains): fail closed, because a
// provider that signs in anybody would otherwise create accounts for
// anybody. An account that does not exist yet is created as a member. A
// provider-authenticated sign-in does not ask for the local second step:
// the provider is the organisation's authority on who this is.

// SSO is the configured provider, or nil.
type SSO struct {
	Provider *oidc.Provider
	Name     string // shown on the button: "Sign in with <Name>"
	// Domains lists the email domains that may sign in this way; empty
	// falls back to the organisation's domains, and none at all refuses.
	Domains []string
}

const ssoCookie = "aether_sso"

var (
	ErrSSOUnavailable = errors.New("identity: single sign-on is not configured")
	ErrSSORefused     = errors.New("identity: single sign-on refused")
)

type ssoState struct {
	State, Nonce, Verifier, Next string
}

// ssoAllowed reports whether an address may sign in through the provider.
func (s *Service) ssoAllowed(ctx context.Context, email string, configured []string) bool {
	domains := configured
	if len(domains) == 0 && s.OrgDomains != nil {
		domains = s.OrgDomains(ctx)
	}
	d := domainOf(email)
	for _, a := range domains {
		if strings.EqualFold(strings.TrimPrefix(a, "@"), d) {
			return true
		}
	}
	return false
}

// SSOLogin signs in the account for a verified identity, creating a member
// when none exists, and returns a session.
func (s *Service) SSOLogin(ctx context.Context, c *oidc.Claims, domains []string, issuer, ip, ua string) (*User, string, error) {
	refuse := func(subject, detail string) (*User, string, error) {
		s.record(ctx, audit.Event{Action: audit.ActionLoginFailed, Outcome: audit.OutcomeDenied,
			Subject: subject, IP: ip, UserAgent: ua, Detail: "sso: " + detail})
		return nil, "", ErrSSORefused
	}
	if !c.EmailVerified || c.Email == "" {
		return refuse(c.Email, "the provider did not mark the address verified")
	}
	norm, err := NormalizeEmail(c.Email)
	if err != nil {
		return refuse(c.Email, "malformed address from the provider")
	}
	if !s.ssoAllowed(ctx, norm, domains) {
		return refuse(norm, "domain not allowed for single sign-on")
	}
	u, err := s.repo.UserByEmail(ctx, norm)
	if errors.Is(err, ErrNotFound) {
		// A password nobody knows: the account signs in through the
		// provider; "Forgot your password?" sets a local one if ever wanted.
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", err
		}
		hash, err := HashPassword(base64.RawURLEncoding.EncodeToString(raw))
		if err != nil {
			return nil, "", err
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = norm
		}
		if len(name) > 120 {
			name = name[:120]
		}
		now := time.Now().UTC()
		u = &User{ID: newID(), Email: norm, DisplayName: name, Role: RoleMember, Status: StatusActive,
			CreatedAt: now, UpdatedAt: now, passwordHash: hash}
		if err := s.repo.CreateUser(ctx, u); err != nil {
			if !errors.Is(err, ErrEmailTaken) {
				return nil, "", err
			}
			if u, err = s.repo.UserByEmail(ctx, norm); err != nil { // made meanwhile
				return nil, "", err
			}
		} else {
			s.record(ctx, audit.Event{Action: audit.ActionUserCreated, Outcome: audit.OutcomeSuccess,
				ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email, IP: ip, UserAgent: ua,
				Detail: "created by single sign-on from " + issuer})
		}
	} else if err != nil {
		return nil, "", err
	}
	if u.Status == StatusSuspended {
		return refuse(norm, "account suspended")
	}
	token, err := s.finishLogin(ctx, u, ip, ua, "sso: "+issuer+" subject "+c.Subject)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// ssoStart sends the browser to the provider. ?next= is where to land
// afterwards: a path on this origin, nothing else.
func (h *Handler) ssoStart(w http.ResponseWriter, r *http.Request) {
	if h.SSO == nil || h.svc.Sealer == nil {
		httpx.Error(w, http.StatusNotFound, "sso_unavailable", "single sign-on is not available on this server")
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || len(next) > 512 {
		next = "/"
	}
	verifier, challenge := oidc.PKCE()
	st := ssoState{State: oidc.Random(), Nonce: oidc.Random(), Verifier: verifier, Next: next}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name: ssoCookie, Value: base64.RawURLEncoding.EncodeToString(h.svc.Sealer.Seal(raw, []byte("sso"))),
		Path: "/api/identity/sso/", HttpOnly: true, Secure: h.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, h.SSO.Provider.AuthURL(st.State, st.Nonce, challenge), http.StatusFound)
}

// ssoCallback is where the provider sends the browser back. Every failure
// lands on the sign-in screen with a short reason; the detail is logged.
func (h *Handler) ssoCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason, detail string) {
		h.log.Warn("sso: sign-in failed", "reason", reason, "detail", detail)
		http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/api/identity/sso/", MaxAge: -1, HttpOnly: true, Secure: h.Secure, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/#sso="+reason, http.StatusSeeOther)
	}
	if h.SSO == nil || h.svc.Sealer == nil {
		fail("unavailable", "not configured")
		return
	}
	c, err := r.Cookie(ssoCookie)
	if err != nil {
		fail("expired", "no state cookie: the sign-in took too long, or started elsewhere")
		return
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		fail("expired", "state cookie unreadable")
		return
	}
	raw, err := h.svc.Sealer.Open(sealed, []byte("sso"))
	if err != nil {
		fail("expired", "state cookie does not open")
		return
	}
	var st ssoState
	if err := json.Unmarshal(raw, &st); err != nil {
		fail("expired", "state cookie malformed")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail("denied", "provider said: "+e)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail("state", "state does not match this browser's sign-in")
		return
	}
	code := q.Get("code")
	if code == "" || len(code) > 2048 {
		fail("state", "no code")
		return
	}
	claims, err := h.SSO.Provider.Exchange(r.Context(), code, st.Verifier, st.Nonce)
	if err != nil {
		fail("token", err.Error())
		return
	}
	u, token, err := h.svc.SSOLogin(r.Context(), claims, h.SSO.Domains, h.SSO.Provider.Issuer, httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if errors.Is(err, ErrSSORefused) {
		fail("refused", "see the audit log")
		return
	}
	if err != nil {
		fail("error", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/api/identity/sso/", MaxAge: -1, HttpOnly: true, Secure: h.Secure, SameSite: http.SameSiteLaxMode})
	h.setSession(w, token)
	h.log.Info("sso: signed in", "user", u.ID)
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}
