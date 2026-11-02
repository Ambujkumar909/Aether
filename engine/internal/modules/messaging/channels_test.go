package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Channels and groups. What these hold: names are unique; private is
// invisible and unjoinable; public is browsable and joinable only inside the
// organisation rule; owners rename and remove, members add and leave; an
// owner who leaves hands over; membership events are in the thread and never
// unread; a big channel ships its count, not its people; pushes reach every
// member without loading them.

// A directory of three, with a fourth outside the organisation.
type fourDir struct{}

func (fourDir) LookupByEmail(ctx context.Context, email string) (string, string, error) {
	switch strings.ToLower(email) {
	case "me@example.com":
		return "u-me", "Me", nil
	case "other@example.com":
		return "u-you", "Other", nil
	case "third@example.com":
		return "u-third", "Third", nil
	case "out@elsewhere.example":
		return "u-out", "Outsider", nil
	}
	return "", "", ErrNotFound
}

func (fourDir) LookupByID(ctx context.Context, id string) (string, string, error) {
	switch id {
	case "u-me":
		return "me@example.com", "Me", nil
	case "u-you":
		return "other@example.com", "Other", nil
	case "u-third":
		return "third@example.com", "Third", nil
	case "u-out":
		return "out@elsewhere.example", "Outsider", nil
	}
	return "", "", ErrNotFound
}

func channelService(t *testing.T) (*Service, Actor, Actor, Actor, Actor) {
	t.Helper()
	s := serviceForTest(t)
	s.dir = fourDir{}
	// The organisation rule: example.com is one company; elsewhere is not.
	s.MayMessage = func(ctx context.Context, from, to string) bool {
		return strings.HasSuffix(from, "@example.com") == strings.HasSuffix(to, "@example.com")
	}
	return s,
		Actor{ID: "u-me", Email: "me@example.com", Name: "Me"},
		Actor{ID: "u-you", Email: "other@example.com", Name: "Other"},
		Actor{ID: "u-third", Email: "third@example.com", Name: "Third"},
		Actor{ID: "u-out", Email: "out@elsewhere.example", Name: "Outsider"}
}

