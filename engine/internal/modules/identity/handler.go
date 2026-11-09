package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Cookie names. The session cookie is HttpOnly so that a cross-site script
// cannot read it; the CSRF cookie deliberately is not, because the page needs
// to read it in order to echo it back in a header.
const (
	SessionCookie = "aether_session"
	CSRFCookie    = "aether_csrf"
	CSRFHeader    = "X-Aether-CSRF"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

// Handler exposes identity over HTTP.
type Handler struct {
	svc        *Service
	log        *slog.Logger
	Secure     bool // set the Secure cookie attribute
	TrustProxy bool

	loginLimit  *httpx.Limiter
	signupLimit *httpx.Limiter
	resetLimit  *httpx.Limiter
	ssoLimit    *httpx.Limiter
	apiLimit    *httpx.Limiter

	// AllowSignup gates self-registration. Whether strangers may create
	// accounts on a meeting server is a deployment decision, not a default,
	// so it is a switch the operator sets rather than something buried here.
	AllowSignup bool

	// ICEServers, if set, returns the STUN/TURN servers a client should use,
	// with a freshly minted short-lived credential each call. Left nil when no
	// relay is running, in which case clients fall back to direct connection
	// only. A func rather than a value because the credential expires.
	ICEServers func() []any

	// SSO is the organisation's identity provider, when one is configured
	// (sso.go); nil hides the button and the endpoints.
	SSO *SSO
}

func NewHandler(svc *Service, log *slog.Logger, secure, trustProxy bool) *Handler {
	return &Handler{
		svc: svc, log: log, Secure: secure, TrustProxy: trustProxy,
		// Five attempts a minute per source, bursting to ten. Generous for a
		// person who has forgotten which password they used; ruinous for a
		// script.
		loginLimit: httpx.NewLimiter(5.0/60.0, 10),
		// Registration is far rarer than sign-in and far more attractive to
		// abuse, so it gets a much tighter budget: three an hour, bursting
		// to three.
		signupLimit: httpx.NewLimiter(3.0/3600.0, 3),
		// Reset emails: five an hour per source. Each account is also sent
		// at most one a minute (ResetEvery), whoever asks.
		resetLimit: httpx.NewLimiter(5.0/3600.0, 5),
		// Single sign-on: one a second, bursting to sixty. Not the password
		// limit -- there is no password to guess here, the provider does the
		// checking -- and an office behind one address signs everybody in
		// at nine o'clock.
		ssoLimit: httpx.NewLimiter(1, 60),
		// 20 a second sustained, 60 in a burst. A page opening fires a
		// handful at once, which the burst absorbs; a script cannot sustain
		// more than twenty without being told to wait.
		apiLimit: httpx.NewLimiter(20, 60),
	}
}

// Routes registers this module's endpoints. Everything is namespaced under
// /api/identity and nothing else in the process registers a route here.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("POST /api/identity/login",
		httpx.RateLimit(h.loginLimit, h.limitKey, http.HandlerFunc(h.login)))
	mux.Handle("POST /api/identity/signup",
		httpx.RateLimit(h.signupLimit, h.limitKey, http.HandlerFunc(h.signup)))
	mux.Handle("GET /api/identity/sso/start",
		httpx.RateLimit(h.ssoLimit, h.limitKey, http.HandlerFunc(h.ssoStart)))
	mux.Handle("GET /api/identity/sso/callback",
		httpx.RateLimit(h.ssoLimit, h.limitKey, http.HandlerFunc(h.ssoCallback)))
	mux.Handle("POST /api/identity/login/second",
		httpx.RateLimit(h.loginLimit, h.limitKey, http.HandlerFunc(h.secondStep)))
	mux.Handle("POST /api/identity/two-step/setup",
		h.RequireAuth(h.csrf(http.HandlerFunc(h.twoStepSetup))))
	mux.Handle("POST /api/identity/two-step/enable",
		h.RequireAuth(h.csrf(http.HandlerFunc(h.twoStepEnable))))
	mux.Handle("POST /api/identity/two-step/disable",
		h.RequireAuth(h.csrf(http.HandlerFunc(h.twoStepDisable))))
	mux.Handle("DELETE /api/identity/users/{id}/two-step",
		h.RequirePermission(PermUserWrite, h.csrf(http.HandlerFunc(h.twoStepReset))))
	mux.Handle("POST /api/identity/signup/verify",
		httpx.RateLimit(h.loginLimit, h.limitKey, http.HandlerFunc(h.completeSignup)))
	mux.HandleFunc("GET /api/identity/config", h.config)
	mux.Handle("POST /api/identity/password/forgot",
		httpx.RateLimit(h.resetLimit, h.limitKey, http.HandlerFunc(h.forgotPassword)))
	mux.Handle("POST /api/identity/password/reset",
		httpx.RateLimit(h.loginLimit, h.limitKey, http.HandlerFunc(h.resetPassword)))
	mux.HandleFunc("POST /api/identity/logout", h.logout)
	mux.Handle("GET /api/identity/me", h.RequireAuth(http.HandlerFunc(h.me)))

	mux.Handle("GET /api/identity/directory",
		h.RequirePermission(PermJoinMeeting, http.HandlerFunc(h.directory)))
	mux.Handle("GET /api/identity/users",
		h.RequirePermission(PermUserRead, http.HandlerFunc(h.listUsers)))
	mux.Handle("POST /api/identity/users",
		h.RequirePermission(PermUserWrite, h.csrf(http.HandlerFunc(h.createUser))))
	mux.Handle("PATCH /api/identity/users/{id}/role",
		h.RequirePermission(PermUserWrite, h.csrf(http.HandlerFunc(h.setRole))))
	mux.Handle("PATCH /api/identity/users/{id}/status",
		h.RequirePermission(PermUserWrite, h.csrf(http.HandlerFunc(h.setStatus))))
	mux.Handle("DELETE /api/identity/users/{id}",
		h.RequirePermission(PermUserDelete, h.csrf(http.HandlerFunc(h.deleteUser))))

	mux.Handle("POST /api/identity/password",
		h.RequireAuth(h.csrf(http.HandlerFunc(h.changePassword))))
	mux.Handle("GET /api/identity/sessions",
		h.RequireAuth(http.HandlerFunc(h.listSessions)))
	mux.Handle("DELETE /api/identity/sessions/{id}",
		h.RequireAuth(h.csrf(http.HandlerFunc(h.revokeSession))))
}

