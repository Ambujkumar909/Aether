package identity

import (
	"context"
	"encoding/base32"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/platform/oidc"
	"github.com/aether-rtc/aether/internal/platform/oidc/oidctest"
)

// ssoHandler is a handler with the fake provider configured and an allowed
// domain, plus a client that keeps cookies and does not follow redirects.
func ssoHandler(t *testing.T, domains []string) (*Service, *Handler, *oidctest.IdP, *httptest.Server, *http.Client) {
	t.Helper()
	s, _ := testService(t)
	s.Sealer = newSealer(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	h.ssoLimit = httpx.NewLimiter(1000, 1000) // these tests sign in many times from one address
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	idp := oidctest.New()
	t.Cleanup(idp.Close)
	p, err := oidc.New(context.Background(), idp.Issuer(), idp.ClientID, idp.Secret, srv.URL+"/api/identity/sso/callback", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.SSO = &SSO{Provider: p, Name: "Example Org", Domains: domains}
	jar := &cookieJar{}
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return s, h, idp, srv, c
}

// start performs the start redirect and returns the state the provider will
// be given (from the Location header).
func ssoBegin(t *testing.T, c *http.Client, srv *httptest.Server, next string) (state, nonce string) {
	t.Helper()
	u := srv.URL + "/api/identity/sso/start"
	if next != "" {
		u += "?next=" + url.QueryEscape(next)
	}
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("start: %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), srv.URL) && !strings.Contains(loc.Host, "127.0.0.1") {
		t.Fatalf("redirected somewhere odd: %s", loc)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("auth url: %s", loc)
	}
	return q.Get("state"), q.Get("nonce")
}

func ssoReturn(t *testing.T, c *http.Client, srv *httptest.Server, code, state string) *http.Response {
	t.Helper()
	res, err := c.Get(srv.URL + "/api/identity/sso/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func sessionCookie(res *http.Response) string {
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func TestSingleSignOnCreatesTheMemberAndSignsThemIn(t *testing.T) {
	s, _, idp, srv, c := ssoHandler(t, []string{"example.com"})
	ctx := context.Background()
	state, nonce := ssoBegin(t, c, srv, "/?room=standup")
	idp.Register("code-1", oidctest.Person{Subject: "sub-zoe", Email: "Zoe@Example.com", Name: "Zoe Q", EmailVerified: true, Nonce: nonce})
	res := ssoReturn(t, c, srv, "code-1", state)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?room=standup" {
		t.Fatalf("callback: %d -> %s", res.StatusCode, res.Header.Get("Location"))
	}
	tok := sessionCookie(res)
	if tok == "" {
		t.Fatal("no session cookie")
	}
	u, _, err := s.Authorize(ctx, tok)
	if err != nil || u.Email != "zoe@example.com" || u.DisplayName != "Zoe Q" || u.Role != RoleMember {
		t.Fatalf("session: %+v %v", u, err)
	}
	// Coming back: the same account, not a second one.
	state, nonce = ssoBegin(t, c, srv, "")
	idp.Register("code-2", oidctest.Person{Subject: "sub-zoe", Email: "zoe@example.com", Name: "Zoe Q", EmailVerified: true, Nonce: nonce})
	res = ssoReturn(t, c, srv, "code-2", state)
	u2, _, err := s.Authorize(ctx, sessionCookie(res))
	if err != nil || u2.ID != u.ID {
		t.Fatalf("second sign-in: %+v %v", u2, err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("%d accounts, want 1", n)
	}
	// The state cookie is spent: the same callback again fails.
	res = ssoReturn(t, c, srv, "code-2", state)
	if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso=") {
		t.Fatalf("a replayed callback: %d -> %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestSingleSignOnRefusesWhatItMust(t *testing.T) {
	s, _, idp, srv, c := ssoHandler(t, []string{"example.com"})
	ctx := context.Background()
	cases := []struct {
		name   string
		person oidctest.Person
		reason string
	}{
		{"unverified address", oidctest.Person{Subject: "a", Email: "a@example.com", EmailVerified: false}, "refused"},
		{"domain not allowed", oidctest.Person{Subject: "b", Email: "b@elsewhere.com", EmailVerified: true}, "refused"},
		{"no address", oidctest.Person{Subject: "c", EmailVerified: true}, "refused"},
		{"wrong audience", oidctest.Person{Subject: "d", Email: "d@example.com", EmailVerified: true, Audience: "other"}, "token"},
	}
	for i, tc := range cases {
		state, nonce := ssoBegin(t, c, srv, "")
		if tc.person.Nonce == "" {
			tc.person.Nonce = nonce
		}
		code := "c" + string(rune('0'+i))
		idp.Register(code, tc.person)
		res := ssoReturn(t, c, srv, code, state)
		if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso="+tc.reason) {
			t.Errorf("%s: %d -> %s", tc.name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("%d accounts were created by refused sign-ins", n)
	}
	// A wrong state (another browser's, or forged).
	state, nonce := ssoBegin(t, c, srv, "")
	idp.Register("c9", oidctest.Person{Subject: "e", Email: "e@example.com", EmailVerified: true, Nonce: nonce})
	res := ssoReturn(t, c, srv, "c9", state+"x")
	if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso=state") {
		t.Fatalf("wrong state: %s", res.Header.Get("Location"))
	}
	// A wrong nonce in the token (a token meant for another sign-in).
	state, _ = ssoBegin(t, c, srv, "")
	idp.Register("c8", oidctest.Person{Subject: "f", Email: "f@example.com", EmailVerified: true, Nonce: "stale"})
	res = ssoReturn(t, c, srv, "c8", state)
	if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso=token") {
		t.Fatalf("wrong nonce: %s", res.Header.Get("Location"))
	}
	// No callback without a start.
	fresh := &http.Client{Jar: &cookieJar{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res = ssoReturn(t, fresh, srv, "c8", state)
	if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso=expired") {
		t.Fatalf("callback without start: %s", res.Header.Get("Location"))
	}
}

func TestWithoutAnyAllowedDomainSingleSignOnFailsClosed(t *testing.T) {
	s, _, idp, srv, c := ssoHandler(t, nil) // and no org domains either
	state, nonce := ssoBegin(t, c, srv, "")
	idp.Register("c1", oidctest.Person{Subject: "a", Email: "a@example.com", EmailVerified: true, Nonce: nonce})
	res := ssoReturn(t, c, srv, "c1", state)
	if sessionCookie(res) != "" {
		t.Fatal("signed in with no allowed domain configured")
	}
	// With the organisation's domains set, it opens for those.
	s.OrgDomains = func(context.Context) []string { return []string{"example.com"} }
	state, nonce = ssoBegin(t, c, srv, "")
	idp.Register("c2", oidctest.Person{Subject: "a", Email: "a@example.com", EmailVerified: true, Nonce: nonce})
	if res := ssoReturn(t, c, srv, "c2", state); sessionCookie(res) == "" {
		t.Fatalf("org domain refused: %s", res.Header.Get("Location"))
	}
}

func TestSingleSignOnSkipsTheLocalSecondStepButNotSuspension(t *testing.T) {
	s, _, idp, srv, c := ssoHandler(t, []string{"example.com"})
	ctx := context.Background()
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	b32, _, _, _ := s.BeginTwoStep(ctx, u, "correct-horse-battery")
	secret, _ := b32decode(b32)
	if _, err := s.EnableTwoStep(ctx, u, nil, codeAt(secret, s.clock()), "", ""); err != nil {
		t.Fatal(err)
	}
	state, nonce := ssoBegin(t, c, srv, "")
	idp.Register("c1", oidctest.Person{Subject: "zoe", Email: "zoe@example.com", EmailVerified: true, Nonce: nonce})
	res := ssoReturn(t, c, srv, "c1", state)
	if sessionCookie(res) == "" {
		t.Fatalf("the provider's sign-in was asked for the local second step: %s", res.Header.Get("Location"))
	}
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	if _, err := s.SetStatus(ctx, admin, u.ID, StatusSuspended, "", ""); err != nil {
		t.Fatal(err)
	}
	state, nonce = ssoBegin(t, c, srv, "")
	idp.Register("c2", oidctest.Person{Subject: "zoe", Email: "zoe@example.com", EmailVerified: true, Nonce: nonce})
	res = ssoReturn(t, c, srv, "c2", state)
	if sessionCookie(res) != "" || !strings.Contains(res.Header.Get("Location"), "#sso=refused") {
		t.Fatalf("a suspended account signed in through the provider: %s", res.Header.Get("Location"))
	}
	_ = audit.ActionLoginFailed
	_ = errors.New
}

func TestTheNextPathIsOnlyEverOnThisOrigin(t *testing.T) {
	_, _, idp, srv, c := ssoHandler(t, []string{"example.com"})
	for _, next := range []string{"https://evil.example/", "//evil.example/x", "/ok\r\nSet-Cookie: a=b", "javascript:alert(1)"} {
		state, nonce := ssoBegin(t, c, srv, next)
		idp.Register("c", oidctest.Person{Subject: "a", Email: "a@example.com", EmailVerified: true, Nonce: nonce})
		res := ssoReturn(t, c, srv, "c", state)
		if res.Header.Get("Location") != "/" {
			t.Errorf("next=%q landed on %q", next, res.Header.Get("Location"))
		}
	}
}

func TestSingleSignOnIsHiddenWhenNotConfigured(t *testing.T) {
	s, _ := testService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/identity/sso/start")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("start without a provider: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/api/identity/config")
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"sso":null`) {
		t.Fatalf("config advertises sso: %s", b)
	}
}

func b32decode(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// cookieJar keeps cookies by name, ignoring domain and path: the tests run
// against one server.
type cookieJar struct{ c map[string]*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	if j.c == nil {
		j.c = map[string]*http.Cookie{}
	}
	for _, c := range cs {
		if c.MaxAge < 0 || c.Value == "" {
			delete(j.c, c.Name)
		} else {
			j.c[c.Name] = c
		}
	}
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(j.c))
	for _, c := range j.c {
		out = append(out, c)
	}
	return out
}