func TestChannelNamesAreUniqueAndCleaned(t *testing.T) {
	s, me, you, _, _ := channelService(t)
	ctx := context.Background()
	c, err := s.CreateChannel(ctx, me, "  #Design   Reviews ", "where mocks go", VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "Design Reviews" || c.Display != "#Design Reviews" || c.Kind != KindChannel {
		t.Fatalf("channel %+v", c)
	}
	if c.MemberCount != 1 || c.Members[0].Role != RoleOwner {
		t.Fatalf("creator should be the sole owner: %+v", c.Members)
	}
	if _, err := s.CreateChannel(ctx, you, "design reviews", "", VisibilityPrivate); !errors.Is(err, ErrChannelExists) {
		t.Fatalf("a second channel with the same name (different case): err=%v", err)
	}
	if _, err := s.CreateChannel(ctx, me, "   ", "", VisibilityPublic); !errors.Is(err, ErrBadName) {
		t.Fatalf("blank name: err=%v", err)
	}
	long := strings.Repeat("n", 200)
	c2, _ := s.CreateChannel(ctx, me, long, strings.Repeat("t", 1000), VisibilityPublic)
	if len([]rune(c2.Title)) != MaxChannelName || len([]rune(c2.Topic)) != MaxChannelTopic {
		t.Fatalf("name/topic not bounded: %d/%d", len(c2.Title), len(c2.Topic))
	}
}

func TestPrivateIsInvisibleAndPublicIsJoinableInsideTheOrg(t *testing.T) {
	s, me, you, _, out := channelService(t)
	ctx := context.Background()
	pub, _ := s.CreateChannel(ctx, me, "general", "", VisibilityPublic)
	priv, _ := s.CreateChannel(ctx, me, "leadership", "", VisibilityPrivate)

	list, err := s.BrowseChannels(ctx, you, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != pub.ID || list[0].Joined {
		t.Fatalf("a colleague browsing: %+v (want only the public one, not joined)", list)
	}
	if _, err := s.JoinChannel(ctx, you, priv.ID); !errors.Is(err, ErrNotJoinable) {
		t.Fatalf("joining a private channel: err=%v", err)
	}
	if _, err := s.Messages(ctx, you, priv.ID, time.Time{}, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reading a private channel: err=%v", err)
	}

	// The outsider sees nothing and may join nothing.
	if list, _ := s.BrowseChannels(ctx, out, "", 50); len(list) != 0 {
		t.Fatalf("an outsider browsing the company's channels: %+v", list)
	}
	if _, err := s.JoinChannel(ctx, out, pub.ID); !errors.Is(err, ErrExternalClosed) {
		t.Fatalf("an outsider joining: err=%v", err)
	}

	// A colleague joins, twice (idempotent), and the count and role follow.
	j, err := s.JoinChannel(ctx, you, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinChannel(ctx, you, pub.ID); err != nil {
		t.Fatalf("second join: %v", err)
	}
	if j.MemberCount != 2 || j.MyRole != "" && j.MyRole != RoleMember {
		t.Fatalf("after joining: count %d role %q", j.MemberCount, j.MyRole)
	}
	list, _ = s.BrowseChannels(ctx, you, "gen", 50)
	if len(list) != 1 || !list[0].Joined || list[0].MemberCount != 2 {
		t.Fatalf("browse after joining: %+v", list)
	}
	// The join is an event in the thread, and not unread for anybody.
	ms, _ := s.Messages(ctx, me, pub.ID, time.Time{}, 10)
	last := ms[len(ms)-1]
	if !last.System || last.Body != "joined" || last.SenderID != you.ID {
		t.Fatalf("join event: %+v", last)
	}
	mine, _ := s.List(ctx, me)
	for _, c := range mine {
		if c.ID == pub.ID && c.Unread != 0 {
			t.Fatalf("a membership event counted as unread: %d", c.Unread)
		}
	}
}

func TestOwnersAndMembers(t *testing.T) {
	s, me, you, third, out := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}

	// Any member adds; outsiders and unknowns are skipped and named.
	c, skipped, err := s.AddMembers(ctx, you, g.ID, []string{third.Email, out.Email, "nobody@example.com", third.Email})
	if err != nil {
		t.Fatal(err)
	}
	if c.MemberCount != 3 || len(skipped) != 2 {
		t.Fatalf("after adding: count %d skipped %v", c.MemberCount, skipped)
	}
	ms, _ := s.Messages(ctx, me, g.ID, time.Time{}, 10)
	if last := ms[len(ms)-1]; !last.System || last.Body != "added Third" {
		t.Fatalf("add event: %+v", last)
	}

	// A member may not remove somebody else; the owner may.
	if err := s.RemoveMember(ctx, you, g.ID, third.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("member removing member: err=%v", err)
	}
	if err := s.RemoveMember(ctx, me, g.ID, third.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.repo.IsMember(ctx, g.ID, third.ID); ok {
		t.Fatal("removed member still in")
	}
	if _, err := s.Messages(ctx, third, g.ID, time.Time{}, 10); !errors.Is(err, ErrForbidden) {
		t.Fatal("a removed member can still read")
	}

	// Renaming a group: any member. A channel: owner only.
	if _, err := s.Rename(ctx, you, g.ID, "Launch 2", "ship it"); err != nil {
		t.Fatalf("member renaming a group: %v", err)
	}
	ch, _ := s.CreateChannel(ctx, me, "eng", "", VisibilityPublic)
	s.JoinChannel(ctx, you, ch.ID)
	if _, err := s.Rename(ctx, you, ch.ID, "engineering", ""); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("member renaming a channel: err=%v", err)
	}
	if _, err := s.Rename(ctx, me, ch.ID, "engineering", "all things eng"); err != nil {
		t.Fatal(err)
	}

	// The owner leaves; the longest-standing member inherits.
	if err := s.Leave(ctx, me, ch.ID); err != nil {
		t.Fatal(err)
	}
	m, _ := s.repo.MemberOf(ctx, ch.ID, you.ID)
	if m == nil || m.Role != RoleOwner {
		t.Fatalf("after the owner left: %+v", m)
	}
	// Direct conversations never change shape.
	d, _ := s.OpenDirect(ctx, me, you.Email)
	if _, _, err := s.AddMembers(ctx, me, d.ID, []string{third.Email}); !errors.Is(err, ErrDirectFixed) {
		t.Fatalf("adding to a direct: err=%v", err)
	}
	if err := s.Leave(ctx, me, d.ID); !errors.Is(err, ErrDirectFixed) {
		t.Fatalf("leaving a direct: err=%v", err)
	}
	// Strangers can do nothing.
	if _, _, err := s.AddMembers(ctx, out, g.ID, []string{third.Email}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger adding: err=%v", err)
	}
	if _, _, err := s.Members(ctx, out, g.ID, 0, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger listing members: err=%v", err)
	}
}

func TestABigChannelShipsItsCountNotItsPeople(t *testing.T) {
	s, me, _, _, _ := channelService(t)
	ctx := context.Background()
	ch, _ := s.CreateChannel(ctx, me, "all-hands", "", VisibilityPublic)
	var ms []Member
	now := time.Now().UTC()
	for i := 0; i < 500; i++ {
		ms = append(ms, Member{UserID: fmt.Sprintf("u%04d", i), Email: fmt.Sprintf("u%04d@example.com", i),
			Name: fmt.Sprintf("Person %d", i), JoinedAt: now, LastRead: now})
	}
	if err := s.repo.AddMembers(ctx, ch.ID, ms); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, me)
	var got *Conversation
	for _, c := range list {
		if c.ID == ch.ID {
			got = c
		}
	}
	if got == nil || got.MemberCount != 501 || len(got.Members) != 0 {
		t.Fatalf("big channel in the list: count %d, %d members shipped", got.MemberCount, len(got.Members))
	}
	page, total, err := s.Members(ctx, me, ch.ID, 0, 50)
	if err != nil || total != 501 || len(page) != 50 || page[0].Role != RoleOwner {
		t.Fatalf("members page: %d of %d, err %v, first %+v", len(page), total, err, page[0])
	}
	// Pushes still reach everybody: ids, not rows.
	ids, _ := s.repo.MemberIDs(ctx, ch.ID)
	if len(ids) != 501 {
		t.Fatalf("%d member ids", len(ids))
	}
	t0 := time.Now()
	if _, err := s.Send(ctx, me, ch.ID, "hello everyone"); err != nil {
		t.Fatal(err)
	}
	// 200 ms is the budget a person notices. Under the race detector, with
	// the whole package running concurrently, the same work takes several
	// times longer for reasons that are the detector's, not the code's --
	// measured: 1.5 s alone, over 15 s in the full run once -- so the budget
	// widens there and the plain run keeps the real number.
	budget := 200 * time.Millisecond
	if raceEnabled {
		budget = 5 * time.Second
	}
	if d := time.Since(t0); d > budget {
		t.Fatalf("a send to 501 people took %s (budget %s)", d, budget)
	}
}
