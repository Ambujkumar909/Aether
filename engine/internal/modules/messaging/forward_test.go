package messaging

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Forwarding is a new message, from you, marked as carried from elsewhere.
// What these tests hold: the flag is set and the body copied; membership is
// checked on every target before anything is written; a tombstone cannot be
// forwarded; the cap is enforced; and the forward never links back to a
// conversation the recipient is not in.

func threeChats(t *testing.T) (s *Service, me, you Actor, withYou, withSelf string) {
	t.Helper()
	s = serviceForTest(t)
	me = Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you = Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	ctx := context.Background()
	c1, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.OpenSelf(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	return s, me, you, c1.ID, c2.ID
}

func TestForwardCopiesTheBodyAndSetsTheFlag(t *testing.T) {
	s, me, you, withYou, withSelf := threeChats(t)
	ctx := context.Background()
	src, _ := s.Send(ctx, you, withYou, "the address is 42 Elm St")

	out, err := s.Forward(ctx, me, src.ID, []string{withSelf, withSelf}) // duplicate collapses
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("%d messages created, want 1 (duplicates collapse)", len(out))
	}
	m := out[0]
	if !m.Forwarded || m.Body != src.Body || m.SenderID != me.ID || m.ConversationID != withSelf {
		t.Fatalf("forwarded message %+v", m)
	}
	if m.ReplyTo != "" {
		t.Fatal("a forward must not carry the source's reply link")
	}

	got, _ := s.Messages(ctx, me, withSelf, time.Time{}, 10)
	if len(got) != 1 || !got[0].Forwarded {
		t.Fatalf("read back: %+v", got)
	}
}

func TestForwardIsAllOrNothingOnMembership(t *testing.T) {
	s, me, you, withYou, withSelf := threeChats(t)
	ctx := context.Background()
	src, _ := s.Send(ctx, you, withYou, "hi")

	// A conversation "me" is not in.
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	theirs, err := s.OpenSelf(ctx, stranger)
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.Forward(ctx, me, src.ID, []string{withSelf, theirs.ID})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("forwarding into a conversation you are not in: err=%v, want ErrForbidden", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d messages written before the refusal; membership must be checked first", len(out))
	}
	if got, _ := s.Messages(ctx, me, withSelf, time.Time{}, 10); len(got) != 0 {
		t.Fatalf("the valid target received a message despite the refusal: %+v", got)
	}
	if got, _ := s.Messages(ctx, stranger, theirs.ID, time.Time{}, 10); len(got) != 0 {
		t.Fatalf("the stranger's thread received a message: %+v", got)
	}
}

func TestForwardNeedsAReadableSource(t *testing.T) {
	s, me, you, withYou, withSelf := threeChats(t)
	ctx := context.Background()

	// Somebody outside the source conversation cannot forward from it.
	src, _ := s.Send(ctx, you, withYou, "secret")
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	own, _ := s.OpenSelf(ctx, stranger)
	if _, err := s.Forward(ctx, stranger, src.ID, []string{own.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member forwarding out of a conversation: err=%v, want ErrForbidden", err)
	}

	// A tombstone has nothing to forward, and forwarding must not read one.
	if err := s.Delete(ctx, you, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forward(ctx, me, src.ID, []string{withSelf}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forwarding a deleted message: err=%v, want ErrNotFound", err)
	}

	if _, err := s.Forward(ctx, me, "no-such", []string{withSelf}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forwarding a message that does not exist: err=%v", err)
	}
}

func TestForwardCapAndEmptyTargets(t *testing.T) {
	s, me, you, withYou, withSelf := threeChats(t)
	ctx := context.Background()
	src, _ := s.Send(ctx, you, withYou, "hi")

	if _, err := s.Forward(ctx, me, src.ID, nil); !errors.Is(err, ErrNoMembers) {
		t.Fatalf("no targets: err=%v, want ErrNoMembers", err)
	}
	if _, err := s.Forward(ctx, me, src.ID, []string{" ", ""}); !errors.Is(err, ErrNoMembers) {
		t.Fatalf("blank targets: err=%v, want ErrNoMembers", err)
	}
	many := make([]string, 0, MaxForwardTargets+1)
	for i := 0; i <= MaxForwardTargets; i++ {
		many = append(many, withSelf+string(rune('a'+i)))
	}
	if _, err := s.Forward(ctx, me, src.ID, many); !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("%d targets: err=%v, want ErrTooManyTargets", len(many), err)
	}
}
