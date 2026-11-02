package messaging

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// The unread counter must equal the count it replaced, after every operation
// that can move either. A random schedule of sends, reads, deletes for
// everyone and deletes for me across three people and two conversations,
// checked op by op against UnreadCount (the counting query), which is the
// truth the counter is held to. Seeded, so a failure replays.

func TestUnreadCounterEqualsTheTruthUnderARandomSchedule(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)

	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	direct, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	convs := []string{direct.ID, group.ID}
	people := []Actor{me, you}

	check := func(step int, what string) {
		t.Helper()
		for _, c := range convs {
			for _, p := range people {
				truth, err := repo.UnreadCount(ctx, c, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := repo.UnreadCounts(ctx, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got[c] != truth {
					t.Fatalf("step %d (%s): %s in %s: counter %d, truth %d", step, what, p.Name, c, got[c], truth)
				}
				mTruth, err := repo.MentionCount(ctx, c, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				var mGot int
				if err := repo.db.QueryRowContext(ctx,
					`SELECT mentions FROM messaging_members WHERE conversation_id=? AND user_id=?`, c, p.ID).Scan(&mGot); err != nil {
					t.Fatal(err)
				}
				if mGot != mTruth {
					t.Fatalf("step %d (%s): %s in %s: mentions counter %d, truth %d", step, what, p.Name, c, mGot, mTruth)
				}
			}
		}
	}

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	var sent []*Message
	for step := 0; step < 400; step++ {
		c := convs[rng.Intn(len(convs))]
		p := people[rng.Intn(len(people))]
		var what string
		switch r := rng.Intn(10); {
		case r < 5:
			// Every other send names the other person.
			var mentions []string
			if step%2 == 0 {
				for _, q := range people {
					if q.ID != p.ID {
						mentions = append(mentions, q.ID)
					}
				}
			}
			m, err := s.SendMessage(ctx, p, c, fmt.Sprintf("m%d", step), "", nil, mentions)
			if err != nil {
				t.Fatal(err)
			}
			sent = append(sent, m)
			what = "send"
		case r < 7:
			if err := s.MarkRead(ctx, p, c); err != nil {
				t.Fatal(err)
			}
			what = "read"
		case r < 8 && len(sent) > 0:
			m := sent[rng.Intn(len(sent))]
			if !m.Deleted {
				if err := s.Delete(ctx, Actor{ID: m.SenderID, Email: "x", Name: "x"}, m.ID); err != nil {
					t.Fatal(err)
				}
				m.Deleted = true
			}
			what = "delete for everyone"
		case len(sent) > 0:
			m := sent[rng.Intn(len(sent))]
			if err := s.Hide(ctx, p, m.ID); err != nil {
				t.Fatal(err)
			}
			what = "delete for me"
		default:
			continue
		}
		check(step, what)
	}
	// Membership events never count.
	if _, _, err := s.AddMembers(ctx, me, group.ID, []string{"other@example.com"}); err != nil {
		t.Fatal(err)
	}
	check(401, "membership event")
}

// The list reads the counter, and the counter is what the badge shows.
func TestListUsesTheCounter(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, _ := s.OpenDirect(ctx, me, you.Email)
	time.Sleep(2 * time.Millisecond) // past the join cursor
	for i := 0; i < 3; i++ {
		s.Send(ctx, you, c.ID, "x")
	}
	list, _ := s.List(ctx, me)
	if len(list) != 1 || list[0].Unread != 3 {
		t.Fatalf("unread after three messages: %+v", list)
	}
	time.Sleep(2 * time.Millisecond)
	s.MarkRead(ctx, me, c.ID)
	list, _ = s.List(ctx, me)
	if list[0].Unread != 0 {
		t.Fatalf("unread after reading: %d", list[0].Unread)
	}
	time.Sleep(2 * time.Millisecond)
	s.Send(ctx, you, c.ID, "y")
	m, _ := s.Send(ctx, you, c.ID, "z")
	s.Delete(ctx, you, m.ID)
	list, _ = s.List(ctx, me)
	if list[0].Unread != 1 {
		t.Fatalf("unread after a send and a retracted send: %d, want 1", list[0].Unread)
	}
}
