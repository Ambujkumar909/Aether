package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/platform/qr"
)

// Two-step sign-in: a code from an authenticator app (TOTP, totp.go) after
// the password, or one of ten single-use recovery codes.
//
// The shared secret is stored sealed (internal/platform/seal) with the key
// the server keeps beside its database, bound to the account id: a copy of
// the database alone yields no secrets, and a sealed value moved to another
// row does not open. Recovery codes are stored as keyed hashes. A code is
// accepted once: the time step it came from is recorded and only a later
// step is accepted, in one UPDATE, so two sign-ins racing with one code
// cannot both win.
//
// The password check and the code are two requests joined by a short-lived
// ticket held in memory: five wrong codes spend the ticket and count as a
// failed sign-in toward the account lockout.

const (
	TicketTTL      = 5 * time.Minute
	TicketTries    = 5
	ticketMax      = 10000
	recoveryCodes  = 10
	recoveryLen    = 10
	recoveryChars  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
	twoStepIssuer  = "Aether"
	secretLen      = 20 // 160 bits, what authenticator apps expect
	twoStepTOTPLen = 6
)

var (
	ErrTwoStepUnavailable = errors.New("identity: two-step sign-in is not configured on this server")
	ErrTwoStepOn          = errors.New("identity: two-step sign-in is already on")
	ErrTwoStepNotSetUp    = errors.New("identity: two-step sign-in has not been set up")
	ErrTicketInvalid      = errors.New("identity: sign-in ticket expired or used up")
)

