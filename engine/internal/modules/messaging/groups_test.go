package messaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Groups, the rest of the way. What these hold: a nameless group is named
// after its people and follows them; history on joining is the adder's
// choice and touches only the newcomer's view; mentions reach members only
// and are counted like unread; the picture is image-only, member-only and
// bounded; a pin is one message from this conversation; typing and read
// cursors need membership.

func TestANamelessGroupIsNamedAfterItsPeople(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "  ", []string{you.Email})
	if err != nil {
		t.Fatal(err)
	}
	if g.Title != "" {
		t.Fatalf("an empty title should stay empty, got %q", g.Title)
	}
	if g.Display != "Other" {
		t.Fatalf("display for me: %q, want the other person", g.Display)
	}
	mine, _ := s.List(ctx, you)
	if mine[0].Display != "Me" {
		t.Fatalf("display for them: %q, want me", mine[0].Display)
	}
	s.AddMembers(ctx, me, g.ID, []string{third.Email})
	mine, _ = s.List(ctx, me)
	if mine[0].Display != "Other and Third" {
		t.Fatalf("after adding: %q", mine[0].Display)
	}
	// A real title wins.
	s.Rename(ctx, me, g.ID, "Launch", "")
	mine, _ = s.List(ctx, me)
	if mine[0].Display != "Launch" {
		t.Fatalf("renamed: %q", mine[0].Display)
	}
	// Many people: first names and a count; a big group with no members
	// shipped: the count alone.
	c := &Conversation{Kind: KindGroup, MemberCount: 9, Members: []Member{
		{UserID: "a", Name: "Priya Sharma"}, {UserID: "b", Name: "Rahul Verma"}, {UserID: "c", Name: "Neha"},
		{UserID: "d", Name: "Arjun"}, {UserID: "e", Name: "Kavya"}, {UserID: "f", Name: "Rohan"},
		{UserID: "g", Name: "Sneha"}, {UserID: "h", Name: "Vikram"}, {UserID: "me", Name: "Me"},
	}}
	if got := displayName(c, "me"); got != "Priya, Rahul and 6 others" {
		t.Fatalf("nine people: %q", got)
	}
	big := &Conversation{Kind: KindGroup, MemberCount: 400}
	if got := displayName(big, "me"); got != "Group · 399 people" {
		t.Fatalf("big group: %q", got)
	}
}

func TestHistoryOnJoiningIsTheAddersChoice(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	g, _ := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	// Plant one old and one recent message directly, so "days" has an edge.
	oldMsg := &Message{ID: newID(), ConversationID: g.ID, SenderID: me.ID, SenderName: "Me", Body: "ancient", CreatedAt: old}
	if err := s.repo.AddMessage(ctx, oldMsg); err != nil {
		t.Fatal(err)
	}
	s.Send(ctx, you, g.ID, "recent")

	// None: the newcomer sees nothing that came before.
	if _, _, err := s.AddMembers(ctx, me, g.ID, []string{third.Email}, History{Mode: HistoryNone}); err != nil {
		t.Fatal(err)
	}
	got := bodies(t, s, third, g.ID)
	for _, b := range got {
		if b == "ancient" || b == "recent" {
			t.Fatalf("newcomer with no history sees %v", got)
		}
	}
	if list, _ := s.List(ctx, third); list[0].Unread != 0 {
		t.Fatalf("hidden history counted as unread: %d", list[0].Unread)
	}
	// The others still see everything.
	if got := bodies(t, s, you, g.ID); len(got) < 2 {
		t.Fatalf("the adder's view shrank: %v", got)
	}

	// Days: a second newcomer sees the recent message but not the ancient one.
	s.RemoveMember(ctx, me, g.ID, third.ID)
	fresh := Actor{ID: "u-third", Email: "third@example.com", Name: "Third"}
	// Re-adding the same person: the earlier hidden rows remain (they were
	// theirs), so use the days mode on a message newer than both.
	if _, _, err := s.AddMembers(ctx, me, g.ID, []string{fresh.Email}, History{Mode: HistoryDays, Days: 7}); err != nil {
		t.Fatal(err)
	}
	got = bodies(t, s, fresh, g.ID)
	for _, b := range got {
		if b == "ancient" {
			t.Fatalf("a ten-day-old message shown under a seven-day window: %v", got)
		}
	}
}

func TestMentionsReachMembersOnlyAndAreCounted(t *testing.T) {
	s, me, you, third, out := channelService(t)
	ctx := context.Background()
	g, _ := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	time.Sleep(2 * time.Millisecond)

	// third and the outsider are not in the group: dropped, not refused.
	m, err := s.SendMessage(ctx, me, g.ID, "@Other @Third look", "", nil, []string{you.ID, third.ID, out.ID, you.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Mentions) != 1 || m.Mentions[0] != you.ID {
		t.Fatalf("mentions kept: %v", m.Mentions)
	}
	list, _ := s.List(ctx, you)
	if list[0].Unread != 1 || list[0].Mentions != 1 {
		t.Fatalf("their badge: unread %d mentions %d", list[0].Unread, list[0].Mentions)
	}
	// Read back, the message says who it names.
	ms, _ := s.Messages(ctx, you, g.ID, time.Time{}, 10)
	s.Decorate(ctx, you, ms)
	if last := ms[len(ms)-1]; len(last.Mentions) != 1 || last.Mentions[0] != you.ID {
		t.Fatalf("mentions on read: %v", last.Mentions)
	}
	time.Sleep(2 * time.Millisecond)
	s.MarkRead(ctx, you, g.ID)
	list, _ = s.List(ctx, you)
	if list[0].Mentions != 0 {
		t.Fatalf("mentions after reading: %d", list[0].Mentions)
	}
	// Mentioning yourself is allowed and counts for nobody.
	s.SendMessage(ctx, me, g.ID, "note to @Me", "", nil, []string{me.ID})
	list, _ = s.List(ctx, me)
	if list[0].Mentions != 0 {
		t.Fatalf("a self-mention counted: %d", list[0].Mentions)
	}
	// Bounded.
	many := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, you.ID)
	}
	m, _ = s.SendMessage(ctx, me, g.ID, "x", "", nil, many)
	if len(m.Mentions) != 1 {
		t.Fatalf("forty of the same mention: %d kept", len(m.Mentions))
	}
}

