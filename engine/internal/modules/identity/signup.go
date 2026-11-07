package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Email-verified sign-up.
//
// Open sign-up used to create the account at once, for whatever address was
// typed: a stranger could register alice@your-org.com and appear in the
// organisation's address book as Alice, or claim bob@gmail.com before Bob.
// With outgoing mail configured, a sign-up now creates no account. It is a
// pending row -- address, name, password hash -- keyed by a link token's hash,
// and the account exists only when the link is opened AND the password chosen
// at sign-up is typed again. Both are needed because re-submitting sign-up for
// someone else's address with your own password must not produce a link that
// gives you their account when they click it.
//
// An address that already has an account gets an email saying so (at most one
// a minute) and no row; the caller is told the same words either way, after
// the same time. Without mail, the old behaviour stands, except that
// addresses in the organisation's own domains are refused: nothing could
// prove them, so an administrator creates those.

const (
	SignupTTL = 24 * time.Hour
	// signupAnswerAfter is how long every sign-up takes to answer when
	// verification is on, so a known address and a new one look alike.
	signupAnswerAfter = 600 * time.Millisecond
	// maxPendingPerEmail bounds the rows one address can collect.
	maxPendingPerEmail = 3
)

var (
	ErrSignupOrgDomain = errors.New("identity: addresses in the organisation's domains are created by an administrator")
	ErrSignupLink      = errors.New("identity: sign-up link expired, used, or the password does not match")
)

// pendingSignup is one sign-up waiting for its link.
type pendingSignup struct {
	email, name, passwordHash string
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

func (r *SQLRepository) SavePendingSignup(ctx context.Context, tokenHash, email, name, passwordHash string, created, expires time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity_signups (token_hash, email, display_name, password_hash, created_at, expires_at)
		VALUES (?,?,?,?,?,?)`, tokenHash, email, name, passwordHash, created.UnixMilli(), expires.UnixMilli()); err != nil {
		return fmt.Errorf("save signup: %w", err)
	}
	// Keep only the newest few per address.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity_signups WHERE email = ? AND token_hash NOT IN (
			SELECT token_hash FROM identity_signups WHERE email = ? ORDER BY created_at DESC, token_hash DESC LIMIT ?)`,
		email, email, maxPendingPerEmail); err != nil {
		return fmt.Errorf("trim signups: %w", err)
	}
	return tx.Commit()
}

