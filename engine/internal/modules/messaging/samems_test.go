package messaging

import (
	"context"
	"testing"
	"time"
)

// A message created in the same millisecond a person joined counts for
// them. The counters compare created_at > last_read; a read cursor equal to
// the join instant therefore has to sit one millisecond before it, or the
// first message of a brand-new group is silently uncounted for everyone
// but its sender. (Seen once in the harness: "B's mention counter: got 0,
// want 1".)
func TestMessageInTheJoinMillisecondCounts(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	_ = third
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	var joined time.Time
	for _, m := range g.Members {
		if m.UserID == you.ID {
			joined = m.JoinedAt
		}
	}
	if joined.IsZero() {
		t.Fatal("you are not a member")
	}
	msg := &Message{ID: newID(), ConversationID: g.ID, SenderID: me.ID, SenderName: "Me", Body: "first", CreatedAt: joined}
	if err := s.repo.AddMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	// Recount from the truth (HideBefore with nothing to hide does that).
	if err := s.repo.HideBefore(ctx, g.ID, you.ID, time.UnixMilli(0)); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, you)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == g.ID {
			if c.Unread != 1 {
				t.Fatalf("a message in the join millisecond: unread %d, want 1", c.Unread)
			}
			return
		}
	}
	t.Fatal("group not listed")
}
