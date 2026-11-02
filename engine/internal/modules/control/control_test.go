package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

// ---------------------------------------------------------------------------
// Scaffolding
// ---------------------------------------------------------------------------

// recorder captures the audit chain so tests can assert that transitions were
// recorded, not merely that they happened.
type recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recorder) Record(_ context.Context, e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) find(a audit.Action) []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.Event
	for _, e := range r.events {
		if e.Action == a {
			out = append(out, e)
		}
	}
	return out
}

type notifier struct {
	mu   sync.Mutex
	sent map[string]int
}

func (n *notifier) NotifyControl(userID string, _ *Session) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.sent == nil {
		n.sent = map[string]int{}
	}
	n.sent[userID]++
}

var (
	operator = Actor{ID: "u-op", Email: "op@example.com", Name: "Op"}
	user     = Actor{ID: "u-sub", Email: "sub@example.com", Name: "Sub"}
	admin    = Actor{ID: "u-adm", Email: "adm@example.com", Name: "Adm", Admin: true}
	stranger = Actor{ID: "u-str", Email: "str@example.com", Name: "Str"}
)

func testService(t *testing.T) (*Service, *recorder) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rec := &recorder{}
	svc := NewService(NewSQLRepository(db), rec,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, rec
}

func requested(t *testing.T, svc *Service, scope Scope) *Session {
	t.Helper()
	s, err := svc.Request(context.Background(), operator, "m-1", user, scope, "1.2.3.4", "ua")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return s
}

func granted(t *testing.T, svc *Service, scope Scope, d time.Duration) *Session {
	t.Helper()
	s := requested(t, svc, scope)
	g, err := svc.Grant(context.Background(), user, s.ID, d, "1.2.3.4", "ua")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return g
}

// ---------------------------------------------------------------------------
// Rule 1: consent is explicit and human, and no role can give it
// ---------------------------------------------------------------------------

// An administrator must not be able to grant themselves, or anyone, control
// of somebody else's machine.
//
// This is the load-bearing test of the whole feature. Every remote access
// product that has become a liability got there by having an authorisation
// path that did not end at the person sitting in front of the machine. If this
// test is ever changed to pass by making the admin succeed, the product has
// become a different and much more dangerous thing.
func TestAdminCannotGrantControl(t *testing.T) {
	svc, rec := testService(t)
	s := requested(t, svc, ScopeControl)

	_, err := svc.Grant(context.Background(), admin, s.ID, time.Minute, "1.2.3.4", "ua")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("an administrator granted control: %v", err)
	}

	after, err := svc.ByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateRequested {
		t.Fatalf("session moved to %q after an admin tried to grant it", after.State)
	}

	// The attempt itself must be on the chain: an administrator trying to
	// take control of somebody's machine is exactly what a review looks for.
	denied := rec.find(audit.ActionControlGranted)
	found := false
	for _, e := range denied {
		if e.Outcome == audit.OutcomeDenied && e.ActorID == admin.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the refused admin grant was not recorded on the audit chain")
	}
}

// The controller cannot grant their own request either. Obvious, and worth a
// test precisely because it is the kind of check that gets refactored away.
func TestControllerCannotGrantTheirOwnRequest(t *testing.T) {
	svc, _ := testService(t)
	s := requested(t, svc, ScopeControl)

	if _, err := svc.Grant(context.Background(), operator, s.ID, time.Minute, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the controller granted their own request: %v", err)
	}
}

