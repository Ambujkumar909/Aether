package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/seal"
)

func newSealer(t *testing.T) *seal.Sealer {
	t.Helper()
	k := make([]byte, seal.KeyLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	s, err := seal.New(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// twoStepService is testService with a sealer and a member who has turned
// two-step sign-in on. Returns the service, the member, their secret (the
// bytes the authenticator app holds) and their recovery codes.
func twoStepService(t *testing.T) (*Service, *audit.Service, *User, []byte, []string) {
	t.Helper()
	s, au := testService(t)
	s.Sealer = newSealer(t)
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	ctx := context.Background()
	b32, uri, svg, err := s.BeginTwoStep(ctx, u, "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "otpauth://totp/Aether:zoe@example.com?secret="+b32+"&issuer=Aether") || !strings.HasPrefix(svg, "<svg ") {
		t.Fatalf("uri %q, svg starts %q", uri, svg[:20])
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if err != nil || len(secret) != secretLen {
		t.Fatalf("secret %v %v", secret, err)
	}
	// The pending secret enforces nothing yet.
	if _, _, err := s.Authenticate(ctx, u.Email, "correct-horse-battery", "", ""); err != nil {
		t.Fatalf("a set-up but unconfirmed second step blocked sign-in: %v", err)
	}
	codes, err := s.EnableTwoStep(ctx, u, nil, codeAt(secret, time.Now()), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != recoveryCodes {
		t.Fatalf("%d recovery codes", len(codes))
	}
	u, _ = s.repo.UserByID(ctx, u.ID)
	return s, au, u, secret, codes
}

func codeAt(secret []byte, at time.Time) string {
	return totpAt(secret, at.Unix()/totpPeriod, totpDigits)
}

func TestTwoStepSignInAsksForACodeAndAcceptsEachCodeOnce(t *testing.T) {
	s, _, u, secret, _ := twoStepService(t)
	ctx := context.Background()
	if _, _, err := s.Authenticate(ctx, u.Email, "correct-horse-battery", "", ""); !errors.Is(err, ErrSecondStepRequired) {
		t.Fatalf("Authenticate bypassed the second step: %v", err)
	}
	got, session, ticket, err := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	if err != nil || got.ID != u.ID || session != "" || ticket == "" {
		t.Fatalf("login: %v session %q ticket %q", err, session, ticket)
	}
	if _, _, _, err := s.Login(ctx, u.Email, "wrong", "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("a wrong password got a ticket: %v", err)
	}
	if _, _, err := s.SecondStep(ctx, ticket, "000000", "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("a wrong code: %v", err)
	}
	now := nextWindow(s)
	code := codeAt(secret, now)
	got, session, err = s.SecondStep(ctx, ticket, code, "", "")
	if err != nil || got.ID != u.ID || session == "" {
		t.Fatalf("second step: %v", err)
	}
	if _, _, err := s.Authorize(ctx, session); err != nil {
		t.Fatalf("the session does not authorise: %v", err)
	}
	if _, _, err := s.SecondStep(ctx, ticket, code, "", ""); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a spent ticket: %v", err)
	}
	// The same code on a fresh sign-in is a replay.
	_, _, ticket2, _ := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	if _, _, err := s.SecondStep(ctx, ticket2, code, "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("a code was accepted twice: %v", err)
	}
	// The next step's code is fine (the ticket survives a wrong try).
	if _, session, err = s.SecondStep(ctx, ticket2, codeAt(secret, now.Add(totpPeriod*time.Second)), "", ""); err != nil || session == "" {
		t.Fatalf("the next code: %v", err)
	}
}

func TestRecoveryCodesWorkOnceEachHoweverTyped(t *testing.T) {
	s, _, u, _, codes := twoStepService(t)
	ctx := context.Background()
	login := func() string {
		_, _, tk, err := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
		if err != nil || tk == "" {
			t.Fatalf("login: %v", err)
		}
		return tk
	}
	typed := strings.ToLower(codes[0][:3]) + " " + codes[0][3:] // "ab cde-fghij"
	if _, session, err := s.SecondStep(ctx, login(), typed, "", ""); err != nil || session == "" {
		t.Fatalf("recovery code as typed: %v", err)
	}
	if _, _, err := s.SecondStep(ctx, login(), codes[0], "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("a recovery code worked twice: %v", err)
	}
	if on, left := s.TwoStepStatus(ctx, u.ID); !on || left != recoveryCodes-1 {
		t.Fatalf("status %v %d", on, left)
	}
	if _, _, err := s.SecondStep(ctx, login(), "ZZZZZ-ZZZZZ", "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("a made-up recovery code: %v", err)
	}
}

func TestFiveWrongCodesSpendTheTicketAndCountAsAFailedSignIn(t *testing.T) {
	s, au, u, secret, _ := twoStepService(t)
	ctx := context.Background()
	_, _, ticket, _ := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	for i := 1; i <= TicketTries; i++ {
		_, _, err := s.SecondStep(ctx, ticket, "123456", "9.9.9.9", "t")
		if i < TicketTries && !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("try %d: %v", i, err)
		}
		if i == TicketTries && !errors.Is(err, ErrTicketInvalid) {
			t.Fatalf("try %d should have spent the ticket: %v", i, err)
		}
	}
	if _, _, err := s.SecondStep(ctx, ticket, codeAt(secret, time.Now()), "", ""); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("the right code after five wrong ones: %v", err)
	}
	got, _ := s.repo.UserByID(ctx, u.ID)
	if got.FailedLogins != 1 {
		t.Fatalf("failed logins %d, want 1 (the spent ticket)", got.FailedLogins)
	}
	events, _ := au.List(ctx, audit.Filter{Limit: 100})
	n := 0
	for _, e := range events {
		if e.Action == audit.ActionSecondStepFailed {
			n++
		}
	}
	if n != TicketTries {
		t.Fatalf("%d second-step failures recorded, want %d", n, TicketTries)
	}
}

func TestEnablingNeedsTheRightCodeAndEndsOtherSessions(t *testing.T) {
	s, _ := testService(t)
	s.Sealer = newSealer(t)
	ctx := context.Background()
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	_, mine, _ := s.Authenticate(ctx, u.Email, "correct-horse-battery", "1.1.1.1", "laptop")
	_, other, _ := s.Authenticate(ctx, u.Email, "correct-horse-battery", "2.2.2.2", "phone")
	_, sess, _ := s.Authorize(ctx, mine)
	if _, err := s.EnableTwoStep(ctx, u, sess, "123456", "", ""); !errors.Is(err, ErrTwoStepNotSetUp) {
		t.Fatalf("enable before setup: %v", err)
	}
	if _, _, _, err := s.BeginTwoStep(ctx, u, "not-my-password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("setup without the password: %v", err)
	}
	b32, _, _, err := s.BeginTwoStep(ctx, u, "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if _, err := s.EnableTwoStep(ctx, u, sess, "000000", "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("enable with a wrong code: %v", err)
	}
	if _, err := s.EnableTwoStep(ctx, u, sess, codeAt(secret, time.Now()), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authorize(ctx, mine); err != nil {
		t.Fatal("the session that turned it on was ended")
	}
	if _, _, err := s.Authorize(ctx, other); err == nil {
		t.Fatal("another session survived turning two-step on")
	}
	if _, _, _, err := s.BeginTwoStep(ctx, u, "correct-horse-battery"); !errors.Is(err, ErrTwoStepOn) {
		t.Fatalf("setup while on: %v", err)
	}
}

func TestDisableNeedsThePasswordAndAnAdministratorCanReset(t *testing.T) {
	s, _, u, _, _ := twoStepService(t)
	ctx := context.Background()
	if err := s.DisableTwoStep(ctx, u, "wrong", "", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("disable without the password: %v", err)
	}
	if err := s.DisableTwoStep(ctx, u, "correct-horse-battery", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, session, ticket, err := s.Login(ctx, u.Email, "correct-horse-battery", "", ""); err != nil || session == "" || ticket != "" {
		t.Fatalf("after disable, login: %v session %q ticket %q", err, session, ticket)
	}

	// On again, then an administrator removes it for a lost phone.
	b32, _, _, _ := s.BeginTwoStep(ctx, u, "correct-horse-battery")
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if _, err := s.EnableTwoStep(ctx, u, nil, codeAt(secret, time.Now()), "", ""); err != nil {
		t.Fatal(err)
	}
	_, live, _, _ := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	_ = live
	member := mkUser(t, s, "other@example.com", RoleMember)
	if err := s.ResetTwoStep(ctx, member, u.ID, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member reset somebody's second step: %v", err)
	}
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	if err := s.ResetTwoStep(ctx, admin, admin.ID, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin reset their own through the admin path: %v", err)
	}
	_, _, ticket, _ := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	_, tok, _ := s.Authenticate(ctx, admin.Email, "correct-horse-battery", "", "")
	_ = tok
	if err := s.ResetTwoStep(ctx, admin, u.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.TwoStepStatus(ctx, u.ID); on {
		t.Fatal("still on after the reset")
	}
	if err := s.ResetTwoStep(ctx, admin, u.ID, "", ""); !errors.Is(err, ErrTwoStepNotSetUp) {
		t.Fatalf("a reset with nothing to remove (it would end every session): %v", err)
	}
	if _, _, err := s.SecondStep(ctx, ticket, "123456", "", ""); !errors.Is(err, ErrInvalidCredential) && !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a ticket from before the reset: %v", err)
	}
}

func TestTheSecretIsSealedAtRestAndBoundToTheServerKey(t *testing.T) {
	s, _, u, secret, _ := twoStepService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	var blob []byte
	if err := repo.db.QueryRowContext(ctx, `SELECT secret FROM identity_two_step WHERE user_id=?`, u.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), string(secret)) || len(blob) < len(secret)+28 {
		t.Fatalf("the stored secret is readable (%d bytes)", len(blob))
	}
	// Another server's key cannot use the row: the second step fails closed.
	s.Sealer = newSealer(t)
	_, _, ticket, _ := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
	if _, session, err := s.SecondStep(ctx, ticket, codeAt(secret, time.Now()), "", ""); err == nil || session != "" {
		t.Fatalf("a secret opened with the wrong key: %v", err)
	}
	// And the row goes with the account.
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	if err := s.DeleteUser(ctx, admin, u.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_two_step WHERE user_id=?`, u.ID).Scan(&n)
	var m int
	_ = repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_recovery_codes WHERE user_id=?`, u.ID).Scan(&m)
	if n+m != 0 {
		t.Fatalf("%d two-step rows and %d codes outlived the account", n, m)
	}
}

func TestTicketsExpireAndTheStoreIsBounded(t *testing.T) {
	var ts ticketStore
	now := time.Now()
	tok := ts.issue("u1", now)
	if id, ok, _ := ts.take(tok, now.Add(TicketTTL-time.Second), false); !ok || id != "u1" {
		t.Fatal("a live ticket was refused")
	}
	if _, ok, _ := ts.take(tok, now.Add(TicketTTL+time.Second), false); ok {
		t.Fatal("an expired ticket was accepted")
	}
	for i := 0; i < ticketMax; i++ {
		if ts.issue("u", now) == "" {
			t.Fatalf("refused at %d, under the bound", i)
		}
	}
	if ts.issue("u", now) != "" {
		t.Fatal("issued past the bound")
	}
	if ts.issue("u", now.Add(TicketTTL+time.Second)) == "" {
		t.Fatal("expired tickets were not evicted to make room")
	}
	if len(ts.m) > ticketMax {
		t.Fatalf("%d tickets held, bound %d", len(ts.m), ticketMax)
	}
}

func TestOverHTTPTheSecondStepIsASeparateRequestWithNoSessionBetween(t *testing.T) {
	s, _, u, secret, _ := twoStepService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), true, false)
	w, _ := post(h.login, `{"email":"zoe@example.com","password":"correct-horse-battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Fatal("a session cookie was set before the code")
		}
	}
	var first struct {
		SecondStep, Ticket string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	if first.SecondStep != "code" || first.Ticket == "" {
		t.Fatalf("first step answer: %s", w.Body)
	}
	w, _ = post(h.secondStep, `{"ticket":"`+first.Ticket+`","code":"000000"}`)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_code") {
		t.Fatalf("wrong code: %d %s", w.Code, w.Body)
	}
	w, _ = post(h.secondStep, `{"ticket":"`+first.Ticket+`","code":"`+codeAt(secret, nextWindow(s))+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second step: %d %s", w.Code, w.Body)
	}
	var token string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			token = c.Value
		}
	}
	if token == "" || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Fatalf("no session after the second step: %s", w.Body)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/identity/me", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.RequireAuth(http.HandlerFunc(h.me)).ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) || !strings.Contains(rec.Body.String(), `"recoveryCodesLeft":10`) {
		t.Fatalf("/me: %d %s", rec.Code, rec.Body)
	}
	_ = u
	w, _ = post(h.secondStep, `{"ticket":"`+strings.Repeat("x", 200)+`","code":"123456"}`)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "ticket_invalid") {
		t.Fatalf("an oversized ticket: %d %s", w.Code, w.Body)
	}
}

