package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Availability: validated, bounded, expiring, and visible only to peers.
func TestStatusIsMineToSetAndMyPeersToSee(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	if _, err := s.SetStatus(ctx, me, "sleeping", "", nil); !errors.Is(err, ErrBadStatus) {
		t.Fatalf("made-up status: %v", err)
	}
	if _, err := s.SetStatus(ctx, me, StatusBusy, strings.Repeat("x", MaxStatusNote+1), nil); !errors.Is(err, ErrBadStatus) {
		t.Fatalf("note one over the bound: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if _, err := s.SetStatus(ctx, me, StatusBusy, "", &past); !errors.Is(err, ErrBadStatus) {
		t.Fatalf("expiry in the past: %v", err)
	}
	far := time.Now().Add(MaxStatusUntil + time.Hour)
	if _, err := s.SetStatus(ctx, me, StatusBusy, "", &far); !errors.Is(err, ErrBadStatus) {
		t.Fatalf("expiry too far: %v", err)
	}
	until := time.Now().Add(time.Hour)
	st, err := s.SetStatus(ctx, me, "DND", " In a meeting  until 3 ", &until)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusDND || st.Note != "In a meeting until 3" || st.Until == nil {
		t.Fatalf("stored: %+v", st)
	}
	// My peer sees it; a stranger asking gets nothing, not "unknown".
	got, _ := s.Statuses(ctx, you, []string{me.ID})
	if got[me.ID].Status != StatusDND || got[me.ID].Note != "In a meeting until 3" {
		t.Fatalf("peer's view: %+v", got)
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if got, _ := s.Statuses(ctx, stranger, []string{me.ID}); len(got) != 0 {
		t.Fatalf("a stranger read a status: %+v", got)
	}
	// Expired reads as available.
	repo := s.repo.(*SQLRepository)
	repo.db.ExecContext(ctx, `UPDATE messaging_status SET until = ? WHERE user_id = ?`, time.Now().Add(-time.Second).UnixMilli(), me.ID)
	got, _ = s.Statuses(ctx, you, []string{me.ID})
	if got[me.ID].Status != StatusAvailable || got[me.ID].Note != "" {
		t.Fatalf("expired status should read available: %+v", got[me.ID])
	}
	// The query is bounded.
	many := make([]string, MaxStatusQuery+50)
	for i := range many {
		many[i] = me.ID
	}
	if _, err := s.Statuses(ctx, you, many); err != nil {
		t.Fatal(err)
	}
}

// Polls: shape checked at compose time; votes are per member, replace, and
// are tallied the same for everyone; anonymous polls carry no names.
func TestPollsTallyForEveryoneAndVotesArePerMember(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	bad := []string{
		`{"poll":{"q":"","options":["a","b"]}}`,
		`{"poll":{"q":"Lunch?","options":["a"]}}`,
		`{"poll":{"q":"Lunch?","options":["a","b","c","d","e","f","g","h","i","j","k"]}}`,
		`{"poll":{"q":"Lunch?","options":["a",""]}}`,
		`{"poll":{"q":"` + strings.Repeat("q", MaxPollText+1) + `","options":["a","b"]}}`,
	}
	for _, meta := range bad {
		if _, err := s.Compose(ctx, me, conv, "poll", "", nil, nil, SendOptions{Meta: meta}); !errors.Is(err, ErrBadPoll) {
			t.Fatalf("%s: err=%v", meta, err)
		}
	}
	m, err := s.Compose(ctx, me, conv, "Lunch?", "", nil, nil, SendOptions{Meta: `{"poll":{"q":"Lunch?","options":["Pizza","Sushi","Salad"]}}`})
	if err != nil {
		t.Fatal(err)
	}
	// Not a poll, not an option, two options on a single-choice poll.
	plain, _ := s.Send(ctx, me, conv, "not a poll")
	if _, err := s.Vote(ctx, you, plain.ID, []int{0}); !errors.Is(err, ErrNotAPoll) {
		t.Fatalf("voting on text: %v", err)
	}
	if _, err := s.Vote(ctx, you, m.ID, []int{3}); !errors.Is(err, ErrBadVote) {
		t.Fatalf("option off the end: %v", err)
	}
	if _, err := s.Vote(ctx, you, m.ID, []int{0, 1}); !errors.Is(err, ErrBadVote) {
		t.Fatalf("two options on single-choice: %v", err)
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, err := s.Vote(ctx, stranger, m.ID, []int{0}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger voted: %v", err)
	}
	// Two votes, then one changes their mind.
	if _, err := s.Vote(ctx, you, m.ID, []int{1}); err != nil {
		t.Fatal(err)
	}
	mine, err := s.Vote(ctx, me, m.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if got := mine.Poll; got == nil || got.Total != 2 || got.Counts[1] != 2 || len(got.Mine) != 1 || got.Mine[0] != 1 {
		t.Fatalf("after two votes: %+v", got)
	}
	if len(mine.Poll.Voters[1]) != 2 {
		t.Fatalf("voters named on an open poll: %+v", mine.Poll.Voters)
	}
	changed, _ := s.Vote(ctx, you, m.ID, []int{2})
	if changed.Poll.Counts[1] != 1 || changed.Poll.Counts[2] != 1 || changed.Poll.Total != 2 {
		t.Fatalf("a changed vote should move, not add: %+v", changed.Poll)
	}
	withdrawn, _ := s.Vote(ctx, you, m.ID, nil)
	if withdrawn.Poll.Counts[2] != 0 || withdrawn.Poll.Total != 1 {
		t.Fatalf("withdrawn: %+v", withdrawn.Poll)
	}
	// The read path carries the same tally, with "mine" per reader.
	theirs := bodiesFull(t, s, you, conv)
	var seen *Message
	for _, x := range theirs {
		if x.ID == m.ID {
			seen = x
		}
	}
	if seen == nil || seen.Poll == nil || seen.Poll.Counts[1] != 1 || len(seen.Poll.Mine) != 0 {
		t.Fatalf("their read: %+v", seen)
	}
	// Anonymous: counts, no names.
	anon, _ := s.Compose(ctx, me, conv, "Secret?", "", nil, nil, SendOptions{Meta: `{"poll":{"q":"Secret?","options":["Yes","No"],"anon":true,"multi":true}}`})
	v, _ := s.Vote(ctx, you, anon.ID, []int{0, 1})
	if v.Poll.Total != 1 || v.Poll.Counts[0] != 1 || v.Poll.Counts[1] != 1 || v.Poll.Voters != nil {
		t.Fatalf("anonymous multi: %+v", v.Poll)
	}
}

// @everyone expands to the members of a group, counted as mentions for each
// of them, and is ignored in a direct chat.
func TestEveryoneMentionReachesEveryMember(t *testing.T) {
	s, me, you, direct := twoPeople(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "all hands", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	// A message in the same millisecond as the member's cursor is not after
	// it; real sends are.
	time.Sleep(2 * time.Millisecond)
	if _, err := s.SendMessage(ctx, me, g.ID, "@everyone standup in 5", "", nil, []string{MentionEveryone}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, you)
	var mentions int
	for _, c := range list {
		if c.ID == g.ID {
			mentions = c.Mentions
		}
	}
	if mentions != 1 {
		t.Fatalf("the other member was not counted as mentioned: %d", mentions)
	}
	m, _ := s.SendMessage(ctx, me, direct, "@everyone here", "", nil, []string{MentionEveryone})
	got := bodiesFull(t, s, you, direct)
	for _, x := range got {
		if x.ID == m.ID && len(x.Mentions) != 0 {
			t.Fatalf("@everyone in a direct chat should mention nobody: %+v", x.Mentions)
		}
	}
}

// Deleting a channel: owner only, and nothing survives it.
func TestDeleteChannelIsTheOwnersAndTakesEverything(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	c, err := s.CreateChannel(ctx, me, "doomed", "", VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinChannel(ctx, you, c.ID); err != nil {
		t.Fatal(err)
	}
	s.Send(ctx, you, c.ID, "hello")
	if err := s.DeleteChannel(ctx, you, c.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a member deleted a channel: %v", err)
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if err := s.DeleteChannel(ctx, stranger, c.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger deleted a channel: %v", err)
	}
	if err := s.DeleteChannel(ctx, me, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.ConversationByID(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel still there: %v", err)
	}
	for _, x := range mustList(t, s, you) {
		if x.ID == c.ID {
			t.Fatal("deleted channel still listed")
		}
	}
	if _, err := s.Messages(ctx, you, c.ID, time.Time{}, 10); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("history readable after delete: %v", err)
	}
	// A group is not a channel; the endpoint refuses it.
	g, _ := s.CreateGroup(ctx, me, "grp", []string{you.Email})
	if err := s.DeleteChannel(ctx, me, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a group went through the channel delete: %v", err)
	}
}

func mustList(t *testing.T, s *Service, a Actor) []*Conversation {
	t.Helper()
	cs, err := s.List(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
