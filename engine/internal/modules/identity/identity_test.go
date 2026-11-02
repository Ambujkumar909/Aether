package identity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

func testService(t *testing.T) (*Service, *audit.Service) {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "id.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	au := audit.NewService(audit.NewSQLRepository(db), quiet)
	if err := au.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(au.Stop)

	return NewService(NewSQLRepository(db), au, quiet), au
}

func mkUser(t *testing.T, s *Service, email string, role Role) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), nil, email, email,
		"correct-horse-battery", role, "10.0.0.1", "test")
	if err != nil {
		t.Fatalf("create %s: %v", email, err)
	}
	return u
}

// ---------------------------------------------------------------------------
// Password handling
// ---------------------------------------------------------------------------

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("a-perfectly-fine-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("a-perfectly-fine-passphrase", h) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("a-perfectly-fine-passphras", h) {
		t.Fatal("wrong password accepted")
	}
	// Two hashes of the same password must differ: a shared salt would let
	// an attacker spot users who chose the same password.
	h2, _ := HashPassword("a-perfectly-fine-passphrase")
	if h == h2 {
		t.Fatal("identical hashes for the same password; the salt is not random")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$aaaa$bbbb",
		"$argon2id$v=19$m=1,t=1,p=1$!!!!$bbbb",
		"$argon2id$v=19$badparams$aaaa$bbbb",
	} {
		if VerifyPassword("anything", bad) {
			t.Fatalf("malformed hash %q was accepted", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("accepted a short password")
	}
	if err := ValidatePassword("aaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("accepted a single repeated character")
	}
	if err := ValidatePassword("                "); err == nil {
		t.Fatal("accepted whitespace only")
	}
	if err := ValidatePassword("a reasonable passphrase"); err != nil {
		t.Fatalf("rejected a good passphrase: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// A failed login must not disclose whether the account exists. Any difference
// -- error text, error type, or timing -- hands an attacker a free way to
// enumerate valid addresses before ever guessing a password.
func TestLoginDoesNotRevealAccountExistence(t *testing.T) {
	s, _ := testService(t)
	mkUser(t, s, "real@example.com", RoleMember)
	ctx := context.Background()

	_, _, errKnown := s.Authenticate(ctx, "real@example.com", "wrong-password-here", "1.1.1.1", "t")
	_, _, errUnknown := s.Authenticate(ctx, "ghost@example.com", "wrong-password-here", "1.1.1.1", "t")

	if !errors.Is(errKnown, ErrInvalidCredential) || !errors.Is(errUnknown, ErrInvalidCredential) {
		t.Fatalf("different errors: known=%v unknown=%v", errKnown, errUnknown)
	}
	if errKnown.Error() != errUnknown.Error() {
		t.Fatalf("error text differs and leaks existence: %q vs %q",
			errKnown.Error(), errUnknown.Error())
	}
}

func TestSuccessfulLoginIssuesUsableSession(t *testing.T) {
	s, _ := testService(t)
	mkUser(t, s, "alice@example.com", RoleMember)
	ctx := context.Background()

	u, token, err := s.Authenticate(ctx, "alice@example.com", "correct-horse-battery", "1.1.1.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("no session token issued")
	}
	got, sess, err := s.Authorize(ctx, token)
	if err != nil {
		t.Fatalf("fresh session rejected: %v", err)
	}
	if got.ID != u.ID || sess.UserID != u.ID {
		t.Fatal("session resolved to the wrong user")
	}
	// The token must not be recoverable from storage.
	stored, err := s.repo.SessionByTokenHash(ctx, HashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	if stored.tokenHash == token {
		t.Fatal("the raw session token is stored; a database dump would hand over live sessions")
	}
}

func TestEmailIsCaseInsensitive(t *testing.T) {
	s, _ := testService(t)
	mkUser(t, s, "Mixed.Case@Example.COM", RoleMember)
	ctx := context.Background()
	if _, _, err := s.Authenticate(ctx, "mixed.case@example.com",
		"correct-horse-battery", "1.1.1.1", "t"); err != nil {
		t.Fatalf("case-different email rejected: %v", err)
	}
	// And a duplicate in different case must be refused.
	_, err := s.CreateUser(ctx, nil, "MIXED.CASE@EXAMPLE.COM", "x",
		"correct-horse-battery", RoleMember, "1.1.1.1", "t")
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email in different case was allowed: %v", err)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	s, _ := testService(t)
	mkUser(t, s, "target@example.com", RoleMember)
	ctx := context.Background()

	for i := 0; i < MaxFailedLogins; i++ {
		_, _, _ = s.Authenticate(ctx, "target@example.com", "nope-nope-nope", "1.1.1.1", "t")
	}
	// Even the correct password must now be refused.
	if _, _, err := s.Authenticate(ctx, "target@example.com",
		"correct-horse-battery", "1.1.1.1", "t"); err == nil {
		t.Fatal("account was not locked after repeated failures")
	}

	u, err := s.repo.UserByEmail(ctx, "target@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Locked(time.Now().UTC()) {
		t.Fatal("lock was not recorded")
	}
	// The lock must be temporary, or knowing an email address is a denial of
	// service against its owner.
	if u.LockedUntil.After(time.Now().Add(LockoutDuration + time.Minute)) {
		t.Fatal("lockout is effectively permanent")
	}
}

func TestSuspensionEndsLiveSessionsImmediately(t *testing.T) {
	s, _ := testService(t)
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	victim := mkUser(t, s, "victim@example.com", RoleMember)
	ctx := context.Background()

	_, token, err := s.Authenticate(ctx, "victim@example.com", "correct-horse-battery", "1.1.1.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authorize(ctx, token); err != nil {
		t.Fatal("session should be valid before suspension")
	}

	if _, err := s.SetStatus(ctx, admin, victim.ID, StatusSuspended, "1.1.1.1", "t"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authorize(ctx, token); err == nil {
		t.Fatal("a suspended user kept a working session; suspension must take effect at once")
	}
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	s, _ := testService(t)
	u := mkUser(t, s, "user@example.com", RoleMember)
	ctx := context.Background()

	_, tokenA, _ := s.Authenticate(ctx, "user@example.com", "correct-horse-battery", "1.1.1.1", "a")
	_, tokenB, _ := s.Authenticate(ctx, "user@example.com", "correct-horse-battery", "2.2.2.2", "b")

	if err := s.ChangePassword(ctx, u, u.ID, "correct-horse-battery",
		"an-entirely-different-passphrase", "1.1.1.1", "a"); err != nil {
		t.Fatal(err)
	}
	// Changing a password is how someone evicts an intruder. Both sessions
	// must die, including the attacker's.
	if _, _, err := s.Authorize(ctx, tokenA); err == nil {
		t.Fatal("session A survived a password change")
	}
	if _, _, err := s.Authorize(ctx, tokenB); err == nil {
		t.Fatal("session B survived a password change")
	}
}

func TestSelfPasswordChangeRequiresCurrentPassword(t *testing.T) {
	s, _ := testService(t)
	u := mkUser(t, s, "user@example.com", RoleMember)
	err := s.ChangePassword(context.Background(), u, u.ID, "not-the-current-one",
		"a-brand-new-passphrase", "1.1.1.1", "t")
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("changed own password without proving the old one: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorisation
// ---------------------------------------------------------------------------

func TestPermissionTable(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleGuest, PermJoinMeeting, true},
		{RoleGuest, PermHostMeeting, false},
		{RoleGuest, PermUserRead, false},
		{RoleMember, PermHostMeeting, true},
		{RoleMember, PermUserRead, false},
		{RoleModerator, PermUserRead, true},
		{RoleModerator, PermUserWrite, false},
		{RoleModerator, PermAuditRead, true},
		{RoleModerator, PermAuditExport, false},
		{RoleAdmin, PermUserDelete, true},
		{RoleAdmin, PermAuditExport, true},
	}
	for _, c := range cases {
		if got := c.role.Can(c.perm); got != c.want {
			t.Errorf("%s.Can(%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestCannotEscalateAboveYourself(t *testing.T) {
	s, _ := testService(t)
	mod := mkUser(t, s, "mod@example.com", RoleModerator)
	member := mkUser(t, s, "member@example.com", RoleMember)
	ctx := context.Background()

	// A moderator cannot write users at all.
	if _, err := s.SetRole(ctx, mod, member.ID, RoleAdmin, "1.1.1.1", "t"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("moderator changed a role: %v", err)
	}
	// Nor mint an account above their own rank.
	if _, err := s.CreateUser(ctx, mod, "new@example.com", "n",
		"correct-horse-battery", RoleAdmin, "1.1.1.1", "t"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("moderator created an admin: %v", err)
	}
}

func TestAdminCannotChangeOwnRole(t *testing.T) {
	s, _ := testService(t)
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	_, err := s.SetRole(context.Background(), admin, admin.ID, RoleMember, "1.1.1.1", "t")
	if !errors.Is(err, ErrSelfDemotion) {
		t.Fatalf("admin changed their own role: %v", err)
	}
}

// Locking everyone out of their own installation cannot be undone from
// inside the product, so it must be refused.
func TestCannotRemoveTheLastAdministrator(t *testing.T) {
	s, _ := testService(t)
	a1 := mkUser(t, s, "a1@example.com", RoleAdmin)
	a2 := mkUser(t, s, "a2@example.com", RoleAdmin)
	ctx := context.Background()

	// Two admins: demoting one is fine.
	if _, err := s.SetRole(ctx, a1, a2.ID, RoleMember, "1.1.1.1", "t"); err != nil {
		t.Fatalf("demoting one of two admins failed: %v", err)
	}
	// One admin left: every route to zero must be refused.
	other := mkUser(t, s, "other@example.com", RoleAdmin)
	if _, err := s.SetRole(ctx, other, a1.ID, RoleMember, "1.1.1.1", "t"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := s.DeleteUser(ctx, other, other.ID, "1.1.1.1", "t"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin deleted themselves: %v", err)
	}
	last := mkUser(t, s, "last@example.com", RoleAdmin)
	if _, err := s.SetRole(ctx, last, other.ID, RoleMember, "1.1.1.1", "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, last, last.ID, "1.1.1.1", "t"); err == nil {
		t.Fatal("deleted the last administrator")
	}
}

func TestRoleChangeRevokesSessions(t *testing.T) {
	s, _ := testService(t)
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	target := mkUser(t, s, "target@example.com", RoleModerator)
	ctx := context.Background()

	_, token, err := s.Authenticate(ctx, "target@example.com", "correct-horse-battery", "1.1.1.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(ctx, admin, target.ID, RoleMember, "1.1.1.1", "t"); err != nil {
		t.Fatal(err)
	}
	// A demoted user must not keep a session carrying the old rights.
	if _, _, err := s.Authorize(ctx, token); err == nil {
		t.Fatal("a demoted user kept their elevated session")
	}
}

// ---------------------------------------------------------------------------
// The audit trail must actually contain what happened
// ---------------------------------------------------------------------------

func TestSecurityEventsAreAudited(t *testing.T) {
	s, au := testService(t)
	admin := mkUser(t, s, "admin@example.com", RoleAdmin)
	ctx := context.Background()

	_, _, _ = s.Authenticate(ctx, "admin@example.com", "wrong-password", "9.9.9.9", "t")
	_, _, _ = s.Authenticate(ctx, "admin@example.com", "correct-horse-battery", "9.9.9.9", "t")
	victim := mkUser(t, s, "victim@example.com", RoleMember)
	_, _ = s.SetRole(ctx, admin, victim.ID, RoleModerator, "9.9.9.9", "t")

	want := map[audit.Action]bool{
		audit.ActionUserCreated:     false,
		audit.ActionLoginFailed:     false,
		audit.ActionLoginSucceeded:  false,
		audit.ActionUserRoleChanged: false,
	}
	events, err := au.List(ctx, audit.Filter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, ok := want[e.Action]; ok {
			want[e.Action] = true
		}
	}
	for a, seen := range want {
		if !seen {
			t.Errorf("%s was never recorded in the audit log", a)
		}
	}

	// And the chain covering those events must verify.
	res, err := au.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("audit chain broken after identity activity: %s", res.Detail)
	}
	t.Logf("%d security events recorded, chain intact", res.Checked)
}

func TestDeniedActionsAreAuditedToo(t *testing.T) {
	s, au := testService(t)
	mod := mkUser(t, s, "mod@example.com", RoleModerator)
	member := mkUser(t, s, "member@example.com", RoleMember)
	ctx := context.Background()

	_, _ = s.SetRole(ctx, mod, member.ID, RoleAdmin, "1.1.1.1", "t")

	events, err := au.List(ctx, audit.Filter{Outcome: audit.OutcomeDenied})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("a refused privilege escalation left no audit trail")
	}
	t.Logf("refusal recorded: %s", audit.Describe(events[0]))
}
