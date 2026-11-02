package messaging

import (
	"context"
	"errors"
	"testing"
	"time"
)

// What one person decides about a conversation stays theirs: the flags round
// trip through the list, hiding takes it out until somebody writes, marking
// unread agrees with the truth, forgetting empties their view and nobody
// else's, and strangers may decide nothing.

func b(v bool) *bool { return &v }

func TestPrefsAreMineAndRoundTrip(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()

	if err := s.SetPrefs(ctx, me, conv, Prefs{Favourite: b(true), Muted: b(true)}); err != nil {
		t.Fatal(err)
	}
	mine, _ := s.List(ctx, me)
	if len(mine) != 1 || !mine[0].Favourite || !mine[0].Muted {
		t.Fatalf("my flags: %+v", mine[0])
	}
	theirs, _ := s.List(ctx, you)
	if theirs[0].Favourite || theirs[0].Muted {
		t.Fatalf("my flags leaked to the other member: %+v", theirs[0])
	}
	if err := s.SetPrefs(ctx, me, conv, Prefs{Favourite: b(false)}); err != nil {
		t.Fatal(err)
	}
	mine, _ = s.List(ctx, me)
	if mine[0].Favourite || !mine[0].Muted {
		t.Fatalf("clearing one flag touched the other: %+v", mine[0])
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if err := s.SetPrefs(ctx, stranger, conv, Prefs{Favourite: b(true)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger setting prefs: err=%v", err)
	}
}

func TestEveryCombinationOfFlagsLandsOnItsOwnColumn(t *testing.T) {
	s, me, _, conv := twoPeople(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	for mask := 0; mask < 8; mask++ {
		fav, mute, hid := mask&1 != 0, mask&2 != 0, mask&4 != 0
		if err := s.SetPrefs(ctx, me, conv, Prefs{Favourite: b(fav), Muted: b(mute), Hidden: b(hid)}); err != nil {
			t.Fatal(err)
		}
		var gf, gm, gh int
		if err := repo.db.QueryRowContext(ctx,
			`SELECT favourite, muted, hidden FROM messaging_members WHERE conversation_id=? AND user_id=?`,
			conv, me.ID).Scan(&gf, &gm, &gh); err != nil {
			t.Fatal(err)
		}
		if (gf != 0) != fav || (gm != 0) != mute || (gh != 0) != hid {
			t.Fatalf("mask %03b: stored favourite=%d muted=%d hidden=%d", mask, gf, gm, gh)
		}
	}
}

func TestHiddenComesBackWhenSomebodyWrites(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	s.Send(ctx, you, conv, "old")
	if err := s.SetPrefs(ctx, me, conv, Prefs{Hidden: b(true)}); err != nil {
		t.Fatal(err)
	}
	if mine, _ := s.List(ctx, me); len(mine) != 0 {
		t.Fatalf("hidden chat still listed: %+v", mine)
	}
	if theirs, _ := s.List(ctx, you); len(theirs) != 1 {
		t.Fatal("hiding on my side removed it from theirs")
	}
	// My own message does not bring it back; theirs does.
	s.Send(ctx, me, conv, "still hidden for me")
	if mine, _ := s.List(ctx, me); len(mine) != 0 {
		t.Fatal("my own message un-hid the chat")
	}
	time.Sleep(2 * time.Millisecond)
	s.Send(ctx, you, conv, "hello?")
	mine, _ := s.List(ctx, me)
	if len(mine) != 1 || mine[0].Unread != 1 {
		t.Fatalf("after they wrote: %+v", mine)
	}
}

func TestMarkUnreadAgreesWithTheTruth(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	time.Sleep(2 * time.Millisecond)
	s.Send(ctx, you, conv, "a")
	time.Sleep(2 * time.Millisecond)
	s.Send(ctx, you, conv, "b")
	time.Sleep(2 * time.Millisecond)
	s.MarkRead(ctx, me, conv)
	if mine, _ := s.List(ctx, me); mine[0].Unread != 0 {
		t.Fatalf("after reading: %d", mine[0].Unread)
	}
	if err := s.MarkUnread(ctx, me, conv); err != nil {
		t.Fatal(err)
	}
	mine, _ := s.List(ctx, me)
	truth, _ := repo.UnreadCount(ctx, conv, me.ID)
	if mine[0].Unread != 1 || truth != 1 {
		t.Fatalf("mark unread: counter %d, truth %d, want 1 and 1", mine[0].Unread, truth)
	}
	// Reading again clears it.
	time.Sleep(2 * time.Millisecond)
	s.MarkRead(ctx, me, conv)
	if mine, _ := s.List(ctx, me); mine[0].Unread != 0 {
		t.Fatalf("after reading again: %d", mine[0].Unread)
	}
	// An empty conversation cannot be marked unread; nothing happens.
	empty, _ := s.OpenSelf(ctx, me)
	if err := s.MarkUnread(ctx, me, empty.ID); err != nil {
		t.Fatal(err)
	}
}

func TestForgetEmptiesMyViewAndNobodyElses(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	s.Send(ctx, you, conv, "one")
	s.Send(ctx, me, conv, "two")
	if err := s.ForgetConversation(ctx, me, conv); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, s, me, conv); len(got) != 0 {
		t.Fatalf("my view after forgetting: %v", got)
	}
	if got := bodies(t, s, you, conv); len(got) != 2 {
		t.Fatalf("their view after I forgot: %v", got)
	}
	if mine, _ := s.List(ctx, me); len(mine) != 0 {
		t.Fatal("forgotten chat still listed")
	}
	// They write again: the chat returns, with only the new message.
	time.Sleep(2 * time.Millisecond)
	s.Send(ctx, you, conv, "three")
	mine, _ := s.List(ctx, me)
	if len(mine) != 1 || mine[0].Unread != 1 {
		t.Fatalf("after they wrote: %+v", mine)
	}
	if got := bodies(t, s, me, conv); len(got) != 1 || got[0] != "three" {
		t.Fatalf("my view after they wrote: %v", got)
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if err := s.ForgetConversation(ctx, stranger, conv); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger forgetting: err=%v", err)
	}
}
