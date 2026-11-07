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
	unreadSchedule(t, LargeConversation)
}

// The same schedule with both conversations past the threshold, so every
// count is taken at read time: the read-time path is held to the same truth.
func TestUnreadCountedAtReadTimeEqualsTheTruthUnderARandomSchedule(t *testing.T) {
	unreadSchedule(t, 1)
}

func unreadSchedule(t *testing.T, large int) {
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	repo.large = large

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
				truth, err := repo.UnreadTruth(ctx, c, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := repo.UnreadCounts(ctx, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got[c] != truth {
					t.Fatalf("step %d (%s): %s in %s: list %d, truth %d", step, what, p.Name, c, got[c], truth)
				}
				// The row and the open conversation say the same number.
				one, err := repo.UnreadCount(ctx, c, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if one != truth {
					t.Fatalf("step %d (%s): %s in %s: one conversation %d, truth %d", step, what, p.Name, c, one, truth)
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

// A conversation that was large keeps no counters; when it shrinks back to
// the threshold its counters are counted from the messages, so the list is
// exact from then on. Without the recount the counter read 0 here.
func TestShrinkingBelowTheThresholdRecountsTheCounters(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	repo.large = 2
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, err := s.CreateGroup(ctx, me, "Big", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AddMembers(ctx, c.ID, []Member{{UserID: "u-third", Email: "third@example.com", Name: "Third", Role: RoleMember}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // past the join cursors
	for i := range 5 {
		if _, err := s.Send(ctx, you, c.ID, fmt.Sprint("while large ", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := repo.UnreadCounts(ctx, me.ID); got[c.ID] != 5 {
		t.Fatalf("large: counted at read time %d, want 5", got[c.ID])
	}
	if err := repo.RemoveMember(ctx, c.ID, "u-third"); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.UnreadCounts(ctx, me.ID); got[c.ID] != 5 {
		t.Fatalf("back to the threshold: the kept counter reads %d, want 5", got[c.ID])
	}
	if _, err := s.Send(ctx, you, c.ID, "small again"); err != nil {
		t.Fatal(err)
	}
	truth, _ := repo.UnreadTruth(ctx, c.ID, me.ID)
	if got, _ := repo.UnreadCounts(ctx, me.ID); got[c.ID] != 6 || truth != 6 {
		t.Fatalf("kept again: counter %d, truth %d, want 6", got[c.ID], truth)
	}
}

// A large conversation's count stops at UnreadCap: the badge shows "99+" for
// anything over 99, so 100 is exactly what a bigger number would show, and
// the read never walks further than that.
func TestALargeConversationsCountStopsAtTheCap(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	repo.large = 1
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, err := s.CreateGroup(ctx, me, "Big", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	for i := range UnreadCap + 50 {
		if _, err := s.Send(ctx, you, c.ID, fmt.Sprint("m", i)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := repo.UnreadCounts(ctx, me.ID)
	one, _ := repo.UnreadCount(ctx, c.ID, me.ID)
	truth, _ := repo.UnreadTruth(ctx, c.ID, me.ID)
	if got[c.ID] != UnreadCap || one != UnreadCap || truth != UnreadCap+50 {
		t.Fatalf("list %d, one %d, truth %d; want %d, %d, %d", got[c.ID], one, truth, UnreadCap, UnreadCap, UnreadCap+50)
	}
}