func (r *SQLRepository) PendingSignup(ctx context.Context, tokenHash string, now time.Time) (*pendingSignup, error) {
	var p pendingSignup
	err := r.db.QueryRowContext(ctx, `
		SELECT email, display_name, password_hash FROM identity_signups WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now.UnixMilli()).Scan(&p.email, &p.name, &p.passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ClearPendingSignups removes every pending row for an address.
func (r *SQLRepository) ClearPendingSignups(ctx context.Context, email string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM identity_signups WHERE email = ?`, email)
	return err
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// noticeThrottle remembers when an address that already has an account was
// last told about a sign-up attempt. Bounded: it never holds more than
// noticeMax addresses.
type noticeThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const noticeMax = 10000

func (n *noticeThrottle) allow(email string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	if t, ok := n.last[email]; ok && now.Sub(t) < ResetEvery {
		return false
	}
	if len(n.last) >= noticeMax {
		for k, t := range n.last {
			if now.Sub(t) >= ResetEvery {
				delete(n.last, k)
			}
		}
		if len(n.last) >= noticeMax {
			return false // full of recent ones: refuse rather than grow
		}
	}
	n.last[email] = now
	return true
}

// inOrgDomain reports whether an address belongs to one of the
// organisation's own domains.
func (s *Service) inOrgDomain(ctx context.Context, email string) bool {
	if s.OrgDomains == nil {
		return false
	}
	d := domainOf(email)
	for _, o := range s.OrgDomains(ctx) {
		if strings.EqualFold(o, d) {
			return true
		}
	}
	return false
}

// verifiedSignup is SignUp when outgoing mail is configured: it never creates
// an account, and returns ErrSignupPending whatever the address.
func (s *Service) verifiedSignup(ctx context.Context, norm, name, password, ip, ua string) error {
	now := time.Now().UTC()
	hash, err := HashPassword(password) // spent either way
	if err != nil {
		return err
	}
	if u, err := s.repo.UserByEmail(ctx, norm); err == nil {
		if s.SendSignupNotice != nil && s.notices.allow(norm, now) {
			s.SendSignupNotice(u.Email)
		}
		s.record(ctx, audit.Event{Action: audit.ActionSignUpBlocked, Outcome: audit.OutcomeDenied,
			Subject: norm, IP: ip, UserAgent: ua, Detail: "address already registered; owner notified"})
		return ErrSignupPending
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.repo.SavePendingSignup(ctx, HashToken(token), norm, name, hash, now, now.Add(SignupTTL)); err != nil {
		return err
	}
	s.SendVerify(norm, token)
	s.record(ctx, audit.Event{Action: audit.ActionSignUp, Outcome: audit.OutcomeFailure,
		Subject: norm, IP: ip, UserAgent: ua, Detail: "pending: verification link sent"})
	return ErrSignupPending
}

// CompleteSignup creates the account a link stands for, if the password is
// the one chosen at sign-up.
func (s *Service) CompleteSignup(ctx context.Context, token, password, ip, ua string) (*User, error) {
	if s.SendVerify == nil {
		return nil, ErrSignupClosed
	}
	now := time.Now().UTC()
	fail := func(detail string) (*User, error) {
		s.record(ctx, audit.Event{Action: audit.ActionSignUp, Outcome: audit.OutcomeFailure,
			IP: ip, UserAgent: ua, Detail: detail})
		return nil, ErrSignupLink
	}
	if token == "" || len(token) > maxTokenLen {
		VerifyPassword(password, s.dummyHash)
		return fail("malformed sign-up link")
	}
	p, err := s.repo.PendingSignup(ctx, HashToken(token), now)
	if errors.Is(err, ErrNotFound) {
		VerifyPassword(password, s.dummyHash)
		return fail("unknown, used or expired sign-up link")
	}
	if err != nil {
		return nil, err
	}
	if !VerifyPassword(password, p.passwordHash) {
		return fail("sign-up link opened with a different password")
	}
	u := &User{
		ID: newID(), Email: p.email, DisplayName: p.name,
		Role: RoleMember, Status: StatusActive,
		CreatedAt: now, UpdatedAt: now, passwordHash: p.passwordHash,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			_ = s.repo.ClearPendingSignups(ctx, p.email)
			return fail("address registered meanwhile")
		}
		return nil, err
	}
	if err := s.repo.ClearPendingSignups(ctx, p.email); err != nil {
		s.log.Error("failed to clear pending sign-ups", "err", err)
	}
	s.record(ctx, audit.Event{Action: audit.ActionSignUp, Outcome: audit.OutcomeSuccess,
		ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email, IP: ip, UserAgent: ua,
		Detail: "self-registered as member, address verified"})
	return u, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (h *Handler) completeSignup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Password) > 1024 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	ip, ua := httpx.ClientIP(r, h.TrustProxy), r.UserAgent()
	u, err := h.svc.CompleteSignup(r.Context(), req.Token, req.Password, ip, ua)
	switch {
	case errors.Is(err, ErrSignupLink):
		httpx.Error(w, http.StatusBadRequest, "signup_link",
			"this link has expired or was already used, or that is not the password you chose; sign up again")
		return
	case err != nil:
		h.fail(w, err)
		return
	}
	token, err := h.svc.StartSession(r.Context(), u, ip, ua)
	if err != nil {
		h.fail(w, err)
		return
	}
	csrf := h.setSession(w, token)
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"user": view(u), "permissions": u.Role.Permissions(), "csrf": csrf,
	})
}