func (h *Handler) limitKey(r *http.Request) string {
	return httpx.ClientIP(r, h.TrustProxy)
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// userView is what leaves the process. The password hash is not a field on
// it, so it cannot be leaked by adding a handler that forgets to strip it.
type userView struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Role        Role       `json:"role"`
	Status      Status     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
	Locked      bool       `json:"locked"`
}

func view(u *User) userView {
	return userView{
		ID: u.ID, Email: u.Email, DisplayName: u.DisplayName,
		Role: u.Role, Status: u.Status, CreatedAt: u.CreatedAt,
		LastLoginAt: u.LastLoginAt, Locked: u.Locked(time.Now().UTC()),
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip := httpx.ClientIP(r, h.TrustProxy)
	ua := r.UserAgent()

	u, token, ticket, err := h.svc.Login(r.Context(), req.Email, req.Password, ip, ua)
	if errors.Is(err, ErrInvalidCredential) {
		// One message for every refusal. See Service.checkPassword.
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials",
			"email or password is incorrect")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if ticket != "" {
		// The password was right; a code from the authenticator app (or a
		// recovery code) is still owed, within TicketTTL. No session yet.
		httpx.JSON(w, http.StatusOK, map[string]any{"secondStep": "code", "ticket": ticket})
		return
	}

	csrf := h.setSession(w, token)
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user":        view(u),
		"permissions": u.Role.Permissions(),
		"csrf":        csrf,
	})
}