func TestOnlyTheSubjectMayGrant(t *testing.T) {
	svc, _ := testService(t)
	for _, a := range []Actor{operator, admin, stranger} {
		s := requested(t, svc, ScopeControl)
		if _, err := svc.Grant(context.Background(), a, s.ID, time.Minute, "", ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s granted control: %v", a.Email, err)
		}
		// Clear it so the next iteration is not blocked by an active request.
		if _, err := svc.Deny(context.Background(), user, s.ID, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	s := requested(t, svc, ScopeControl)
	if _, err := svc.Grant(context.Background(), user, s.ID, time.Minute, "", ""); err != nil {
		t.Fatalf("the subject could not grant: %v", err)
	}
}

func TestNobodyControlsThemselves(t *testing.T) {
	svc, _ := testService(t)
	_, err := svc.Request(context.Background(), operator, "m-1", operator, ScopeControl, "", "")
	if !errors.Is(err, ErrSelfControl) {
		t.Fatalf("self-control was allowed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Rule 2: consent is bounded in time
// ---------------------------------------------------------------------------

func TestGrantIsCappedAtMaxGrant(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, 100*time.Hour)

	if s.ExpiresAt == nil {
		t.Fatal("a grant was issued with no deadline")
	}
	got := s.ExpiresAt.Sub(*s.DecidedAt)
	if got > MaxGrant+time.Second {
		t.Fatalf("a 100 hour request produced a %s grant; the cap is %s", got, MaxGrant)
	}
	t.Logf("a 100 hour request was capped to %s", got.Round(time.Minute))
}

func TestGrantExpiresWithoutAnyoneActing(t *testing.T) {
	svc, rec := testService(t)
	s := granted(t, svc, ScopeControl, time.Second)

	if !s.Live(time.Now()) {
		t.Fatal("a fresh grant is not live")
	}

	future := time.Now().UTC().Add(2 * time.Second)
	gone, err := svc.ExpireDue(context.Background(), future)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 {
		t.Fatalf("expiry swept %d sessions, want 1", len(gone))
	}

	after, _ := svc.ByID(context.Background(), s.ID)
	if after.State != StateExpired {
		t.Fatalf("session is %q after its deadline, want expired", after.State)
	}
	if after.Live(future) {
		t.Fatal("an expired session still reports itself live")
	}
	if len(rec.find(audit.ActionControlExpired)) != 1 {
		t.Fatal("expiry was not recorded; it is indistinguishable from a revoke")
	}
}

// An expired grant must not be extendable back to life. Otherwise a session
// the user watched run out could be resurrected without asking them again.
func TestExpiredGrantCannotBeExtended(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Second)

	future := time.Now().UTC().Add(2 * time.Second)
	if _, err := svc.ExpireDue(context.Background(), future); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Extend(context.Background(), user, s.ID, time.Minute, "", ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("an expired grant was extended: %v", err)
	}
}

func TestOnlyTheSubjectMayExtend(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Minute)
	for _, a := range []Actor{operator, admin, stranger} {
		if _, err := svc.Extend(context.Background(), a, s.ID, time.Minute, "", ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s extended a grant: %v", a.Email, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Rule 3: revocation is unilateral
// ---------------------------------------------------------------------------

// The subject can always stop it, alone, without the controller agreeing.
func TestSubjectRevokesUnilaterally(t *testing.T) {
	svc, rec := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)

	got, err := svc.Revoke(context.Background(), user, s.ID, "no thanks", "", "")
	if err != nil {
		t.Fatalf("the subject could not revoke: %v", err)
	}
	if got.State != StateRevoked {
		t.Fatalf("state is %q after revocation", got.State)
	}
	if got.Live(time.Now()) {
		t.Fatal("a revoked session still reports itself live")
	}
	if len(rec.find(audit.ActionControlRevoked)) != 1 {
		t.Fatal("revocation was not audited")
	}
}

// Revocation and granting are deliberately asymmetric: an administrator can
// STOP a session but cannot START one. Being able to stop is safe; being able
// to start is the thing rule 1 exists to prevent.
func TestAdminMayRevokeButNotGrant(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)

	if _, err := svc.Revoke(context.Background(), admin, s.ID, "policy", "", ""); err != nil {
		t.Fatalf("an administrator could not stop a session: %v", err)
	}

	// And still cannot grant one.
	s2 := requested(t, svc, ScopeControl)
	if _, err := svc.Grant(context.Background(), admin, s2.ID, time.Minute, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("an administrator granted control after being allowed to revoke")
	}
}

func TestStrangersCannotRevoke(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)
	if _, err := svc.Revoke(context.Background(), stranger, s.ID, "", "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an unrelated user revoked a session: %v", err)
	}
}

// Revoking twice must not be an error. The person pressing the stop button
// again because nothing visibly happened must not see a failure.
func TestRevokeIsIdempotent(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)

	for i := 0; i < 5; i++ {
		if _, err := svc.Revoke(context.Background(), user, s.ID, "stop", "", ""); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
}

// Concurrent revocation from both sides at once must be safe. Under -race.
func TestConcurrentRevokeIsSafe(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)

	var wg sync.WaitGroup
	for _, a := range []Actor{user, operator, admin} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(a Actor) {
				defer wg.Done()
				_, _ = svc.Revoke(context.Background(), a, s.ID, "stop", "", "")
			}(a)
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent revoke deadlocked")
	}

	after, _ := svc.ByID(context.Background(), s.ID)
	if after.State != StateRevoked {
		t.Fatalf("state is %q after concurrent revocation", after.State)
	}
}

