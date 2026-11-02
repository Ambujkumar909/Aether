package scheduling

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// Attendance, on a real SQLite file, because the thing under test is the SQL:
// the NULL-while-present column, the "latest scheduled title for this room"
// subquery, the ordering. A fake repository would test none of it.

func attendanceService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "att.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(NewSQLRepository(db), nullRecorder{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A visit opens on join and closes on leave, and the dashboard sees both
// states correctly: "in progress" while open, a duration once closed.
func TestVisitOpensAndCloses(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()

	id := s.Joined(ctx, "u-alice", "meet-abc", "Alice")
	if id == "" {
		t.Fatal("Joined returned no visit id for a signed-in user")
	}

	rows, err := s.History(ctx, Actor{ID: "u-alice"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !rows[0].LeftAt.IsZero() || rows[0].Seconds != 0 {
		t.Fatalf("an open visit reports left=%v seconds=%d; want open", rows[0].LeftAt, rows[0].Seconds)
	}
	// An ad-hoc room has no scheduled title: the room id stands in.
	if rows[0].Title != "meet-abc" {
		t.Fatalf("ad-hoc title = %q, want the room id", rows[0].Title)
	}

	s.Left(ctx, id)
	rows, _ = s.History(ctx, Actor{ID: "u-alice"}, 10)
	if rows[0].LeftAt.IsZero() {
		t.Fatal("Left did not close the visit")
	}
	if rows[0].Seconds < 0 {
		t.Fatalf("negative duration %d", rows[0].Seconds)
	}
}

// Guests leave no row. There is no account to show the history to, and a row
// keyed on an empty user id would be shown to nobody -- or to everybody.
func TestGuestsAreNotRecorded(t *testing.T) {
	s := attendanceService(t)
	if id := s.Joined(context.Background(), "", "meet-x", "Guest"); id != "" {
		t.Fatalf("a guest got a visit id %q", id)
	}
}

// Closing twice, or closing an unknown id, is harmless: a socket can unwind
// twice on the way out, and the second time has nothing to say.
func TestLeaveIsIdempotent(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	id := s.Joined(ctx, "u-bob", "meet-y", "Bob")
	s.Left(ctx, id)
	first, _ := s.History(ctx, Actor{ID: "u-bob"}, 10)
	time.Sleep(20 * time.Millisecond)
	s.Left(ctx, id)        // again
	s.Left(ctx, "no-such") // unknown
	s.Left(ctx, "")        // empty
	second, _ := s.History(ctx, Actor{ID: "u-bob"}, 10)
	if !first[0].LeftAt.Equal(second[0].LeftAt) {
		t.Fatalf("a second Left moved left_at from %v to %v", first[0].LeftAt, second[0].LeftAt)
	}
}

// History is the caller's own. Alice does not see Bob's visits.
func TestHistoryIsPerAccount(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	s.Left(ctx, s.Joined(ctx, "u-alice", "meet-a", "Alice"))
	s.Left(ctx, s.Joined(ctx, "u-bob", "meet-b", "Bob"))
	rows, _ := s.History(ctx, Actor{ID: "u-alice"}, 10)
	if len(rows) != 1 || rows[0].Room != "meet-a" {
		t.Fatalf("alice sees %v", rows)
	}
}

// Most recent first, and a scheduled meeting's title is picked up for its
// room -- so "recent" reads "Weekly review", not "weekly-review-k3Zq".
func TestHistoryOrderAndScheduledTitle(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()

	// A scheduled meeting whose room we then visit.
	start := time.Now().Add(time.Hour)
	m, err := s.Create(ctx, Actor{ID: "u-alice", Email: "a@x"}, &Meeting{
		Title: "Weekly review", Category: CatInternal,
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}

	s.Left(ctx, s.Joined(ctx, "u-alice", "older-room", "Alice"))
	time.Sleep(15 * time.Millisecond)
	s.Left(ctx, s.Joined(ctx, "u-alice", m.Room, "Alice"))

	rows, _ := s.History(ctx, Actor{ID: "u-alice"}, 10)
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Room != m.Room {
		t.Fatalf("most recent first: got %q, want %q", rows[0].Room, m.Room)
	}
	if rows[0].Title != "Weekly review" {
		t.Fatalf("scheduled title not resolved: %q", rows[0].Title)
	}
	if rows[1].Title != "older-room" {
		t.Fatalf("ad-hoc title should be the room id, got %q", rows[1].Title)
	}
}

// A call is a conversation event, not a meeting: rooms present in the call
// log must not appear in the dashboard's recent meetings.
func TestHistoryExcludesCallRooms(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	s.Left(ctx, s.Joined(ctx, "u-alice", "meet-normal", "Alice"))
	s.Left(ctx, s.Joined(ctx, "u-alice", "meet-call", "Alice"))
	// Mark meet-call as a call, the way messaging does when a ring is placed.
	if _, err := s.repo.(*SQLRepository).db.ExecContext(ctx, `INSERT INTO messaging_calls (id,room,caller_id,callee_id,kind,started_at,answered) VALUES ('c1','meet-call','u-alice','u-bob','video',?,1)`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, Actor{ID: "u-alice"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Room != "meet-normal" {
		t.Fatalf("history should hold only the meeting, got %v", rows)
	}
}
