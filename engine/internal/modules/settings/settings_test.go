package settings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/database"
)

type recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recorder) Record(_ context.Context, e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) count(a audit.Action, o audit.Outcome) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Action == a && e.Outcome == o {
			n++
		}
	}
	return n
}

var (
	admin  = Actor{ID: "u-adm", Email: "adm@example.com", Admin: true}
	member = Actor{ID: "u-mem", Email: "mem@example.com"}
)

func testService(t *testing.T) (*Service, *recorder) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rec := &recorder{}
	svc := NewService(NewSQLRepository(db), rec,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, rec
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// A deployment nobody has configured must already be the safe one.
//
// The alternative -- convenient defaults, tightened later by whoever reads the
// documentation -- means every deployment is insecure for exactly as long as
// it takes somebody to notice, and the ones nobody notices stay that way.
func TestUnconfiguredDeploymentIsSafe(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	if got := svc.Value(ctx, KeyAdmissionDefault); got == "open" {
		t.Fatal("a fresh deployment defaults meetings to open; a leaked link would admit anybody")
	}
	if svc.Bool(ctx, KeyAllowSignup) {
		t.Fatal("a fresh deployment lets strangers create accounts")
	}
	if svc.Bool(ctx, KeyRecording) {
		t.Fatal("a fresh deployment permits recording without anybody enabling it")
	}
}

// Every catalogue entry must resolve to its documented default, and none may
// resolve to empty: a caller on the join path asking "may this person share"
// needs an answer, not a blank.
func TestEverySettingResolves(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	for _, spec := range Catalogue {
		got := svc.Value(ctx, spec.Key)
		// A text setting may legitimately default to empty, and the one that
		// does -- the organisation's domains -- means "no organisation", which
		// is the closed answer. Every other type must resolve to something.
		if got == "" && spec.Type != "text" {
			t.Errorf("%s resolved to empty", spec.Key)
		}
		if got != spec.Default {
			t.Errorf("%s resolved to %q, want its default %q", spec.Key, got, spec.Default)
		}
		if spec.Type == "bool" && got != "true" && got != "false" {
			t.Errorf("%s is a bool but resolved to %q", spec.Key, got)
		}
		if spec.Type == "choice" {
			ok := false
			for _, c := range spec.Choices {
				if c == got {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s resolved to %q, which is not one of its choices", spec.Key, got)
			}
		}
	}
}

func TestUnknownKeyResolvesToEmptyAndCannotBeSet(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	if got := svc.Value(ctx, "no.such.setting"); got != "" {
		t.Fatalf("an unknown key resolved to %q", got)
	}
	if err := svc.Set(ctx, admin, "no.such.setting", "true", "", ""); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("setting an unknown key returned %v", err)
	}
}

// ---------------------------------------------------------------------------
// Who may change policy
// ---------------------------------------------------------------------------

func TestOnlyAdministratorsChangePolicy(t *testing.T) {
	svc, rec := testService(t)
	ctx := context.Background()

	if err := svc.Set(ctx, member, KeyRemoteControl, "false", "1.2.3.4", "ua"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member changed deployment policy: %v", err)
	}
	if !svc.Bool(ctx, KeyRemoteControl) {
		t.Fatal("a refused change took effect anyway")
	}
	// The attempt is on the chain: somebody trying to alter deployment policy
	// is exactly what a review looks for.
	if rec.count(audit.ActionSettingChanged, audit.OutcomeDenied) != 1 {
		t.Fatal("a refused policy change was not audited")
	}

	if _, err := svc.All(ctx, member); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member read deployment policy: %v", err)
	}
}

func TestAdministratorChangeIsAppliedAndAudited(t *testing.T) {
	svc, rec := testService(t)
	ctx := context.Background()

	if err := svc.Set(ctx, admin, KeyRemoteControl, "false", "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	if svc.Bool(ctx, KeyRemoteControl) {
		t.Fatal("the change did not take effect")
	}
	if rec.count(audit.ActionSettingChanged, audit.OutcomeSuccess) != 1 {
		t.Fatal("an applied policy change was not audited")
	}
}

// A change must apply to the very next request, not after a cache interval.
func TestChangesApplyImmediately(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	for _, v := range []string{"false", "true", "false"} {
		if err := svc.Set(ctx, admin, KeyAttendeeShare, v, "", ""); err != nil {
			t.Fatal(err)
		}
		if got := svc.Value(ctx, KeyAttendeeShare); got != v {
			t.Fatalf("set %q, immediately read %q", v, got)
		}
	}
}

