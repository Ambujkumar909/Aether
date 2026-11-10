package scheduling

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The recap: who may read it, and what it says.
func TestTheRecapIsForAttendeesAndInviteesAndSumsTheirVisits(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	room := "weekly-sync"
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	alice := Actor{ID: "u-alice", Email: "alice@example.com"}
	bob := Actor{ID: "u-bob", Email: "bob@example.com"}
	carol := Actor{ID: "u-carol", Email: "carol@example.com"} // invited, never came
	dave := Actor{ID: "u-dave", Email: "dave@example.com"}    // nothing to do with it

	// The scheduled meeting behind the room, with Carol invited.
	if _, err := s.Create(ctx, alice, &Meeting{Room: room, Title: "Weekly sync", Category: CatInternal,
		StartsAt: t0, EndsAt: t0.Add(time.Hour), Invitees: []Invitee{{Email: carol.Email}}}, "", ""); err != nil {
		t.Fatal(err)
	}
	// Visits: Alice 09:00-09:40, Bob 09:05-09:20 and again 09:25-09:40.
	a1, _ := s.repo.RecordJoin(ctx, alice.ID, room, "Alice", t0)
	b1, _ := s.repo.RecordJoin(ctx, bob.ID, room, "Bob", t0.Add(5*time.Minute))
	_ = s.repo.RecordLeave(ctx, b1, t0.Add(20*time.Minute))
	b2, _ := s.repo.RecordJoin(ctx, bob.ID, room, "Bob", t0.Add(25*time.Minute))
	_ = s.repo.RecordLeave(ctx, b2, t0.Add(40*time.Minute))
	_ = s.repo.RecordLeave(ctx, a1, t0.Add(40*time.Minute))
	// Notes and a transcript the meeting left.
	_ = s.repo.CreateNotes(ctx, room, alice.ID, t0)
	_ = s.repo.AddNoteItem(ctx, room, &NoteItem{ID: "n1", Section: "tasks", Body: "Ship it", Assignee: "Bob", UpdatedAt: t0})
	_, _ = s.repo.AppendTranscriptLine(ctx, room, 1, alice.ID, "Alice", "let us begin", t0.Add(time.Minute))

	now := t0.Add(2 * time.Hour)
	for _, who := range []Actor{alice, bob, carol} {
		r, err := s.Recap(ctx, who, room, now)
		if err != nil {
			t.Fatalf("%s: %v", who.ID, err)
		}
		if r.Title != "Weekly sync" || r.Live || len(r.People) != 2 || r.Seconds != 40*60 {
			t.Fatalf("%s: title %q live %v people %d seconds %d", who.ID, r.Title, r.Live, len(r.People), r.Seconds)
		}
		if r.People[0].Name != "Alice" || r.People[0].Seconds != 40*60 || r.People[1].Name != "Bob" || r.People[1].Seconds != 30*60 {
			t.Fatalf("%s: people %+v", who.ID, r.People)
		}
		if !r.HasNotes || len(r.Notes) != 1 || r.Notes[0].Body != "Ship it" || len(r.Lines) != 1 || r.Lines[0].Text != "let us begin" || r.Session != 1 {
			t.Fatalf("%s: notes %v lines %v", who.ID, r.Notes, r.Lines)
		}
		if !r.StartedAt.Equal(t0) || !r.EndedAt.Equal(t0.Add(40*time.Minute)) {
			t.Fatalf("%s: %s - %s", who.ID, r.StartedAt, r.EndedAt)
		}
	}
	if _, err := s.Recap(ctx, dave, room, now); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read the recap: %v", err)
	}
	if _, err := s.Recap(ctx, Actor{}, room, now); !errors.Is(err, ErrForbidden) {
		t.Fatal("nobody read the recap")
	}
	if _, err := s.Recap(ctx, dave, "", now); !errors.Is(err, ErrForbidden) {
		t.Fatal("an empty room was accepted")
	}
	// An administrator may read any recap.
	if _, err := s.Recap(ctx, Actor{ID: "u-adm", Email: "adm@example.com", Admin: true}, room, now); err != nil {
		t.Fatalf("admin: %v", err)
	}
	// Somebody still in the room: live, no end, time counted to now.
	_, _ = s.repo.RecordJoin(ctx, bob.ID, room, "Bob", now.Add(-10*time.Minute))
	r, _ := s.Recap(ctx, bob, room, now)
	if !r.Live || !r.EndedAt.IsZero() || !r.People[1].Present || r.People[1].Seconds != 40*60 {
		t.Fatalf("live recap: live %v ended %s bob %+v", r.Live, r.EndedAt, r.People[1])
	}
	// A room nobody has ever used reads as an empty recap for an invitee,
	// and as forbidden for everyone else.
	if _, err := s.Create(ctx, alice, &Meeting{Room: "quiet-room", Title: "Quiet", Category: CatInternal,
		StartsAt: t0, EndsAt: t0.Add(time.Hour), Invitees: []Invitee{{Email: carol.Email}}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Recap(ctx, carol, "quiet-room", now); err != nil || len(r.People) != 0 || r.Title != "Quiet" || r.Seconds != 0 {
		t.Fatalf("empty recap: %+v %v", r, err)
	}
	if _, err := s.Recap(ctx, bob, "quiet-room", now); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an uninvited non-attendee read an empty recap: %v", err)
	}
}
