package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Password reset by email.
//
// A request never says whether the address has an account: the same answer,
// after the same time, either way. The link carries a 256-bit token whose
// SHA-256 is all that is stored; it works once, for ResetTTL, and asking
// again replaces it. Using it sets the password, clears any lockout and ends
// every session the account had -- a reset is what somebody does when they
// think someone else has their password.

const (
	ResetTTL = 30 * time.Minute
	// ResetEvery is the most often one account is sent a link, so a stranger
	// cannot fill somebody's inbox.
	ResetEvery = time.Minute
	// resetAnswerAfter is how long every request takes to answer, whatever
	// happened: an account and none must not differ by a database write.
	resetAnswerAfter = 400 * time.Millisecond
	maxTokenLen      = 128
)

var (
	ErrResetUnavailable = errors.New("identity: password reset is not configured")
	ErrResetInvalid     = errors.New("identity: reset link expired or already used")
)

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

func (r *SQLRepository) ResetCreatedAt(ctx context.Context, userID string) (time.Time, error) {
	var ms int64
	err := r.db.QueryRowContext(ctx,
		`SELECT created_at FROM identity_password_resets WHERE user_id=?`, userID).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return time.UnixMilli(ms).UTC(), err
}

// SaveReset stores a token, replacing the account's previous one.
func (r *SQLRepository) SaveReset(ctx context.Context, tokenHash, userID string, created, expires time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO identity_password_resets (token_hash, user_id, created_at, expires_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash,
			created_at=excluded.created_at, expires_at=excluded.expires_at`,
		tokenHash, userID, created.UnixMilli(), expires.UnixMilli())
	if err != nil {
		return fmt.Errorf("save reset: %w", err)
	}
	return nil
}

// ConsumeReset deletes a live token and returns its account, in one
// statement: of two requests racing with the same link, one gets it.
func (r *SQLRepository) ConsumeReset(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var uid string
	err := r.db.QueryRowContext(ctx,
		`DELETE FROM identity_password_resets WHERE token_hash=? AND expires_at>? RETURNING user_id`,
		tokenHash, now.UnixMilli()).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return uid, err
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// RequestPasswordReset sends a link if email belongs to an active account.
// It returns nil whether or not one was sent; only ErrResetUnavailable and
// infrastructure failures are errors.
func (s *Service) RequestPasswordReset(ctx context.Context, email, ip, ua string) error {
	if s.SendReset == nil {
		return ErrResetUnavailable
	}
	now := time.Now().UTC()
	ev := audit.Event{Action: audit.ActionPasswordResetAsked, Outcome: audit.OutcomeFailure,
		Subject: email, IP: ip, UserAgent: ua}
	norm, err := NormalizeEmail(email)
	if err != nil {
		ev.Detail = "malformed email"
		s.record(ctx, ev)
		return nil
	}
	ev.Subject = norm
	u, err := s.repo.UserByEmail(ctx, norm)
	if errors.Is(err, ErrNotFound) {
		ev.Detail = "no such account"
		s.record(ctx, ev)
		return nil
	}
	if err != nil {
		return err
	}
	ev.ActorID, ev.ActorEmail = u.ID, u.Email
	if u.Status == StatusSuspended {
		ev.Outcome, ev.Detail = audit.OutcomeDenied, "account suspended"
		s.record(ctx, ev)
		return nil
	}
	if last, err := s.repo.ResetCreatedAt(ctx, u.ID); err == nil && now.Sub(last) < ResetEvery {
		ev.Outcome, ev.Detail = audit.OutcomeDenied, "asked again within a minute"
		s.record(ctx, ev)
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.repo.SaveReset(ctx, HashToken(token), u.ID, now, now.Add(ResetTTL)); err != nil {
		return err
	}
	s.SendReset(u.Email, token)
	ev.Outcome = audit.OutcomeSuccess
	s.record(ctx, ev)
	return nil
}

// ResetPassword sets a new password with a link's token.
func (s *Service) ResetPassword(ctx context.Context, token, next, ip, ua string) error {
	if s.SendReset == nil {
		return ErrResetUnavailable
	}
	// The password is checked first, so a typo does not spend the link.
	if err := ValidatePassword(next); err != nil {
		return err
	}
	ev := audit.Event{Action: audit.ActionPasswordReset, Outcome: audit.OutcomeFailure, IP: ip, UserAgent: ua}
	if token == "" || len(token) > maxTokenLen {
		ev.Detail = "malformed token"
		s.record(ctx, ev)
		return ErrResetInvalid
	}
	now := time.Now().UTC()
	uid, err := s.repo.ConsumeReset(ctx, HashToken(token), now)
	if errors.Is(err, ErrNotFound) {
		ev.Detail = "unknown, used or expired link"
		s.record(ctx, ev)
		return ErrResetInvalid
	}
	if err != nil {
		return err
	}
	u, err := s.repo.UserByID(ctx, uid)
	if err != nil {
		return err
	}
	ev.ActorID, ev.ActorEmail, ev.Subject = u.ID, u.Email, u.Email
	if u.Status == StatusSuspended {
		ev.Outcome, ev.Detail = audit.OutcomeDenied, "account suspended"
		s.record(ctx, ev)
		return ErrResetInvalid
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	u.passwordHash = hash
	u.FailedLogins = 0
	u.LockedUntil = nil
	u.UpdatedAt = now
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return err
	}
	if _, err := s.repo.RevokeUserSessions(ctx, u.ID, now); err != nil {
		s.log.Error("failed to revoke sessions after password reset", "err", err)
	}
	ev.Outcome = audit.OutcomeSuccess
	s.record(ctx, ev)
	return nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	defer evenOut(time.Now())
	var req struct {
		Email string `json:"email"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Email) > 320 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	err := h.svc.RequestPasswordReset(r.Context(), req.Email, httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	switch {
	case errors.Is(err, ErrResetUnavailable):
		httpx.Error(w, http.StatusNotFound, "reset_unavailable", "password reset is not available on this server")
	case err != nil:
		h.fail(w, err)
	default:
		httpx.JSON(w, http.StatusAccepted, map[string]string{
			"status": "If an account uses that address, a link to reset its password is on its way.",
		})
	}
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	err := h.svc.ResetPassword(r.Context(), req.Token, req.Password, httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	switch {
	case errors.Is(err, ErrResetUnavailable):
		httpx.Error(w, http.StatusNotFound, "reset_unavailable", "password reset is not available on this server")
	case errors.Is(err, ErrResetInvalid):
		httpx.Error(w, http.StatusBadRequest, "reset_invalid", "this link has expired or was already used; ask for a new one")
	case err != nil:
		h.fail(w, err)
	default:
		// Any session this browser had died with the others.
		h.clearSession(w)
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "Password changed. Sign in with the new one."})
	}
}

// evenOut holds a response until resetAnswerAfter has passed since start.
func evenOut(start time.Time) {
	if d := resetAnswerAfter - time.Since(start); d > 0 {
		time.Sleep(d)
	}
}