// twoStep is an account's row.
type twoStep struct {
	sealed    []byte
	enabled   bool
	lastStep  int64
	createdAt time.Time
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

func (r *SQLRepository) TwoStep(ctx context.Context, userID string) (*twoStep, error) {
	var t twoStep
	var en int
	var created int64
	err := r.db.QueryRowContext(ctx, `SELECT secret, enabled, last_step, created_at FROM identity_two_step WHERE user_id = ?`, userID).
		Scan(&t.sealed, &en, &t.lastStep, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.enabled, t.createdAt = en == 1, time.UnixMilli(created).UTC()
	return &t, nil
}

// SaveTwoStep stores a new, not yet confirmed secret, replacing any row.
func (r *SQLRepository) SaveTwoStep(ctx context.Context, userID string, sealed []byte, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO identity_two_step (user_id, secret, enabled, last_step, created_at) VALUES (?,?,0,0,?)
		ON CONFLICT(user_id) DO UPDATE SET secret=excluded.secret, enabled=0, last_step=0, created_at=excluded.created_at, enabled_at=NULL`,
		userID, sealed, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("save two-step: %w", err)
	}
	return nil
}

func (r *SQLRepository) EnableTwoStep(ctx context.Context, userID string, step int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE identity_two_step SET enabled=1, last_step=?, enabled_at=? WHERE user_id=?`,
		step, at.UnixMilli(), userID)
	return err
}

// ConsumeTOTPStep records that a code from step was used, and reports
// whether it was the first use: a later step than any seen, in one statement.
func (r *SQLRepository) ConsumeTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE identity_two_step SET last_step=? WHERE user_id=? AND enabled=1 AND last_step<?`,
		step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *SQLRepository) DeleteTwoStep(ctx context.Context, userID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_two_step WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLRepository) ReplaceRecoveryCodes(ctx context.Context, userID string, macs []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, m := range macs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_recovery_codes (user_id, code_mac) VALUES (?,?)`, userID, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode spends a code, reporting whether it was live.
func (r *SQLRepository) UseRecoveryCode(ctx context.Context, userID, mac string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE identity_recovery_codes SET used_at=? WHERE user_id=? AND code_mac=? AND used_at IS NULL`,
		at.UnixMilli(), userID, mac)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *SQLRepository) RecoveryCodesLeft(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_recovery_codes WHERE user_id=? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// RevokeOtherSessions ends every session of an account but one.
func (r *SQLRepository) RevokeOtherSessions(ctx context.Context, userID, keepID string, at time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE identity_sessions SET revoked_at=? WHERE user_id=? AND id<>? AND revoked_at IS NULL`,
		at.UnixMilli(), userID, keepID)
	if err != nil {
		return 0, fmt.Errorf("revoke other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Tickets: the password checked, the code still owed
// ---------------------------------------------------------------------------

type loginTicket struct {
	userID  string
	expires time.Time
	tries   int
}

type ticketStore struct {
	mu sync.Mutex
	m  map[string]*loginTicket // by HashToken(ticket)
}

// issue returns a new ticket for an account, or "" when the store is full
// of live tickets (a flood: sign-in fails closed rather than growing).
func (t *ticketStore) issue(userID string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*loginTicket{}
	}
	if len(t.m) >= ticketMax {
		for k, v := range t.m {
			if !now.Before(v.expires) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= ticketMax {
			return ""
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	t.m[HashToken(tok)] = &loginTicket{userID: userID, expires: now.Add(TicketTTL)}
	return tok
}

// take returns the ticket's account and whether this attempt may proceed;
// the ticket is spent on success (done) or after TicketTries failures.
func (t *ticketStore) take(tok string, now time.Time, done bool) (userID string, ok, exhausted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := HashToken(tok)
	lt := t.m[k]
	if lt == nil || !now.Before(lt.expires) {
		delete(t.m, k)
		return "", false, false
	}
	if done {
		delete(t.m, k)
		return lt.userID, true, false
	}
	lt.tries++
	if lt.tries >= TicketTries {
		delete(t.m, k)
		return lt.userID, true, true
	}
	return lt.userID, true, false
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

func twoStepAD(userID string) []byte { return []byte("totp:" + userID) }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// twoStepOn reports whether an account must present a second step.
func (s *Service) twoStepOn(ctx context.Context, userID string) bool {
	if s.Sealer == nil {
		return false
	}
	t, err := s.repo.TwoStep(ctx, userID)
	return err == nil && t.enabled
}

// Login checks the password and either starts a session or, for an account
// with two-step sign-in, returns a ticket for the code.
func (s *Service) Login(ctx context.Context, email, password, ip, ua string) (u *User, session, ticket string, err error) {
	u, err = s.checkPassword(ctx, email, password, ip, ua)
	if err != nil {
		return nil, "", "", err
	}
	if s.twoStepOn(ctx, u.ID) {
		tok := s.tickets.issue(u.ID, s.clock())
		if tok == "" {
			return nil, "", "", errors.New("identity: too many sign-ins waiting for a code")
		}
		return u, "", tok, nil
	}
	session, err = s.finishLogin(ctx, u, ip, ua, "")
	return u, session, "", err
}

// SecondStep completes a sign-in with a code from the app or a recovery code.
func (s *Service) SecondStep(ctx context.Context, ticket, code, ip, ua string) (*User, string, error) {
	now := s.clock()
	code = normalizeCode(code)
	if s.Sealer == nil || ticket == "" || len(ticket) > maxTokenLen {
		return nil, "", ErrTicketInvalid
	}
	userID, ok, exhausted := s.tickets.take(ticket, now, false)
	if !ok {
		return nil, "", ErrTicketInvalid
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	fail := func(detail string) (*User, string, error) {
		s.record(ctx, audit.Event{Action: audit.ActionSecondStepFailed, Outcome: audit.OutcomeFailure,
			ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email, IP: ip, UserAgent: ua, Detail: detail})
		if exhausted {
			s.loginFailed(ctx, u, ip, ua, "five wrong codes")
			return nil, "", ErrTicketInvalid
		}
		return nil, "", ErrInvalidCredential
	}
	t, err := s.repo.TwoStep(ctx, u.ID)
	if err != nil || !t.enabled {
		return fail("second step asked of an account without one")
	}
	var detail string
	switch {
	case len(code) == twoStepTOTPLen && isDigits(code):
		secret, err := s.Sealer.Open(t.sealed, twoStepAD(u.ID))
		if err != nil {
			return nil, "", fmt.Errorf("two-step secret: %w", err)
		}
		step, ok := totpCheck(secret, code, now, t.lastStep)
		if !ok {
			return fail("wrong code")
		}
		first, err := s.repo.ConsumeTOTPStep(ctx, u.ID, step)
		if err != nil {
			return nil, "", err
		}
		if !first {
			return fail("code already used")
		}
		detail = "second step: authenticator code"
	case len(code) == recoveryLen:
		used, err := s.repo.UseRecoveryCode(ctx, u.ID, s.recoveryMAC(u.ID, code), now)
		if err != nil {
			return nil, "", err
		}
		if !used {
			return fail("wrong recovery code")
		}
		left, _ := s.repo.RecoveryCodesLeft(ctx, u.ID)
		detail = fmt.Sprintf("second step: recovery code (%d left)", left)
	default:
		return fail("malformed code")
	}
	s.tickets.take(ticket, now, true)
	session, err := s.finishLogin(ctx, u, ip, ua, detail)
	return u, session, err
}

// BeginTwoStep makes a new secret for the signed-in person to add to their
// authenticator app. Nothing is enforced until EnableTwoStep confirms a code.
func (s *Service) BeginTwoStep(ctx context.Context, actor *User, password string) (secret, uri, svg string, err error) {
	if s.Sealer == nil {
		return "", "", "", ErrTwoStepUnavailable
	}
	if !VerifyPassword(password, actor.passwordHash) {
		return "", "", "", ErrInvalidCredential
	}
	if t, err := s.repo.TwoStep(ctx, actor.ID); err == nil && t.enabled {
		return "", "", "", ErrTwoStepOn
	}
	raw := make([]byte, secretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", err
	}
	if err := s.repo.SaveTwoStep(ctx, actor.ID, s.Sealer.Seal(raw, twoStepAD(actor.ID)), time.Now().UTC()); err != nil {
		return "", "", "", err
	}
	secret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	uri = "otpauth://totp/" + url.PathEscape(twoStepIssuer+":"+actor.Email) +
		"?secret=" + secret + "&issuer=" + url.QueryEscape(twoStepIssuer) + "&algorithm=SHA1&digits=6&period=30"
	code, err := qr.Encode(uri)
	if err != nil {
		return "", "", "", err
	}
	return secret, uri, code.SVG(), nil
}

// EnableTwoStep turns the second step on once a code from the new secret
// proves the app has it, and returns the recovery codes -- the only time
// they are readable. Every other session ends: whoever else holds one did
// not pass the step.
func (s *Service) EnableTwoStep(ctx context.Context, actor *User, sess *Session, code, ip, ua string) ([]string, error) {
	if s.Sealer == nil {
		return nil, ErrTwoStepUnavailable
	}
	t, err := s.repo.TwoStep(ctx, actor.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrTwoStepNotSetUp
	}
	if err != nil {
		return nil, err
	}
	if t.enabled {
		return nil, ErrTwoStepOn
	}
	secret, err := s.Sealer.Open(t.sealed, twoStepAD(actor.ID))
	if err != nil {
		return nil, fmt.Errorf("two-step secret: %w", err)
	}
	now := s.clock()
	step, ok := totpCheck(secret, normalizeCode(code), now, 0)
	if !ok {
		s.record(ctx, audit.Event{Action: audit.ActionTwoStepEnabled, Outcome: audit.OutcomeFailure,
			ActorID: actor.ID, ActorEmail: actor.Email, Subject: actor.Email, IP: ip, UserAgent: ua, Detail: "wrong code"})
		return nil, ErrInvalidCredential
	}
	codes, macs := s.newRecoveryCodes(actor.ID)
	if err := s.repo.ReplaceRecoveryCodes(ctx, actor.ID, macs); err != nil {
		return nil, err
	}
	if err := s.repo.EnableTwoStep(ctx, actor.ID, step, now); err != nil {
		return nil, err
	}
	keep := ""
	if sess != nil {
		keep = sess.ID
	}
	if _, err := s.repo.RevokeOtherSessions(ctx, actor.ID, keep, now); err != nil {
		s.log.Error("failed to revoke other sessions after enabling two-step", "err", err)
	}
	s.record(ctx, audit.Event{Action: audit.ActionTwoStepEnabled, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: actor.Email, IP: ip, UserAgent: ua})
	return codes, nil
}

// DisableTwoStep turns it off for the signed-in person, password re-checked.
func (s *Service) DisableTwoStep(ctx context.Context, actor *User, password, ip, ua string) error {
	if s.Sealer == nil {
		return ErrTwoStepUnavailable
	}
	if !VerifyPassword(password, actor.passwordHash) {
		s.record(ctx, audit.Event{Action: audit.ActionTwoStepDisabled, Outcome: audit.OutcomeFailure,
			ActorID: actor.ID, ActorEmail: actor.Email, Subject: actor.Email, IP: ip, UserAgent: ua, Detail: "password incorrect"})
		return ErrInvalidCredential
	}
	if err := s.repo.DeleteTwoStep(ctx, actor.ID); err != nil {
		return err
	}
	s.record(ctx, audit.Event{Action: audit.ActionTwoStepDisabled, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: actor.Email, IP: ip, UserAgent: ua})
	return nil
}

// ResetTwoStep is an administrator removing somebody's second step -- a lost
// phone and no recovery codes. Every session of the account ends.
func (s *Service) ResetTwoStep(ctx context.Context, actor *User, userID, ip, ua string) error {
	if !actor.Role.Can(PermUserWrite) || actor.ID == userID {
		s.denied(ctx, actor, audit.ActionTwoStepReset, userID, ip, ua)
		return ErrForbidden
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role.rank() > actor.Role.rank() {
		s.denied(ctx, actor, audit.ActionTwoStepReset, u.Email, ip, ua)
		return ErrForbidden
	}
	// Nothing to remove is an answer, not a no-op: the reset also ends every
	// session, which must not happen to an account that never had the step.
	if _, err := s.repo.TwoStep(ctx, u.ID); errors.Is(err, ErrNotFound) {
		return ErrTwoStepNotSetUp
	} else if err != nil {
		return err
	}
	if err := s.repo.DeleteTwoStep(ctx, u.ID); err != nil {
		return err
	}
	if _, err := s.repo.RevokeUserSessions(ctx, u.ID, time.Now().UTC()); err != nil {
		s.log.Error("failed to revoke sessions after two-step reset", "err", err)
	}
	s.record(ctx, audit.Event{Action: audit.ActionTwoStepReset, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email, IP: ip, UserAgent: ua})
	return nil
}

// TwoStepStatus is what the account screen shows.
func (s *Service) TwoStepStatus(ctx context.Context, userID string) (enabled bool, codesLeft int) {
	if s.Sealer == nil {
		return false, 0
	}
	t, err := s.repo.TwoStep(ctx, userID)
	if err != nil || !t.enabled {
		return false, 0
	}
	n, _ := s.repo.RecoveryCodesLeft(ctx, userID)
	return true, n
}

func (s *Service) recoveryMAC(userID, code string) string {
	return s.Sealer.MAC("recovery:" + userID + ":" + code)
}

func (s *Service) newRecoveryCodes(userID string) (codes, macs []string) {
	for i := 0; i < recoveryCodes; i++ {
		raw := make([]byte, recoveryLen)
		if _, err := rand.Read(raw); err != nil {
			panic(err)
		}
		b := make([]byte, recoveryLen)
		for j := range b {
			b[j] = recoveryChars[int(raw[j])%len(recoveryChars)]
		}
		c := string(b)
		codes = append(codes, c[:5]+"-"+c[5:])
		macs = append(macs, s.recoveryMAC(userID, c))
	}
	return codes, macs
}

// normalizeCode strips what people type around a code: spaces, dashes, case.
func normalizeCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (h *Handler) secondStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Code) > 64 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	u, token, err := h.svc.SecondStep(r.Context(), req.Ticket, req.Code, httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	switch {
	case errors.Is(err, ErrTicketInvalid):
		httpx.Error(w, http.StatusUnauthorized, "ticket_invalid", "start again with your email and password")
		return
	case errors.Is(err, ErrInvalidCredential):
		httpx.Error(w, http.StatusUnauthorized, "invalid_code", "that code is not right")
		return
	case err != nil:
		h.fail(w, err)
		return
	}
	csrf := h.setSession(w, token)
	httpx.JSON(w, http.StatusOK, map[string]any{"user": view(u), "permissions": u.Role.Permissions(), "csrf": csrf})
}

func (h *Handler) twoStepSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	secret, uri, svg, err := h.svc.BeginTwoStep(r.Context(), UserFrom(r.Context()), req.Password)
	if err != nil {
		h.failTwoStep(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secret": secret, "uri": uri, "qr": svg})
}

func (h *Handler) twoStepEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Code) > 64 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	codes, err := h.svc.EnableTwoStep(r.Context(), UserFrom(r.Context()), SessionFrom(r.Context()), req.Code,
		httpx.ClientIP(r, h.TrustProxy), r.UserAgent())
	if err != nil {
		h.failTwoStep(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func (h *Handler) twoStepDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if err := h.svc.DisableTwoStep(r.Context(), UserFrom(r.Context()), req.Password, httpx.ClientIP(r, h.TrustProxy), r.UserAgent()); err != nil {
		h.failTwoStep(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "two-step sign-in is off"})
}

func (h *Handler) twoStepReset(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.ResetTwoStep(r.Context(), UserFrom(r.Context()), r.PathValue("id"), httpx.ClientIP(r, h.TrustProxy), r.UserAgent()); err != nil {
		h.failTwoStep(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "two-step sign-in removed; every session of the account ended"})
}

func (h *Handler) failTwoStep(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTwoStepUnavailable):
		httpx.Error(w, http.StatusNotFound, "two_step_unavailable", "two-step sign-in is not available on this server")
	case errors.Is(err, ErrTwoStepOn):
		httpx.Error(w, http.StatusConflict, "two_step_on", "two-step sign-in is already on; turn it off first")
	case errors.Is(err, ErrTwoStepNotSetUp):
		httpx.Error(w, http.StatusConflict, "two_step_not_set_up", "two-step sign-in is not set up on this account")
	case errors.Is(err, ErrInvalidCredential):
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", "incorrect credentials")
	default:
		h.fail(w, err)
	}
}