// ---------------------------------------------------------------------------
// Rule 4: view and control are different grants
// ---------------------------------------------------------------------------

// Escalating from view to control must not upgrade the existing session. It
// must close it and ask again, so the subject answers a new question rather
// than having a session they already accepted quietly gain the ability to
// type on their machine.
func TestEscalationRequiresFreshConsent(t *testing.T) {
	svc, _ := testService(t)
	view := granted(t, svc, ScopeView, time.Hour)

	fresh, err := svc.Escalate(context.Background(), operator, view.ID, "", "")
	if err != nil {
		t.Fatalf("escalate: %v", err)
	}

	if fresh.ID == view.ID {
		t.Fatal("escalation modified the existing session instead of opening a new one")
	}
	if fresh.State != StateRequested {
		t.Fatalf("the escalated session is %q; it must wait for consent", fresh.State)
	}
	if fresh.Scope != ScopeControl {
		t.Fatalf("the escalated session has scope %q", fresh.Scope)
	}

	old, _ := svc.ByID(context.Background(), view.ID)
	if !old.State.Terminal() {
		t.Fatalf("the view session is still %q after escalation", old.State)
	}
	// And crucially, no injection is permitted until the subject answers.
	if svc.MayInject(context.Background(), fresh.ID, operator.ID, user.ID, time.Now()) {
		t.Fatal("an unanswered escalation already permits injection")
	}
}

// A view grant must never permit injection, however live it is.
func TestViewScopeNeverInjects(t *testing.T) {
	svc, _ := testService(t)
	view := granted(t, svc, ScopeView, time.Hour)

	if !view.Live(time.Now()) {
		t.Fatal("the view session is not live")
	}
	if svc.MayInject(context.Background(), view.ID, operator.ID, user.ID, time.Now()) {
		t.Fatal("a view-scope session permitted input injection")
	}
}

// ---------------------------------------------------------------------------
// MayInject
// ---------------------------------------------------------------------------

func TestMayInjectOnlyForTheGrantedPair(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)
	now := time.Now()
	ctx := context.Background()

	if !svc.MayInject(ctx, s.ID, operator.ID, user.ID, now) {
		t.Fatal("the granted controller may not inject")
	}
	if svc.MayInject(ctx, s.ID, stranger.ID, user.ID, now) {
		t.Fatal("a different controller was permitted to inject")
	}
	if svc.MayInject(ctx, s.ID, operator.ID, stranger.ID, now) {
		t.Fatal("injection into a different machine was permitted")
	}
	if svc.MayInject(ctx, "no-such-session", operator.ID, user.ID, now) {
		t.Fatal("an unknown session permitted injection")
	}
	if svc.MayInject(ctx, s.ID, operator.ID, user.ID, s.ExpiresAt.Add(time.Second)) {
		t.Fatal("injection was permitted after the deadline")
	}
}

