package identity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheBlockListRefusesSilentlyAndCoversSubdomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.txt")
	if err := os.WriteFile(path, []byte("# throwaway mail\nthrowaway.example\n@Burner.Example  # trailing comment\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, err := LoadBlockedDomains(path)
	if err != nil || len(blocked) != 2 {
		t.Fatalf("loaded %v %v", blocked, err)
	}
	s, ob := verifiedService(t)
	s.Guard = NewSignupGuard(blocked)
	s.Guard.LookupDomain = func(context.Context, string) bool { return true }
	ctx := context.Background()
	for _, e := range []string{"a@throwaway.example", "b@mail.throwaway.example", "c@burner.example", "d@THROWAWAY.EXAMPLE"} {
		if _, err := s.SignUp(ctx, e, "X", pwOwner, "", ""); !errors.Is(err, ErrSignupPending) {
			t.Fatalf("%s: %v (the same answer as a new address)", e, err)
		}
	}
	if n := len(ob.links); n != 0 {
		t.Fatalf("%d links mailed to blocked domains", n)
	}
	// A look-alike that is not on the list is not blocked.
	if _, err := s.SignUp(ctx, "ok@notthrowaway.example", "X", pwOwner, "", ""); !errors.Is(err, ErrSignupPending) || len(ob.links["ok@notthrowaway.example"]) != 1 {
		t.Fatalf("a domain off the list: %v, %d links", err, len(ob.links["ok@notthrowaway.example"]))
	}
	if SignupTotals()["blocked"] < 4 {
		t.Fatalf("blocked count %d", SignupTotals()["blocked"])
	}
}

func TestADomainWithNoMailServerGetsNoEmail(t *testing.T) {
	s, ob := verifiedService(t)
	s.Guard = NewSignupGuard(nil)
	var asked []string
	s.Guard.LookupDomain = func(_ context.Context, d string) bool { asked = append(asked, d); return d == "example.com" }
	ctx := context.Background()
	if _, err := s.SignUp(ctx, "zoe@nowhere.invalid", "Zoe", pwOwner, "", ""); !errors.Is(err, ErrSignupPending) {
		t.Fatalf("no-mail domain: %v", err)
	}
	if len(ob.links) != 0 {
		t.Fatal("mail was sent to a domain that cannot receive it")
	}
	if _, err := s.SignUp(ctx, "zoe@example.com", "Zoe", pwOwner, "", ""); err == nil || len(ob.links["zoe@example.com"]) != 1 {
		t.Fatalf("a real domain: %v, %d links", err, len(ob.links["zoe@example.com"]))
	}
	if strings.Join(asked, ",") != "nowhere.invalid,example.com" {
		t.Fatalf("looked up %v", asked)
	}
	// An address that already has an account is never looked up: the
	// check is for what would be mailed, and it is not mailed a link.
	asked = nil
	mkUser(t, s, "taken@example.com", RoleMember)
	_, _ = s.SignUp(ctx, "taken@example.com", "X", pwOwner, "", "")
	if len(asked) != 0 {
		t.Fatalf("an existing address was looked up: %v", asked)
	}
}

func TestTheRealMailCheckKnowsAMailDomainFromAMadeUpOne(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx := context.Background()
	if !domainReceivesMail(ctx, "gmail.com") {
		t.Skip("no network: gmail.com has no MX from here")
	}
	if domainReceivesMail(ctx, "no-such-domain-"+strings.ToLower(t.Name())+".invalid") {
		t.Fatal("a .invalid domain receives mail?")
	}
}

func TestTheGlobalCeilingBoundsSignUpsAcrossAddresses(t *testing.T) {
	s, _ := testService(t)
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	h.AllowSignup = true
	h.SetSignupCeiling(3)
	codes := []int{}
	for i := 0; i < 5; i++ {
		w, _ := post(h.signup, `{"email":"p`+string(rune('a'+i))+`@example.com","displayName":"P","password":"`+pwOwner+`"}`)
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusCreated || codes[2] != http.StatusCreated || codes[3] != http.StatusTooManyRequests || codes[4] != http.StatusTooManyRequests {
		t.Fatalf("codes %v: want three accepted then 429", codes)
	}
	// Tokens come back at the configured rate.
	h.signupCeiling.last = h.signupCeiling.last.Add(-time.Minute)
	if w, _ := post(h.signup, `{"email":"pz@example.com","displayName":"P","password":"`+pwOwner+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("after a minute: %d", w.Code)
	}
	h.SetSignupCeiling(0)
	if w, _ := post(h.signup, `{"email":"py@example.com","displayName":"P","password":"`+pwOwner+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("no ceiling: %d", w.Code)
	}
}

func TestSignupCountsFollowOutcomes(t *testing.T) {
	before := SignupTotals()
	s, ob := verifiedService(t)
	ctx := context.Background()
	mkUser(t, s, "taken@example.com", RoleMember)
	_, _ = s.SignUp(ctx, "taken@example.com", "X", pwOwner, "", "")
	_, _ = s.SignUp(ctx, "new@example.com", "X", pwOwner, "", "")
	if _, err := s.CompleteSignup(ctx, ob.last(t, "new@example.com"), pwOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	after := SignupTotals()
	for _, k := range []string{"existing", "pending", "completed"} {
		if after[k] != before[k]+1 {
			t.Errorf("%s: %d -> %d", k, before[k], after[k])
		}
	}
}