// The server's key file: made on first start, and a restart reads the same
// one, so secrets sealed before still open.
func TestTheSealKeySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aether.key")
	k1, err := seal.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := seal.New(k1)
	sealed := s1.Seal([]byte("secret"), []byte("totp:u1"))
	k2, _ := seal.LoadOrCreateKey(path)
	s2, _ := seal.New(k2)
	if got, err := s2.Open(sealed, []byte("totp:u1")); err != nil || string(got) != "secret" {
		t.Fatalf("after a restart: %q %v", got, err)
	}
	db, err := database.Open(context.Background(), filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('identity_two_step','identity_recovery_codes')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("migration 0070: %d tables, %v", n, err)
	}
	_ = sql.ErrNoRows
}

// nextWindow moves the service clock one TOTP step past real time: the code
// used to turn two-step on was this step, and a code is taken once.
func nextWindow(s *Service) time.Time {
	at := time.Now().UTC().Add(totpPeriod * time.Second)
	s.now = func() time.Time { return at }
	return at
}

// Two sign-ins racing with the same code: the sequential check reads the
// last step before either writes it, so both pass it. The UPDATE that
// records the step is what lets exactly one through.
func TestOfManySignInsRacingWithOneCodeExactlyOneWins(t *testing.T) {
	s, _, u, secret, _ := twoStepService(t)
	ctx := context.Background()
	now := nextWindow(s)
	code := codeAt(secret, now)
	tickets := make([]string, 8)
	for i := range tickets {
		_, _, tk, err := s.Login(ctx, u.Email, "correct-horse-battery", "", "")
		if err != nil {
			t.Fatal(err)
		}
		tickets[i] = tk
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, tk := range tickets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, session, err := s.SecondStep(ctx, tk, code, "", ""); err == nil && session != "" {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d sign-ins succeeded with one code", wins.Load())
	}
}
