package messaging

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func now() time.Time { return time.Now().UTC() }

// A block empties the direct line in both directions and nothing else; it
// lifts cleanly; it is bounded; a stranger's id cannot be blocked into
// existence.

func TestABlockedDirectConversationCarriesNothingEitherWay(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	s.Send(ctx, you, conv, "before")

	if err := s.Block(ctx, me, you.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, you, conv, "after"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("the blocked person could send: %v", err)
	}
	if _, err := s.Send(ctx, me, conv, "me neither"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("the blocker could send: %v", err)
	}
	if got := bodies(t, s, me, conv); len(got) != 1 || got[0] != "before" {
		t.Fatalf("history changed: %v", got)
	}
	// A ring is dropped, and not even logged.
	s.Ring(ctx, you, me.ID, "room-1")
	if calls, _ := s.CallsFor(ctx, me, conv); len(calls) != 0 {
		t.Fatalf("a blocked ring was logged: %+v", calls)
	}
	// The list tells the blocker, and only the blocker.
	mine, _ := s.List(ctx, me)
	theirs, _ := s.List(ctx, you)
	if !mine[0].Blocked {
		t.Fatal("my list does not show the block")
	}
	if theirs[0].Blocked {
		t.Fatal("their list shows my block")
	}
	list, _ := s.Blocks(ctx, me)
	if len(list) != 1 || list[0].UserID != you.ID || list[0].Email != you.Email {
		t.Fatalf("block list: %+v", list)
	}
	if list, _ := s.Blocks(ctx, you); len(list) != 0 {
		t.Fatalf("their block list: %+v", list)
	}

	// Lifted: the line is open again, and the ring goes through.
	if err := s.Unblock(ctx, me, you.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, you, conv, "after"); err != nil {
		t.Fatalf("after unblock: %v", err)
	}
	s.Ring(ctx, you, me.ID, "room-2")
	if calls, _ := s.CallsFor(ctx, me, conv); len(calls) != 1 {
		t.Fatalf("ring after unblock not logged: %+v", calls)
	}
	if mine, _ := s.List(ctx, me); mine[0].Blocked {
		t.Fatal("still marked blocked after unblock")
	}
	// Idempotent both ways.
	if err := s.Unblock(ctx, me, you.ID); err != nil {
		t.Fatal(err)
	}
	s.Block(ctx, me, you.ID)
	if err := s.Block(ctx, me, you.ID); err != nil {
		t.Fatalf("blocking twice: %v", err)
	}
	if n, _ := s.repo.BlockCount(ctx, me.ID); n != 1 {
		t.Fatalf("blocking twice made %d rows", n)
	}
}

func TestBlocksAreBoundedAndOnlyRealPeople(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	if err := s.Block(ctx, me, "u-nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocking an unknown id: %v", err)
	}
	if err := s.Block(ctx, me, me.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocking yourself: %v", err)
	}
	if err := s.Block(ctx, me, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocking nobody: %v", err)
	}
	// The bound, one over the line. Rows are written directly because the
	// directory fake knows two people; the count is what the bound reads.
	repo := s.repo.(*SQLRepository)
	for i := 0; i < MaxBlocks; i++ {
		if err := repo.Block(ctx, me.ID, fmt.Sprintf("u-%d", i), now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Block(ctx, me, you.ID); !errors.Is(err, ErrTooManyBlocks) {
		t.Fatalf("one over the bound: %v", err)
	}
	// Groups are never blocked: the two still talk in one.
	g, err := s.CreateGroup(ctx, me, "both", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	repo.Block(ctx, me.ID, you.ID, now())
	if _, err := s.Send(ctx, you, g.ID, "in the group"); err != nil {
		t.Fatalf("a block reached a group: %v", err)
	}
}

func TestSearchInOneConversationStaysInIt(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	s.Send(ctx, you, conv, "the budget spreadsheet")
	own, _ := s.OpenSelf(ctx, me)
	s.Send(ctx, me, own.ID, "budget notes to self")
	all, _ := s.SearchIn(ctx, me, "budget", "", 10)
	if len(all) != 2 {
		t.Fatalf("all conversations: %d", len(all))
	}
	one, _ := s.SearchIn(ctx, me, "budget", conv, 10)
	if len(one) != 1 || one[0].ConversationID != conv {
		t.Fatalf("one conversation: %+v", one)
	}
	// A conversation the searcher is not in answers nothing, not an error.
	if got, err := s.SearchIn(ctx, you, "budget", own.ID, 10); err != nil || len(got) != 0 {
		t.Fatalf("a stranger searched somebody's notes: %v %d", err, len(got))
	}
}