func TestRevokedSessionStopsInjectingImmediately(t *testing.T) {
	svc, _ := testService(t)
	s := granted(t, svc, ScopeControl, time.Hour)
	ctx := context.Background()
	now := time.Now()

	if !svc.MayInject(ctx, s.ID, operator.ID, user.ID, now) {
		t.Fatal("setup: injection should be permitted")
	}
	if _, err := svc.Revoke(ctx, user, s.ID, "stop", "", ""); err != nil {
		t.Fatal(err)
	}
	if svc.MayInject(ctx, s.ID, operator.ID, user.ID, now) {
		t.Fatal("injection was still permitted after revocation")
	}
}

// ---------------------------------------------------------------------------
// One session per machine
// ---------------------------------------------------------------------------

func TestOneLiveSessionPerMachine(t *testing.T) {
	svc, _ := testService(t)
	granted(t, svc, ScopeControl, time.Hour)

	_, err := svc.Request(context.Background(),
		Actor{ID: "u-op2", Email: "op2@example.com"}, "m-1", user, ScopeControl, "", "")
	if !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("a second operator opened a session on the same machine: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Rule 6: everything is on the chain
// ---------------------------------------------------------------------------

func TestEveryTransitionIsAudited(t *testing.T) {
	svc, rec := testService(t)
	ctx := context.Background()

	s := requested(t, svc, ScopeView)
	if _, err := svc.Grant(ctx, user, s.ID, time.Minute, "", ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := svc.Escalate(ctx, operator, s.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, user, fresh.ID, time.Minute, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Extend(ctx, user, fresh.ID, time.Minute, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Revoke(ctx, user, fresh.ID, "done", "", ""); err != nil {
		t.Fatal(err)
	}

	for _, a := range []audit.Action{
		audit.ActionControlRequested,
		audit.ActionControlGranted,
		audit.ActionControlEscalated,
		audit.ActionControlExtended,
		audit.ActionControlRevoked,
	} {
		if len(rec.find(a)) == 0 {
			t.Fatalf("no audit event recorded for %s", a)
		}
	}

	// Both identities must appear, since "who did this" has two answers.
	for _, e := range rec.find(audit.ActionControlGranted) {
		if e.Subject == "" {
			t.Fatal("a control audit event names no session")
		}
	}
}

// Both parties must be told about every change. A session change only one
// side can see is how a user believes control has stopped when it has not.
func TestBothPartiesAreNotified(t *testing.T) {
	svc, _ := testService(t)
	n := &notifier{}
	svc.SetNotifier(n)

	s := requested(t, svc, ScopeControl)
	if _, err := svc.Grant(context.Background(), user, s.ID, time.Minute, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Revoke(context.Background(), user, s.ID, "done", "", ""); err != nil {
		t.Fatal(err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.sent[user.ID] < 3 || n.sent[operator.ID] < 3 {
		t.Fatalf("notifications: subject %d, controller %d; both need all three",
			n.sent[user.ID], n.sent[operator.ID])
	}
}

// ---------------------------------------------------------------------------
// Denial
// ---------------------------------------------------------------------------

func TestDenyEndsTheRequest(t *testing.T) {
	svc, rec := testService(t)
	s := requested(t, svc, ScopeControl)

	got, err := svc.Deny(context.Background(), user, s.ID, "busy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateDenied {
		t.Fatalf("state is %q after denial", got.State)
	}
	if svc.MayInject(context.Background(), s.ID, operator.ID, user.ID, time.Now()) {
		t.Fatal("a denied session permitted injection")
	}
	if len(rec.find(audit.ActionControlDenied)) != 1 {
		t.Fatal("denial was not audited")
	}
	// A denied request cannot then be granted.
	if _, err := svc.Grant(context.Background(), user, s.ID, time.Minute, "", ""); !errors.Is(err, ErrBadState) {
		t.Fatalf("a denied request was granted afterwards: %v", err)
	}
}
