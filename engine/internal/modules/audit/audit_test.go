package audit

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/aether-rtc/aether/internal/platform/database"
)

func testService(t *testing.T) (*Service, *database.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := NewService(NewSQLRepository(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	return svc, db
}

func seed(t *testing.T, svc *Service, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		svc.Record(ctx, Event{
			Action: ActionLoginSucceeded, Outcome: OutcomeSuccess,
			ActorID: "u1", ActorEmail: "someone@example.com",
			Subject: "someone@example.com", IP: "10.0.0.1",
		})
	}
}

func TestChainVerifiesWhenIntact(t *testing.T) {
	svc, _ := testService(t)
	seed(t, svc, 25)

	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("intact chain reported broken at %d: %s", res.BrokenAt, res.Detail)
	}
	if res.Checked != 25 {
		t.Fatalf("verified %d events, expected 25", res.Checked)
	}
	t.Logf("verified %d events, head %s", res.Checked, res.HeadHash[:16])
}

// The whole point of the design: editing a row in place must be detectable.
// This is the attack the chain exists to catch -- an administrator with
// database access quietly rewriting what they did.
func TestEditingARowIsDetected(t *testing.T) {
	svc, db := testService(t)
	seed(t, svc, 10)

	// Someone with database access changes the recorded IP address on the
	// fifth event to point at somebody else.
	_, err := db.Exec(`UPDATE audit_events SET ip = ? WHERE seq = ?`, "203.0.113.9", 5)
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("an edited audit row passed verification; the log is not tamper-evident")
	}
	if res.BrokenAt != 5 {
		t.Fatalf("break reported at %d, expected 5", res.BrokenAt)
	}
	t.Logf("detected: %s", res.Detail)
}

// Deleting a row must break the chain too. Deletion is the more likely
// attack, because it leaves no obviously wrong content behind.
func TestDeletingARowIsDetected(t *testing.T) {
	svc, db := testService(t)
	seed(t, svc, 10)

	if _, err := db.Exec(`DELETE FROM audit_events WHERE seq = ?`, 6); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("a deleted audit row passed verification")
	}
	if res.BrokenAt != 7 {
		t.Fatalf("break reported at %d, expected 7 (the event after the hole)", res.BrokenAt)
	}
	t.Logf("detected: %s", res.Detail)
}

// Rewriting content AND its own hash still fails, because the next event
// commits to the old hash. Tampering has to be done to every subsequent row,
// which is what makes the head hash worth pinning externally.
func TestRecomputingOneHashStillFails(t *testing.T) {
	svc, db := testService(t)
	seed(t, svc, 10)

	var e Event
	row := db.QueryRow(`SELECT seq, id, at, actor_id, actor_email, action,
		subject, outcome, ip, user_agent, detail, prev_hash, hash
		FROM audit_events WHERE seq = 5`)
	e, err := scan(row)
	if err != nil {
		t.Fatal(err)
	}

	// The attacker changes the detail and correctly recomputes this row.
	e.Detail = "nothing to see here"
	e.Hash = ChainHash(&e)
	if _, err := db.Exec(`UPDATE audit_events SET detail=?, hash=? WHERE seq=?`,
		e.Detail, e.Hash, 5); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("a locally-consistent forgery passed verification")
	}
	// Row 5 now hashes correctly, so the break surfaces at row 6, whose
	// prev_hash still commits to the original row 5.
	if res.BrokenAt != 6 {
		t.Fatalf("break reported at %d, expected 6", res.BrokenAt)
	}
	t.Logf("detected at the following link: %s", res.Detail)
}

// The hash must be unambiguous across field boundaries. With naive
// concatenation, moving a delimiter between two attacker-controlled fields
// produces an identical digest, which is a forgery primitive.
func TestFieldBoundariesAreUnambiguous(t *testing.T) {
	a := Event{PrevHash: GenesisHash, ID: "x", ActorEmail: "ab", Subject: "c"}
	b := Event{PrevHash: GenesisHash, ID: "x", ActorEmail: "a", Subject: "bc"}
	if ChainHash(&a) == ChainHash(&b) {
		t.Fatal("two different events hash identically; field encoding is ambiguous")
	}
}

func TestEmptyLogVerifies(t *testing.T) {
	svc, _ := testService(t)
	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Checked != 0 || res.HeadHash != GenesisHash {
		t.Fatalf("empty log did not verify cleanly: %+v", res)
	}
}

func TestFilteringAndSearch(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	svc.Record(ctx, Event{Action: ActionLoginSucceeded, Outcome: OutcomeSuccess,
		ActorID: "a", ActorEmail: "alice@example.com"})
	svc.Record(ctx, Event{Action: ActionLoginFailed, Outcome: OutcomeFailure,
		ActorID: "b", ActorEmail: "bob@example.com"})
	svc.Record(ctx, Event{Action: ActionUserCreated, Outcome: OutcomeSuccess,
		ActorID: "a", ActorEmail: "alice@example.com", Subject: "carol@example.com"})

	got, err := svc.List(ctx, Filter{ActorID: "a"})
	if err != nil || len(got) != 2 {
		t.Fatalf("actor filter returned %d events, err=%v", len(got), err)
	}
	got, err = svc.List(ctx, Filter{Outcome: OutcomeFailure})
	if err != nil || len(got) != 1 {
		t.Fatalf("outcome filter returned %d events", len(got))
	}
	got, err = svc.List(ctx, Filter{Search: "carol"})
	if err != nil || len(got) != 1 {
		t.Fatalf("search returned %d events", len(got))
	}

	// A search containing LIKE metacharacters must be treated literally,
	// not as a wildcard that matches everything.
	got, err = svc.List(ctx, Filter{Search: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a literal %% matched %d rows; LIKE metacharacters are not escaped", len(got))
	}
}

func TestCountMatchesListFilter(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		svc.Record(ctx, Event{Action: ActionLoginFailed, Outcome: OutcomeFailure, ActorID: "z"})
	}
	for i := 0; i < 3; i++ {
		svc.Record(ctx, Event{Action: ActionLoginSucceeded, Outcome: OutcomeSuccess, ActorID: "z"})
	}
	n, err := svc.Count(ctx, Filter{Outcome: OutcomeFailure})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("count returned %d, expected 7", n)
	}
}

func TestChainSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.db")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	db1, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s1 := NewService(NewSQLRepository(db1), quiet)
	if err := s1.Start(ctx); err != nil {
		t.Fatal(err)
	}
	seed(t, s1, 5)
	s1.Stop()
	_ = db1.Close()

	// A restart must pick the chain back up rather than starting a new one.
	db2, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2 := NewService(NewSQLRepository(db2), quiet)
	if err := s2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	seed(t, s2, 5)

	res, err := s2.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("chain broke across a restart: %s", res.Detail)
	}
	if res.Checked != 10 {
		t.Fatalf("expected 10 events after restart, got %d", res.Checked)
	}
}
