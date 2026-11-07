package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// outbox captures what the sign-up mailers would have sent.
type outbox struct {
	mu      sync.Mutex
	links   map[string][]string // email -> tokens, oldest first
	notices map[string]int
}

func verifiedService(t *testing.T) (*Service, *outbox) {
	t.Helper()
	s, _ := testService(t)
	ob := &outbox{links: map[string][]string{}, notices: map[string]int{}}
	s.SendVerify = func(email, token string) {
		ob.mu.Lock()
		ob.links[email] = append(ob.links[email], token)
		ob.mu.Unlock()
	}
	s.SendSignupNotice = func(email string) {
		ob.mu.Lock()
		ob.notices[email]++
		ob.mu.Unlock()
	}
	return s, ob
}

func (o *outbox) last(t *testing.T, email string) string {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	l := o.links[email]
	if len(l) == 0 {
		t.Fatalf("no link mailed to %s", email)
	}
	return l[len(l)-1]
}

const pwOwner, pwAttacker = "owner-chose-this-one", "attacker-chose-this"

func TestASignUpCreatesNothingUntilItsLinkIsOpenedWithItsPassword(t *testing.T) {
	s, ob := verifiedService(t)
	ctx := context.Background()
	if _, err := s.SignUp(ctx, "Zoe@Example.com", "Zoe", pwOwner, "", ""); !errors.Is(err, ErrSignupPending) {
		t.Fatalf("sign-up: %v", err)
	}
	if _, err := s.repo.UserByEmail(ctx, "zoe@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an account exists before its address was proven")
	}
	if _, _, err := s.Authenticate(ctx, "zoe@example.com", pwOwner, "", ""); err == nil {
		t.Fatal("signed in before the link was opened")
	}
	tok := ob.last(t, "zoe@example.com")
	if _, err := s.CompleteSignup(ctx, tok, "not-the-password-chosen", "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatalf("the link opened with another password: %v", err)
	}
	u, err := s.CompleteSignup(ctx, tok, pwOwner, "", "")
	if err != nil || u.Email != "zoe@example.com" || u.DisplayName != "Zoe" || u.Role != RoleMember || u.Status != StatusActive {
		t.Fatalf("complete: %+v %v", u, err)
	}
	if _, _, err := s.Authenticate(ctx, "zoe@example.com", pwOwner, "", ""); err != nil {
		t.Fatalf("the new account does not sign in: %v", err)
	}
	if _, err := s.CompleteSignup(ctx, tok, pwOwner, "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatal("a link worked twice")
	}
}

// The attack the password check exists for: a stranger re-submits sign-up
// for someone else's address with their own password. Their link goes to the
// owner's inbox; if the owner opens it, it must not finish with the
// stranger's password, and the owner's own link must still work.
func TestAStrangersSignUpForYourAddressCannotBecomeYourAccount(t *testing.T) {
	s, ob := verifiedService(t)
	ctx := context.Background()
	_, _ = s.SignUp(ctx, "zoe@example.com", "Zoe", pwOwner, "", "")
	owners := ob.last(t, "zoe@example.com")
	_, _ = s.SignUp(ctx, "zoe@example.com", "Not Zoe", pwAttacker, "", "")
	strangers := ob.last(t, "zoe@example.com")
	if strangers == owners {
		t.Fatal("one link for two sign-ups")
	}
	if _, err := s.CompleteSignup(ctx, strangers, pwOwner, "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatalf("the stranger's link finished with the owner's password: %v", err)
	}
	u, err := s.CompleteSignup(ctx, owners, pwOwner, "", "")
	if err != nil || u.DisplayName != "Zoe" {
		t.Fatalf("the owner's own link: %+v %v", u, err)
	}
	if _, _, err := s.Authenticate(ctx, "zoe@example.com", pwAttacker, "", ""); err == nil {
		t.Fatal("the stranger's password signs in to the owner's account")
	}
	if _, err := s.CompleteSignup(ctx, strangers, pwAttacker, "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatal("a second account was made from a cleared link")
	}
}