// config tells the sign-in page which options to offer, so it never renders
// a "Create an account" link on a server that will refuse.
func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"allowSignup":       h.AllowSignup,
		"minPasswordLength": MinPasswordLength,
		"passwordReset":     h.svc.SendReset != nil,
		"verifySignup":      h.svc.SendVerify != nil,
		"twoStep":           h.svc.Sealer != nil,
		"sso":               h.ssoConfig(),
	}
	// The relay's URLs and a fresh credential, when a relay is running. This
	// is why the client fetches config on join rather than hard-coding ICE
	// servers: the credential is short-lived and minted here.
	//
	// Only for a signed-in session. This page is public (the sign-in screen
	// reads allowSignup from it), and it once handed a relay credential to
	// anyone who asked -- with a relay that forwarded anywhere, which made the
	// server an open relay into its own network. Joining a meeting needs a
	// session anyway, so nobody who may use the relay is turned away.
	if h.ICEServers != nil {
		if _, _, err := h.svc.Authorize(r.Context(), h.sessionToken(r)); err == nil {
			body["iceServers"] = h.ICEServers()
		}
	}
	httpx.JSON(w, http.StatusOK, body)
}

// ssoConfig is what the sign-in screen needs to offer the provider: a name
// for the button, nothing about the provider itself.
func (h *Handler) ssoConfig() any {
	if h.SSO == nil || h.svc.Sealer == nil {
		return nil
	}
	return map[string]string{"name": h.SSO.Name}
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	if !h.AllowSignup {
		httpx.Error(w, http.StatusForbidden, "signup_closed",
			"this server does not accept new accounts")
		return
	}
	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip := httpx.ClientIP(r, h.TrustProxy)
	ua := r.UserAgent()
	if h.svc.SendVerify != nil {
		// A new address and a known one answer alike, after the same time.
		defer evenOut(time.Now(), signupAnswerAfter)
	}

	u, err := h.svc.SignUp(r.Context(), req.Email, req.DisplayName, req.Password, ip, ua)
	if err != nil {
		if errors.Is(err, ErrSignupPending) && h.svc.SendVerify != nil {
			httpx.JSON(w, http.StatusAccepted, map[string]any{
				"status": "Check your email: the link there finishes creating your account.",
				"verify": true,
			})
			return
		}
		if errors.Is(err, ErrSignupPending) {
			// The address is taken, and we are not going to say so. The
			// response is the same shape a success would produce minus the
			// session, so timing and content give nothing away.
			httpx.JSON(w, http.StatusAccepted, map[string]any{
				"status": "If that address is available, the account has been created. Try signing in.",
			})
			return
		}
		h.fail(w, err)
		return
	}

	// Sign the new account straight in. Making somebody type the password
	// they just chose, into a second form, is friction that protects nobody.
	token, err := h.svc.StartSession(r.Context(), u, ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	csrf := h.setSession(w, token)
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"user":        view(u),
		"permissions": u.Role.Permissions(),
		"csrf":        csrf,
	})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token := h.sessionToken(r)
	if token != "" {
		if u, sess, err := h.svc.Authorize(r.Context(), token); err == nil {
			_ = h.svc.Logout(r.Context(), u, sess,
				httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
		}
	}
	h.clearSession(w)
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	sess := SessionFrom(r.Context())
	on, left := h.svc.TwoStepStatus(r.Context(), u.ID)
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user":        view(u),
		"permissions": u.Role.Permissions(),
		"session": map[string]any{
			"id": sess.ID, "expiresAt": sess.ExpiresAt, "ip": sess.IP,
		},
		"twoStep": map[string]any{"available": h.svc.Sealer != nil, "enabled": on, "recoveryCodesLeft": left},
	})
}