func TestAPictureIsImageOnlyMemberOnlyAndBounded(t *testing.T) {
	s, me, you, _, out := channelService(t)
	ctx := context.Background()
	fs, _ := NewDiskStore(t.TempDir())
	s.UseFiles(fs)
	g, _ := s.CreateGroup(ctx, me, "Ops", []string{you.Email})

	html := strings.NewReader("<html><script>1</script></html>")
	if err := s.SetAvatar(ctx, me, g.ID, "evil.png", "image/png", html); !errors.Is(err, ErrNotAnImage) {
		t.Fatalf("HTML declared as PNG: err=%v", err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	if err := s.SetAvatar(ctx, out, g.ID, "p.png", "image/png", bytes.NewReader(png)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider setting the picture: err=%v", err)
	}
	if err := s.SetAvatar(ctx, me, g.ID, "p.png", "image/png", bytes.NewReader(png)); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, you)
	if !list[0].Avatar {
		t.Fatal("the picture is not announced on the list")
	}
	mime, f, _, err := s.OpenAvatar(ctx, you, g.ID)
	if err != nil || mime != "image/png" {
		t.Fatalf("member reading the picture: %v %q", err, mime)
	}
	f.Close()
	if _, _, _, err := s.OpenAvatar(ctx, out, g.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider reading the picture: err=%v", err)
	}
	// Too big, one byte over.
	big := io.LimitReader(io.MultiReader(bytes.NewReader(png), zeroReader{}), MaxAvatarBytes+1)
	if err := s.SetAvatar(ctx, me, g.ID, "big.png", "image/png", big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized picture: err=%v", err)
	}
	d, _ := s.OpenDirect(ctx, me, you.Email)
	if err := s.SetAvatar(ctx, me, d.ID, "p.png", "image/png", bytes.NewReader(png)); !errors.Is(err, ErrDirectFixed) {
		t.Fatalf("picture on a direct conversation: err=%v", err)
	}
}

func TestPinIsOneMessageFromThisConversation(t *testing.T) {
	s, me, you, _, out := channelService(t)
	ctx := context.Background()
	g, _ := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	other, _ := s.OpenSelf(ctx, me)
	m, _ := s.Send(ctx, you, g.ID, "the address")
	foreign, _ := s.Send(ctx, me, other.ID, "elsewhere")

	if err := s.Pin(ctx, you, g.ID, m.ID); err != nil {
		t.Fatalf("a member pinning: %v", err)
	}
	list, _ := s.List(ctx, me)
	if list[0].PinnedMessageID != m.ID || list[0].Pinned == nil || list[0].Pinned.Body != "the address" {
		t.Fatalf("pinned on the list: %+v %+v", list[0].PinnedMessageID, list[0].Pinned)
	}
	if err := s.Pin(ctx, me, g.ID, foreign.ID); !errors.Is(err, ErrNotInConv) {
		t.Fatalf("pinning a message from another conversation: err=%v", err)
	}
	if err := s.Pin(ctx, out, g.ID, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider pinning: err=%v", err)
	}
	if err := s.Pin(ctx, me, g.ID, ""); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, me)
	if list[0].PinnedMessageID != "" || list[0].Pinned != nil {
		t.Fatal("unpin did not clear")
	}
}

func TestTypingAndReadCursorsNeedMembership(t *testing.T) {
	s, me, you, _, out := channelService(t)
	ctx := context.Background()
	g, _ := s.CreateGroup(ctx, me, "Ops", []string{you.Email})
	// Neither call has a return worth asserting for the outsider; what
	// matters is that they do not reach the hub. A hub with no sockets makes
	// that hard to observe directly, so this asserts the gate itself.
	if err := s.MarkRead(ctx, out, g.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider moving a cursor: err=%v", err)
	}
	if ok, _ := s.repo.IsMember(ctx, g.ID, out.ID); ok {
		t.Fatal("precondition")
	}
	s.Typing(ctx, out, g.ID, true) // must not panic, must not push
	// And a big audience gets no ephemeral traffic.
	ch, _ := s.CreateChannel(ctx, me, "all", "", VisibilityPublic)
	var ms []Member
	now := time.Now().UTC()
	for i := 0; i < seenByThreshold+5; i++ {
		ms = append(ms, Member{UserID: "u" + itoa(i), Email: "u" + itoa(i) + "@example.com", Name: "U", JoinedAt: now, LastRead: now})
	}
	s.repo.AddMembers(ctx, ch.ID, ms)
	if got := s.smallAudience(ctx, ch.ID); got != nil {
		t.Fatalf("a %d-member channel is not a small audience: %d ids", len(ms)+1, len(got))
	}
	if got := s.smallAudience(ctx, g.ID); len(got) != 2 {
		t.Fatalf("a two-person group: %d ids", len(got))
	}
}