func TestAnAddressWithAnAccountGetsANoticeNotALink(t *testing.T) {
	s, ob := verifiedService(t)
	ctx := context.Background()
	mkUser(t, s, "taken@example.com", RoleMember)
	for i := 0; i < 3; i++ {
		if _, err := s.SignUp(ctx, "taken@example.com", "X", pwAttacker, "", ""); !errors.Is(err, ErrSignupPending) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if len(ob.links["taken@example.com"]) != 0 {
		t.Fatal("a link was mailed for an address that has an account")
	}
	if ob.notices["taken@example.com"] != 1 {
		t.Fatalf("%d notices for three attempts in a minute, want 1", ob.notices["taken@example.com"])
	}
	if _, _, err := s.Authenticate(ctx, "taken@example.com", pwAttacker, "", ""); err == nil {
		t.Fatal("the attempt changed the existing account's password")
	}
}

func TestPendingSignUpsAreBoundedAndExpire(t *testing.T) {
	s, ob := verifiedService(t)
	ctx := context.Background()
	for i := 0; i < maxPendingPerEmail+2; i++ {
		_, _ = s.SignUp(ctx, "zoe@example.com", "Zoe", fmt.Sprintf("password-number-%02d", i), "", "")
		time.Sleep(2 * time.Millisecond) // distinct created_at
	}
	toks := ob.links["zoe@example.com"]
	if _, err := s.CompleteSignup(ctx, toks[0], "password-number-00", "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatal("the oldest of more than the bound still works")
	}
	if _, err := s.CompleteSignup(ctx, toks[len(toks)-1], fmt.Sprintf("password-number-%02d", len(toks)-1), "", ""); err != nil {
		t.Fatalf("the newest: %v", err)
	}

	past := time.Now().Add(-SignupTTL - time.Hour)
	hash, _ := HashPassword(pwOwner)
	if err := s.repo.SavePendingSignup(ctx, HashToken("old-link"), "late@example.com", "Late", hash, past, past.Add(SignupTTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteSignup(ctx, "old-link", pwOwner, "", ""); !errors.Is(err, ErrSignupLink) {
		t.Fatalf("an expired link: %v", err)
	}
}

func TestWithoutMailTheOrganisationsDomainsAreRefused(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	s.OrgDomains = func(context.Context) []string { return []string{"our-org.com"} }
	if _, err := s.SignUp(ctx, "alice@OUR-ORG.com", "Alice", pwOwner, "", ""); !errors.Is(err, ErrSignupOrgDomain) {
		t.Fatalf("an unverifiable org address: %v", err)
	}
	if _, err := s.repo.UserByEmail(ctx, "alice@our-org.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the org address was created anyway")
	}
	if u, err := s.SignUp(ctx, "visitor@elsewhere.com", "Visitor", pwOwner, "", ""); err != nil || u == nil {
		t.Fatalf("an outside address without mail (the old behaviour): %v", err)
	}
}

func TestTheNoticeThrottleNeverGrowsPastItsBound(t *testing.T) {
	var n noticeThrottle
	now := time.Now()
	for i := 0; i < noticeMax+500; i++ {
		n.allow(fmt.Sprintf("a%d@example.com", i), now)
	}
	if len(n.last) > noticeMax {
		t.Fatalf("%d entries, bound %d", len(n.last), noticeMax)
	}
}

func TestSignUpAnswersTheSameForANewAndAKnownAddress(t *testing.T) {
	s, ob := verifiedService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), true, false)
	h.AllowSignup = true
	mkUser(t, s, "taken@example.com", RoleMember)
	body := func(e string) string {
		return `{"email":"` + e + `","displayName":"X","password":"` + pwOwner + `"}`
	}
	fresh, tf := post(h.signup, body("fresh@example.com"))
	known, tk := post(h.signup, body("taken@example.com"))
	if fresh.Code != http.StatusAccepted || fresh.Code != known.Code || fresh.Body.String() != known.Body.String() {
		t.Fatalf("new: %d %s / known: %d %s", fresh.Code, fresh.Body, known.Code, known.Body)
	}
	if tf < signupAnswerAfter || tk < signupAnswerAfter {
		t.Fatalf("answered in %v and %v, under the %v floor", tf, tk, signupAnswerAfter)
	}
	if d := tf - tk; d > 150*time.Millisecond || d < -150*time.Millisecond {
		t.Fatalf("a new address answers %v apart from a known one", d)
	}
	for _, c := range fresh.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Fatal("a session was started before the address was proven")
		}
	}

	w, _ := post(h.completeSignup, `{"token":"`+ob.last(t, "fresh@example.com")+`","password":"`+pwOwner+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	signedIn := false
	for _, c := range w.Result().Cookies() {
		signedIn = signedIn || (c.Name == SessionCookie && c.Value != "")
	}
	if !signedIn || !strings.Contains(w.Body.String(), `"csrf"`) {
		t.Fatalf("opening the link did not sign the person in: %s", w.Body)
	}
	if w, _ := post(h.completeSignup, `{"token":"`+strings.Repeat("x", 200)+`","password":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized token: %d", w.Code)
	}
}
