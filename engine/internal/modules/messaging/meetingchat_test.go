package messaging

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A meeting of three or more gets a conversation; a call of two does not.
// What these hold: the threshold counts signed-in accounts, not devices or
// guests; the chat is one per room and outlives the meeting; later arrivals
// are added and told so in the thread; a scheduled title names it and an
// ad-hoc room leaves it to the people; only members may look it up; two
// joins racing to create it produce one.

func TestTwoIsACallThreeIsAMeeting(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	room := "weekly-sync"
	people := []MeetingPerson{{me.ID, me.Name}, {you.ID, you.Name}}
	s.MeetingJoined(ctx, room, "Weekly sync", people)
	if _, err := s.MeetingChat(ctx, me, room); !errors.Is(err, ErrNotFound) {
		t.Fatalf("two people: a chat exists: err=%v", err)
	}
	// Same person on a second device, and a guest: still two accounts.
	s.MeetingJoined(ctx, room, "Weekly sync", append(people, MeetingPerson{me.ID, me.Name}, MeetingPerson{"", "Guest"}))
	if _, err := s.MeetingChat(ctx, me, room); !errors.Is(err, ErrNotFound) {
		t.Fatalf("two accounts on three devices plus a guest: a chat exists: err=%v", err)
	}
	// The third account arrives.
	people = append(people, MeetingPerson{third.ID, third.Name})
	s.MeetingJoined(ctx, room, "Weekly sync", people)
	c, err := s.MeetingChat(ctx, third, room)
	if err != nil {
		t.Fatalf("three people: %v", err)
	}
	if c.Kind != KindGroup || c.MeetingRoom != room || c.Title != "Weekly sync" || c.MemberCount != 3 {
		t.Fatalf("meeting chat: %+v", c)
	}
	// Idempotent.
	s.MeetingJoined(ctx, room, "Weekly sync", people)
	again, _ := s.MeetingChat(ctx, me, room)
	if again.ID != c.ID || again.MemberCount != 3 {
		t.Fatalf("a repeat join changed the chat: %+v", again)
	}
	ms, _ := s.Messages(ctx, me, c.ID, time.Time{}, 10)
	if len(ms) != 1 || !ms[0].System || ms[0].Body != "started a meeting" {
		t.Fatalf("thread after creation: %+v", ms)
	}
	// It is in everybody's list, named after the meeting.
	list, _ := s.List(ctx, you)
	var found *Conversation
	for _, x := range list {
		if x.ID == c.ID {
			found = x
		}
	}
	if found == nil || found.Display != "Weekly sync" || found.MeetingRoom != room {
		t.Fatalf("in the list: %+v", found)
	}
}

func TestLaterArrivalsJoinTheMeetingChat(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	room := "standup"
	s.MeetingJoined(ctx, room, "", []MeetingPerson{{me.ID, me.Name}, {you.ID, you.Name}, {third.ID, third.Name}})
	c, _ := s.MeetingChat(ctx, me, room)
	// An ad-hoc room: no title, named after its people.
	if c.Title != "" || c.Display != "Other and Third" {
		t.Fatalf("ad-hoc meeting chat name: title %q display %q", c.Title, c.Display)
	}
	// Third leaves, a fourth arrives later; the fourth is added and the
	// thread says so.
	fourth := Actor{ID: "u-out", Email: "out@elsewhere.example", Name: "Outsider"}
	s.MeetingJoined(ctx, room, "", []MeetingPerson{{me.ID, me.Name}, {you.ID, you.Name}, {fourth.ID, fourth.Name}})
	c2, err := s.MeetingChat(ctx, fourth, room)
	if err != nil {
		t.Fatalf("fourth looking up the chat: %v", err)
	}
	if c2.ID != c.ID || c2.MemberCount != 4 {
		t.Fatalf("after the fourth: %+v", c2)
	}
	ms, _ := s.Messages(ctx, me, c.ID, time.Time{}, 10)
	if last := ms[len(ms)-1]; !last.System || last.Body != "joined the meeting" || last.SenderID != fourth.ID {
		t.Fatalf("join event: %+v", last)
	}
	// Somebody who was never in the meeting cannot look it up.
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, err := s.MeetingChat(ctx, stranger, room); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: err=%v", err)
	}
	// Even a two-person return to the room lands in the existing chat: a
	// fifth person joining a later, smaller session is still added.
	fifth := MeetingPerson{"u-third", "Third"}
	s.MeetingJoined(ctx, room, "", []MeetingPerson{{me.ID, me.Name}, fifth})
	c3, _ := s.MeetingChat(ctx, me, room)
	if c3.MemberCount != 4 {
		t.Fatalf("a member returning is not added twice: %d", c3.MemberCount)
	}
}

func TestRacingJoinsCreateOneMeetingChat(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	room := "race"
	people := []MeetingPerson{{me.ID, me.Name}, {you.ID, you.Name}, {third.ID, third.Name}}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			s.MeetingJoined(ctx, room, "Race", people)
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	list, _ := s.List(ctx, me)
	n := 0
	for _, c := range list {
		if c.MeetingRoom == room {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d meeting chats for one room", n)
	}
}
