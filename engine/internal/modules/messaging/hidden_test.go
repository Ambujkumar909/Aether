package messaging

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two deletes, two questions. "For me" edits one reader's view; "for
// everyone" edits the record. The tests below hold the line between them:
// hiding must be invisible to the other member, and tombstoning must be
// visible to both.

func twoPeople(t *testing.T) (*Service, Actor, Actor, string) {
	t.Helper()
	s := serviceForTest(t)
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, err := s.OpenDirect(context.Background(), me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	return s, me, you, c.ID
}

func bodies(t *testing.T, s *Service, a Actor, conv string) []string {
	t.Helper()
	ms, err := s.Messages(context.Background(), a, conv, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Deleted {
			out = append(out, "<deleted>")
			continue
		}
		out = append(out, m.Body)
	}
	return out
}

func TestDeleteForMeLeavesTheOtherPersonsThreadAlone(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()

	m1, _ := s.Send(ctx, you, conv, "one")
	s.Send(ctx, you, conv, "two")
	s.Send(ctx, me, conv, "three")

	// Anybody in the conversation may hide anybody's message: it is their
	// own view they are editing.
	if err := s.Hide(ctx, me, m1.ID); err != nil {
		t.Fatalf("hide: %v", err)
	}

	got := bodies(t, s, me, conv)
	want := []string{"two", "three"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("my view after delete-for-me: got %v, want %v", got, want)
	}

	theirs := bodies(t, s, you, conv)
	if len(theirs) != 3 || theirs[0] != "one" {
		t.Fatalf("delete-for-me leaked to the other member: %v", theirs)
	}

	// Idempotent: hiding twice is not an error and does not hide anything else.
	if err := s.Hide(ctx, me, m1.ID); err != nil {
		t.Fatalf("second hide: %v", err)
	}
	if got := bodies(t, s, me, conv); len(got) != 2 {
		t.Fatalf("second hide changed the view: %v", got)
	}
}

func TestDeleteForEveryoneIsATombstoneForBoth(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()

	m, _ := s.Send(ctx, me, conv, "take this back")
	s.Send(ctx, you, conv, "reply")

	// Only the sender may delete for everyone.
	if err := s.Delete(ctx, you, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("somebody else deleting my message for everyone: err=%v, want ErrForbidden", err)
	}
	if err := s.Delete(ctx, me, m.ID); err != nil {
		t.Fatal(err)
	}
	for _, who := range []Actor{me, you} {
		got := bodies(t, s, who, conv)
		if len(got) != 2 || got[0] != "<deleted>" || got[1] != "reply" {
			t.Fatalf("%s sees %v after delete-for-everyone, want [<deleted> reply]", who.Name, got)
		}
	}
}

func TestOutsidersCannotHideWhatTheyCannotRead(t *testing.T) {
	s, me, _, conv := twoPeople(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, conv, "private")

	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if err := s.Hide(ctx, stranger, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member hiding a message: err=%v, want ErrForbidden", err)
	}
	if err := s.Hide(ctx, me, "no-such-message"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hiding a message that does not exist: err=%v, want ErrNotFound", err)
	}
}
