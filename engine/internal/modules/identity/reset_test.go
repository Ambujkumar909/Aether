package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
)

// mailbox captures what SendReset would have mailed.
type mailbox struct {
	mu    sync.Mutex
	sent  []string // "email token"
	count atomic.Int32
}

func (m *mailbox) send(email, token string) {
	m.mu.Lock()
	m.sent = append(m.sent, email+" "+token)
	m.mu.Unlock()
	m.count.Add(1)
}

func (m *mailbox) last(t *testing.T) (string, string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("nothing was mailed")
	}
	f := strings.SplitN(m.sent[len(m.sent)-1], " ", 2)
	return f[0], f[1]
}

func resetService(t *testing.T) (*Service, *audit.Service, *mailbox) {
	t.Helper()
	s, au := testService(t)
	mb := &mailbox{}
	s.SendReset = mb.send
	return s, au, mb
}

const newPassword = "a-brand-new-passphrase"

func TestAResetLinkSetsThePasswordOnceAndEndsEverySession(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	_, oldSession, err := s.Authenticate(ctx, "zoe@example.com", "correct-horse-battery", "1.1.1.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	// A lockout in progress is cleared by a reset.
	for i := 0; i < MaxFailedLogins; i++ {
		_, _, _ = s.Authenticate(ctx, "zoe@example.com", "wrong", "1.1.1.1", "t")
	}

	if err := s.RequestPasswordReset(ctx, "ZOE@example.com", "2.2.2.2", "t"); err != nil {
		t.Fatal(err)
	}
	to, token := mb.last(t)
	if to != "zoe@example.com" || len(token) < 43 {
		t.Fatalf("mailed %q a token of %d characters", to, len(token))
	}
	if err := s.ResetPassword(ctx, token, newPassword, "2.2.2.2", "t"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authorize(ctx, oldSession); err == nil {
		t.Fatal("a session from before the reset still works")
	}
	if _, _, err := s.Authenticate(ctx, "zoe@example.com", "correct-horse-battery", "1.1.1.1", "t"); err == nil {
		t.Fatal("the old password still works")
	}
	if _, _, err := s.Authenticate(ctx, "zoe@example.com", newPassword, "1.1.1.1", "t"); err != nil {
		t.Fatalf("the new password does not work (lockout not cleared?): %v", err)
	}
	if err := s.ResetPassword(ctx, token, "another-passphrase-x", "2.2.2.2", "t"); !errors.Is(err, ErrResetInvalid) {
		t.Fatalf("a link worked twice: %v", err)
	}
	if got, _ := s.repo.UserByID(ctx, u.ID); got.LockedUntil != nil {
		t.Fatal("still locked")
	}
}

func TestOfManyRacingUsesOfOneLinkExactlyOneWins(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	mkUser(t, s, "zoe@example.com", RoleMember)
	_ = s.RequestPasswordReset(ctx, "zoe@example.com", "", "")
	_, token := mb.last(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.ResetPassword(ctx, token, newPassword, "", "") == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d uses of one link succeeded", wins.Load())
	}
}

func TestAnExpiredLinkDoesNothing(t *testing.T) {
	s, _, _ := resetService(t)
	ctx := context.Background()
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	past := time.Now().Add(-ResetTTL - time.Minute)
	if err := s.repo.SaveReset(ctx, HashToken("old-token"), u.ID, past, past.Add(ResetTTL)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, "old-token", newPassword, "", ""); !errors.Is(err, ErrResetInvalid) {
		t.Fatalf("an expired link: %v", err)
	}
}

func TestNoAccountSuspendedOrMalformedAllAnswerTheSameAndMailNothing(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	sus := mkUser(t, s, "gone@example.com", RoleMember)
	if _, err := s.SetStatus(ctx, admin, sus.ID, StatusSuspended, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"nobody@example.com", "gone@example.com", "not an address", ""} {
		if err := s.RequestPasswordReset(ctx, e, "", ""); err != nil {
			t.Errorf("%q: %v", e, err)
		}
	}
	if n := mb.count.Load(); n != 0 {
		t.Fatalf("%d links mailed for accounts that may not reset", n)
	}
}

func TestOneAccountIsMailedAtMostOnceAMinuteAndANewLinkReplacesTheOld(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	for i := 0; i < 5; i++ {
		_ = s.RequestPasswordReset(ctx, "zoe@example.com", "", "")
	}
	if n := mb.count.Load(); n != 1 {
		t.Fatalf("five requests in a row mailed %d links", n)
	}
	_, first := mb.last(t)
	// A minute later: a new link, and the first stops working.
	earlier := time.Now().Add(-2 * ResetEvery)
	if err := s.repo.SaveReset(ctx, HashToken(first), u.ID, earlier, earlier.Add(ResetTTL)); err != nil {
		t.Fatal(err)
	}
	_ = s.RequestPasswordReset(ctx, "zoe@example.com", "", "")
	_, second := mb.last(t)
	if second == first || mb.count.Load() != 2 {
		t.Fatalf("no new link after the minute (%d sent)", mb.count.Load())
	}
	if err := s.ResetPassword(ctx, first, newPassword, "", ""); !errors.Is(err, ErrResetInvalid) {
		t.Fatalf("the replaced link still works: %v", err)
	}
	if err := s.ResetPassword(ctx, second, newPassword, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestAWeakPasswordDoesNotSpendTheLink(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	mkUser(t, s, "zoe@example.com", RoleMember)
	_ = s.RequestPasswordReset(ctx, "zoe@example.com", "", "")
	_, token := mb.last(t)
	if err := s.ResetPassword(ctx, token, "short", "", ""); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if err := s.ResetPassword(ctx, token, newPassword, "", ""); err != nil {
		t.Fatalf("the link was spent by a rejected password: %v", err)
	}
}

func TestDeletingTheAccountDeletesItsLink(t *testing.T) {
	s, _, mb := resetService(t)
	ctx := context.Background()
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	u := mkUser(t, s, "zoe@example.com", RoleMember)
	_ = s.RequestPasswordReset(ctx, "zoe@example.com", "", "")
	_, token := mb.last(t)
	if err := s.DeleteUser(ctx, admin, u.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.ConsumeReset(ctx, HashToken(token), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the link outlived its account: %v", err)
	}
}

func TestResetIsOffWithoutMail(t *testing.T) {
	s, _ := testService(t)
	mkUser(t, s, "zoe@example.com", RoleMember)
	if err := s.RequestPasswordReset(context.Background(), "zoe@example.com", "", ""); !errors.Is(err, ErrResetUnavailable) {
		t.Fatalf("request: %v", err)
	}
	if err := s.ResetPassword(context.Background(), "x", newPassword, "", ""); !errors.Is(err, ErrResetUnavailable) {
		t.Fatalf("reset: %v", err)
	}
}

func TestResetsAreAudited(t *testing.T) {
	s, au, mb := resetService(t)
	ctx := context.Background()
	mkUser(t, s, "zoe@example.com", RoleMember)
	_ = s.RequestPasswordReset(ctx, "zoe@example.com", "3.3.3.3", "t")
	_ = s.RequestPasswordReset(ctx, "nobody@example.com", "3.3.3.3", "t")
	_, token := mb.last(t)
	_ = s.ResetPassword(ctx, token, newPassword, "3.3.3.3", "t")
	_ = s.ResetPassword(ctx, token, newPassword, "3.3.3.3", "t")
	events, err := au.List(ctx, audit.Filter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range events {
		seen[string(e.Action)+"/"+string(e.Outcome)]++
	}
	for _, k := range []string{
		"auth.password.reset.requested/success", "auth.password.reset.requested/failure",
		"auth.password.reset/success", "auth.password.reset/failure",
	} {
		if seen[k] != 1 {
			t.Errorf("%s recorded %d times, want 1 (%v)", k, seen[k], seen)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func post(h http.HandlerFunc, body string) (*httptest.ResponseRecorder, time.Duration) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	t0 := time.Now()
	h(w, r)
	return w, time.Since(t0)
}

func TestTheForgotAnswerIsTheSameWithOrWithoutAnAccount(t *testing.T) {
	s, _, mb := resetService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	mkUser(t, s, "zoe@example.com", RoleMember)

	known, tk := post(h.forgotPassword, `{"email":"zoe@example.com"}`)
	unknown, tu := post(h.forgotPassword, `{"email":"nobody@example.com"}`)
	if known.Code != http.StatusAccepted || known.Code != unknown.Code || known.Body.String() != unknown.Body.String() {
		t.Fatalf("known: %d %s / unknown: %d %s", known.Code, known.Body, unknown.Code, unknown.Body)
	}
	if tk < resetAnswerAfter || tu < resetAnswerAfter {
		t.Fatalf("answered in %v and %v, under the %v floor", tk, tu, resetAnswerAfter)
	}
	if d := tk - tu; d > 100*time.Millisecond || d < -100*time.Millisecond {
		t.Fatalf("an account answers %v slower than none", d)
	}
	if mb.count.Load() != 1 {
		t.Fatalf("%d links mailed", mb.count.Load())
	}
	if w, _ := post(h.forgotPassword, `{"email":"`+strings.Repeat("a", 330)+`@example.com"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized address: %d", w.Code)
	}
}

func TestTheResetEndpointEndsThisBrowsersSessionToo(t *testing.T) {
	s, _, mb := resetService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), true, false)
	mkUser(t, s, "zoe@example.com", RoleMember)
	_ = s.RequestPasswordReset(context.Background(), "zoe@example.com", "", "")
	_, token := mb.last(t)

	body, _ := json.Marshal(map[string]string{"token": token, "password": newPassword})
	w, _ := post(h.resetPassword, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the session cookie was not cleared")
	}
	if w, _ := post(h.resetPassword, string(body)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reset_invalid") {
		t.Fatalf("second use: %d %s", w.Code, w.Body)
	}
	if w, _ := post(h.resetPassword, `{"token":"`+strings.Repeat("x", 200)+`","password":"`+newPassword+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized token: %d", w.Code)
	}
}

func TestConfigSaysWhetherResetIsOffered(t *testing.T) {
	s, _ := testService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	read := func() bool {
		w := httptest.NewRecorder()
		h.config(w, httptest.NewRequest(http.MethodGet, "/", nil))
		var b map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return b["passwordReset"] == true
	}
	if read() {
		t.Fatal("reset offered with no mail configured")
	}
	s.SendReset = func(string, string) {}
	if !read() {
		t.Fatal("reset not offered with mail configured")
	}
}