// Values are validated against the catalogue, not stored blindly.
func TestValuesAreValidated(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	for _, bad := range []string{"", "yes", "1", "TRUE", "on", "maybe"} {
		if err := svc.Set(ctx, admin, KeyRemoteControl, bad, "", ""); !errors.Is(err, ErrBadValue) {
			t.Errorf("bool setting accepted %q: %v", bad, err)
		}
	}
	for _, bad := range []string{"", "public", "OPEN", "anyone"} {
		if err := svc.Set(ctx, admin, KeyAdmissionDefault, bad, "", ""); !errors.Is(err, ErrBadValue) {
			t.Errorf("choice setting accepted %q: %v", bad, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The ceiling
// ---------------------------------------------------------------------------

// A host may not choose a meeting more open than the deployment permits.
//
// This is the difference between an admin console that means something and one
// that lists preferences. The clamp runs where the meeting is written, so a
// client that skips the interface entirely still gets held to it.
func TestAdmissionCeilingHoldsHostsDown(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	// Default floor is "open", so nothing is clamped.
	for _, chosen := range []string{"open", "trusted", "knock"} {
		if got := svc.ClampAdmission(ctx, chosen); got != chosen {
			t.Fatalf("with no ceiling, %q became %q", chosen, got)
		}
	}

	// Tighten to "trusted": nobody may open a meeting to the whole link.
	if err := svc.Set(ctx, admin, KeyAdmissionFloor, "trusted", "", ""); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"open":    "trusted", // clamped
		"trusted": "trusted",
		"knock":   "knock", // stricter than the floor is always allowed
	}
	for chosen, want := range cases {
		if got := svc.ClampAdmission(ctx, chosen); got != want {
			t.Errorf("floor=trusted: %q became %q, want %q", chosen, got, want)
		}
	}

	// Tighten to "knock": everybody waits, whatever the host asks for.
	if err := svc.Set(ctx, admin, KeyAdmissionFloor, "knock", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, chosen := range []string{"open", "trusted", "knock"} {
		if got := svc.ClampAdmission(ctx, chosen); got != "knock" {
			t.Errorf("floor=knock: %q became %q, want knock", chosen, got)
		}
	}
}

// Unrecognised input to the clamp becomes the strictest, like everywhere else.
func TestClampFailsClosedOnRubbish(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	for _, bad := range []string{"", "public", "OPEN", "everyone", "1"} {
		if got := svc.ClampAdmission(ctx, bad); got != "knock" {
			t.Errorf("clamp turned %q into %q, not the strictest", bad, got)
		}
	}
}

// Policy is read from every join, so it must be safe to read while it changes.
func TestConcurrentReadsAndWrites(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = svc.Value(ctx, KeyAdmissionDefault)
				_ = svc.Bool(ctx, KeyRemoteControl)
				_ = svc.ClampAdmission(ctx, "open")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 40; j++ {
			v := "true"
			if j%2 == 0 {
				v = "false"
			}
			_ = svc.Set(ctx, admin, KeyRemoteControl, v, "", "")
		}
	}()
	wg.Wait()
}

// Policy must survive a restart, or an administrator's decision lasts until
// the next deploy.
func TestPolicySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.db")

	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(NewSQLRepository(db), &recorder{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Set(ctx, admin, KeyAdmissionFloor, "knock", "", ""); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	svc2 := NewService(NewSQLRepository(db2), &recorder{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := svc2.Value(ctx, KeyAdmissionFloor); got != "knock" {
		t.Fatalf("after restart the floor is %q, want knock", got)
	}
	if got := svc2.ClampAdmission(ctx, "open"); got != "knock" {
		t.Fatal("the ceiling stopped being enforced after a restart")
	}
}

// The organisation directory is off until somebody turns it on. A deployment
// that has never opened the console does not publish its account list, and a
// public provider is refused even when one is typed in.
func TestOrganisationDirectoryIsClosedByDefault(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	if got := svc.OrgDomains(ctx); len(got) != 0 {
		t.Fatalf("a fresh deployment declared the organisation %v", got)
	}

	admin := Actor{ID: "u-admin", Email: "admin@ddecor.com", Admin: true}
	if err := svc.Set(ctx, admin, KeyOrgDomains, "ddecor.com, @Example.COM ", "", ""); err != nil {
		t.Fatal(err)
	}
	got := svc.OrgDomains(ctx)
	if len(got) != 2 || got[0] != "ddecor.com" || got[1] != "example.com" {
		t.Fatalf("parsed %v; want the two domains, lowercased and unprefixed", got)
	}

	// Naming a public provider is refused rather than honoured: it would make
	// every account on it browsable by every other.
	if err := svc.Set(ctx, admin, KeyOrgDomains, "gmail.com,ddecor.com,yahoo.com", "", ""); err != nil {
		t.Fatal(err)
	}
	got = svc.OrgDomains(ctx)
	if len(got) != 1 || got[0] != "ddecor.com" {
		t.Fatalf("public providers survived: %v", got)
	}

	// Nonsense is dropped, not stored as a domain.
	if err := svc.Set(ctx, admin, KeyOrgDomains, "localhost, , @, ddecor.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := svc.OrgDomains(ctx); len(got) != 1 || got[0] != "ddecor.com" {
		t.Fatalf("junk survived: %v", got)
	}
}

// The seed is a default, not a decision: it fills an empty setting once and
// never overwrites what an administrator chose.
func TestSeedingTheOrganisationNeverOverwritesAChoice(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()

	seeded, err := svc.SeedOrgDomains(ctx, "ddecor.com")
	if err != nil {
		t.Fatal(err)
	}
	if !seeded {
		t.Fatal("the first seed did not take")
	}

	// A restart must not re-seed.
	if again, _ := svc.SeedOrgDomains(ctx, "somewhere-else.com"); again {
		t.Fatal("a second seed overwrote the first")
	}
	if got := svc.OrgDomains(ctx); len(got) != 1 || got[0] != "ddecor.com" {
		t.Fatalf("after a second seed the value is %v", got)
	}

	// An administrator who clears it has closed the address book on purpose,
	// and a restart must not reopen it.
	admin := Actor{ID: "u-admin", Email: "a@ddecor.com", Admin: true}
	if err := svc.Set(ctx, admin, KeyOrgDomains, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.SeedOrgDomains(ctx, "ddecor.com"); again {
		t.Fatal("a deliberately cleared setting was re-seeded")
	}
	if got := svc.OrgDomains(ctx); len(got) != 0 {
		t.Fatalf("the address book reopened: %v", got)
	}
}

// A public provider is never seeded, however the administrator signed up.
func TestAPersonalAddressDoesNotSeedAnOrganisation(t *testing.T) {
	svc, _ := testService(t)
	for _, d := range []string{"gmail.com", "outlook.com", "yahoo.co.in", ""} {
		if seeded, _ := svc.SeedOrgDomains(context.Background(), d); seeded {
			t.Errorf("%q was seeded as an organisation", d)
		}
	}
}