func (h *Handler) directory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiOr(q.Get("limit"), 20)
	entries, err := h.svc.SearchDirectory(r.Context(), UserFrom(r.Context()),
		strings.TrimSpace(q.Get("q")), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if entries == nil {
		entries = []DirectoryEntry{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"people": entries})
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	q := r.URL.Query()
	f := UserFilter{
		Search: strings.TrimSpace(q.Get("q")),
		Role:   Role(q.Get("role")),
		Status: Status(q.Get("status")),
		Limit:  atoiOr(q.Get("limit"), 50),
		Offset: atoiOr(q.Get("offset"), 0),
	}
	users, total, err := h.svc.ListUsers(r.Context(), actor, f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, view(u))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"users": out, "total": total,
		"limit": f.Limit, "offset": f.Offset,
	})
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		Role        Role   `json:"role"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if req.Role == "" {
		req.Role = RoleMember
	}
	u, err := h.svc.CreateUser(r.Context(), actor, req.Email, req.DisplayName,
		req.Password, req.Role, httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, view(u))
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var req struct {
		Role Role `json:"role"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	u, err := h.svc.SetRole(r.Context(), actor, r.PathValue("id"), req.Role,
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, view(u))
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var req struct {
		Status Status `json:"status"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if req.Status != StatusActive && req.Status != StatusSuspended {
		httpx.Error(w, http.StatusBadRequest, "bad_status", "unknown status")
		return
	}
	u, err := h.svc.SetStatus(r.Context(), actor, r.PathValue("id"), req.Status,
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, view(u))
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	err := h.svc.DeleteUser(r.Context(), actor, r.PathValue("id"),
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var req struct {
		UserID  string `json:"userId"`
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	target := req.UserID
	if target == "" {
		target = actor.ID
	}
	err := h.svc.ChangePassword(r.Context(), actor, target, req.Current, req.Next,
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	// Every session died, including this one.
	h.clearSession(w)
	httpx.JSON(w, http.StatusOK, map[string]string{
		"status": "password changed, please sign in again",
	})
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	target := r.URL.Query().Get("userId")
	if target == "" {
		target = actor.ID
	}
	sessions, err := h.svc.Sessions(r.Context(), actor, target)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	target := r.URL.Query().Get("userId")
	if target == "" {
		target = actor.ID
	}
	err := h.svc.RevokeSession(r.Context(), actor, target, r.PathValue("id"),
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// ---------------------------------------------------------------------------
// Cookies and CSRF
// ---------------------------------------------------------------------------

func (h *Handler) setSession(w http.ResponseWriter, token string) string {
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly is the difference between "a cross-site script can steal
		// your session" and "it cannot". Storing tokens in localStorage,
		// which is the common alternative, forfeits this entirely.
		HttpOnly: true,
		Secure:   h.Secure,
		// Lax rather than Strict: Strict breaks following a meeting link
		// from an email or a chat client, which is how meetings are joined.
		// Lax still blocks the cross-site POST that CSRF depends on.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionLifetime.Seconds()),
	})

	csrf := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name:  CSRFCookie,
		Value: csrf,
		Path:  "/",
		// Readable by script on purpose: the page must echo it in a header.
		// That is the whole mechanism -- a cross-origin attacker can cause
		// the browser to SEND our cookies but cannot READ them, so it cannot
		// produce the matching header.
		HttpOnly: false,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionLifetime.Seconds()),
	})
	return csrf
}

func (h *Handler) clearSession(w http.ResponseWriter) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/",
			HttpOnly: name == SessionCookie,
			Secure:   h.Secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

func (h *Handler) sessionToken(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// csrf enforces the double-submit check on state-changing requests.
// Throttle bounds how fast one signed-in account can call the API.
//
// Rate limiting was previously only on login and signup, which covers the
// unauthenticated surface and nothing else. Everything past the door was
// unmetered: one account, or one stolen session, could hold a connection open
// and issue requests as fast as the network allowed -- creating meetings,
// opening conversations, sending messages -- until the database or the
// process gave out. That is a denial of service that needs no privilege
// beyond an ordinary member account.
//
// Keyed by account rather than by address, because address is the wrong unit
// twice over: everybody behind one office NAT shares it, and anybody with a
// handful of addresses evades it. The account is the thing being abused and
// the thing that can be suspended afterwards.
//
// The ceiling is deliberately generous. A real client makes a handful of
// requests when a page opens and then goes quiet, because the chatty parts of
// this product -- viewport reports, presence, speaker energy -- ride the
// WebSocket and never touch this path at all. Anything approaching this rate
// is a script.
func (h *Handler) Throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.apiLimit == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := ""
		if u := UserFrom(r.Context()); u != nil {
			key = u.ID
		}
		if key == "" {
			key = "ip:" + httpx.ClientIP(r, h.TrustProxy)
		}
		if !h.apiLimit.Allow(key) {
			// A second is how long the bucket takes to hold another request
			// at the sustained rate; a client that honours this never sees
			// two 429s in a row.
			w.Header().Set("Retry-After", "1")
			httpx.Error(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests; slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CSRF is exported so the shared Guard can apply it to every protected route.
func (h *Handler) CSRF(next http.Handler) http.Handler { return h.csrf(next) }

func (h *Handler) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(CSRFCookie)
		header := r.Header.Get(CSRFHeader)
		if err != nil || c.Value == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(header)) != 1 {
			httpx.Error(w, http.StatusForbidden, "csrf",
				"missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// RequireAuth rejects unauthenticated requests and puts the user in context.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, sess, err := h.svc.Authorize(r.Context(), h.sessionToken(r))
		if err != nil {
			h.clearSession(w)
			httpx.Error(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, u)
		ctx = context.WithValue(ctx, ctxSession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequirePermission additionally checks authorisation.
//
// Every permission is checked here, server-side, on every request. The
// client's copy of its permission list exists only so the interface can hide
// buttons that would fail, and is never trusted for anything.
func (h *Handler) RequirePermission(p Permission, next http.Handler) http.Handler {
	return h.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if !u.Role.Can(p) {
			httpx.Error(w, http.StatusForbidden, "forbidden",
				"your role does not permit this")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// UserFrom reads the authenticated user.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxUser).(*User)
	return u
}

// SessionFrom reads the active session.
func SessionFrom(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxSession).(*Session)
	return s
}

// fail maps domain errors onto status codes.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "your role does not permit this")
	case errors.Is(err, ErrEmailTaken):
		httpx.Error(w, http.StatusConflict, "email_taken", "that email is already registered")
	case errors.Is(err, ErrSignupOrgDomain):
		httpx.Error(w, http.StatusForbidden, "signup_org_domain",
			"accounts at this address's domain are created by an administrator")
	case errors.Is(err, ErrSignupClosed):
		httpx.Error(w, http.StatusForbidden, "signup_closed",
			"this server does not accept new accounts")
	case errors.Is(err, ErrWeakPassword):
		httpx.Error(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, ErrInvalidEmail):
		httpx.Error(w, http.StatusBadRequest, "invalid_email", "that is not a valid email address")
	case errors.Is(err, ErrInvalidRole):
		httpx.Error(w, http.StatusBadRequest, "invalid_role", "unknown role")
	case errors.Is(err, ErrSelfDemotion):
		httpx.Error(w, http.StatusConflict, "self_demotion",
			"you cannot change your own role; ask another administrator")
	case errors.Is(err, ErrLastAdmin):
		httpx.Error(w, http.StatusConflict, "last_admin",
			"this is the last active administrator; promote someone else first")
	case errors.Is(err, ErrInvalidCredential):
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", "incorrect credentials")
	default:
		h.log.Error("identity request failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}
