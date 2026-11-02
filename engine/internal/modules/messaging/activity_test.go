package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

func feedOf(t *testing.T, s *Service, a Actor, kind string, unread bool) ([]*Activity, int) {
	t.Helper()
	items, n, err := s.Activities(context.Background(), a, "", kind, unread, 100)
	if err != nil {
		t.Fatal(err)
	}
	return items, n
}

func kindsOf(items []*Activity) string {
	var out []string
	for _, x := range items {
		out = append(out, x.Kind+":"+x.ActorName)
	}
	return strings.Join(out, ",")
}

// Mentions, replies and reactions land in the right person's feed, once;
// the row reads through to what is stored now.
func TestActivityMentionsRepliesReactions(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}

	// Me mentions you: one row for you, none for me or third.
	m1, err := s.SendMessage(ctx, me, g.ID, "@Other can you check the build?", "", nil, []string{you.ID})
	if err != nil {
		t.Fatal(err)
	}
	items, n := feedOf(t, s, you, "", false)
	if len(items) != 1 || n != 1 || items[0].Kind != ActivityMention || items[0].ActorID != me.ID ||
		items[0].ConvID != g.ID || items[0].ConvTitle != "Launch" || items[0].MessageID != m1.ID || items[0].Read {
		t.Fatalf("you after a mention: n=%d %+v", n, items)
	}
	if items, _ := feedOf(t, s, me, "", false); len(items) != 0 {
		t.Fatalf("the sender has activity: %s", kindsOf(items))
	}
	// Nor from mentioning yourself, or replying to yourself.
	if _, err := s.SendMessage(ctx, me, g.ID, "@Me note to self", m1.ID, nil, []string{me.ID}); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, me, "", false); len(items) != 0 {
		t.Fatalf("activity from your own message: %s", kindsOf(items))
	}
	if items, _ := feedOf(t, s, third, "", false); len(items) != 0 {
		t.Fatalf("an unmentioned member has activity: %s", kindsOf(items))
	}

	// You reply to it: me gets a reply. Third replies and mentions me: one
	// row (the mention), not two.
	if _, err := s.SendMessage(ctx, you, g.ID, "on it", m1.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, third, g.ID, "@Me me too", m1.ID, nil, []string{me.ID}); err != nil {
		t.Fatal(err)
	}
	if items, n := feedOf(t, s, me, "", false); kindsOf(items) != "mention:Third,reply:Other" || n != 2 {
		t.Fatalf("me after a reply and a mention-reply: n=%d %s", n, kindsOf(items))
	}
	if items, _ := feedOf(t, s, me, ActivityReply, false); kindsOf(items) != "reply:Other" {
		t.Fatalf("filter by kind: %s", kindsOf(items))
	}

	// An edit shows through.
	if _, err := s.Edit(ctx, me, m1.ID, "@Other can you check the release build?"); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, you, "", false); items[0].Snippet != "@Other can you check the release build?" {
		t.Fatalf("snippet after an edit: %q", items[0].Snippet)
	}

	// Reactions: on someone else's message, once however it is toggled; off
	// takes it back; on your own message, nothing.
	for _, on := range []bool{true, false, true, true} {
		if err := s.React(ctx, you, m1.ID, "👍", on); err != nil {
			t.Fatal(err)
		}
	}
	if items, _ := feedOf(t, s, me, ActivityReaction, false); len(items) != 1 || items[0].Emoji != "👍" || items[0].ActorID != you.ID {
		t.Fatalf("one reaction row: %+v", items)
	}
	if err := s.React(ctx, you, m1.ID, "👍", false); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, me, ActivityReaction, false); len(items) != 0 {
		t.Fatalf("a reaction taken back stays: %+v", items)
	}
	if err := s.React(ctx, me, m1.ID, "🎉", true); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, me, ActivityReaction, false); len(items) != 0 {
		t.Fatalf("reacting to your own message is activity: %+v", items)
	}

	// Deleting the message takes its rows.
	if err := s.Delete(ctx, me, m1.ID); err != nil {
		t.Fatal(err)
	}
	if items, n := feedOf(t, s, you, "", false); len(items) != 0 || n != 0 {
		t.Fatalf("a deleted message's activity stays: n=%d %s", n, kindsOf(items))
	}
}

// Reading: by id, all, and by reading the conversation (mentions and
// replies only). Leaving the conversation hides its rows and their count.
func TestActivityReadAndVisibility(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.SendMessage(ctx, you, g.ID, "first", "", nil, nil)
	_, _ = s.SendMessage(ctx, me, g.ID, "@Other one", "", nil, []string{you.ID})
	_, _ = s.SendMessage(ctx, me, g.ID, "@Other two", "", nil, []string{you.ID})
	if err := s.React(ctx, me, a.ID, "❤️", true); err != nil {
		t.Fatal(err)
	}
	items, n := feedOf(t, s, you, "", false)
	if n != 3 || len(items) != 3 {
		t.Fatalf("three unread: n=%d %s", n, kindsOf(items))
	}

	// One by id (a mention: the newest row is the reaction).
	var mention *Activity
	for _, x := range items {
		if x.Kind == ActivityMention {
			mention = x
		}
	}
	if n, err := s.ReadActivity(ctx, you, []string{mention.ID}, false); err != nil || n != 2 {
		t.Fatalf("read one: n=%d err=%v", n, err)
	}
	if un, _ := feedOf(t, s, you, "", true); len(un) != 2 {
		t.Fatalf("unread filter: %s", kindsOf(un))
	}
	// Someone else's ids do nothing.
	if n, _ := s.ReadActivity(ctx, me, []string{items[0].ID}, false); n != 0 {
		t.Fatalf("me reading your row: %d", n)
	}
	if _, n := feedOf(t, s, you, "", false); n != 2 {
		t.Fatalf("another person read my row: %d", n)
	}
	// Reading the conversation reads the mentions, not the reaction.
	if err := s.MarkRead(ctx, you, g.ID); err != nil {
		t.Fatal(err)
	}
	if un, n := feedOf(t, s, you, "", true); n != 1 || len(un) != 1 || un[0].Kind != ActivityReaction {
		t.Fatalf("after reading the conversation: n=%d %s", n, kindsOf(un))
	}
	if n, _ := s.ReadActivity(ctx, you, nil, true); n != 0 {
		t.Fatalf("read all: %d", n)
	}

	// Bounds and bad input.
	ids := make([]string, MaxActivityRead+1)
	for i := range ids {
		ids[i] = fmt.Sprint("x", i)
	}
	if _, err := s.ReadActivity(ctx, you, ids, false); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("%d ids: %v", len(ids), err)
	}
	if _, err := s.ReadActivity(ctx, you, ids[:MaxActivityRead], false); err != nil {
		t.Fatalf("%d ids: %v", MaxActivityRead, err)
	}
	for _, bad := range [][2]string{{"", "everything"}, {"abc:x", ""}, {"-5:x", ""}} {
		if _, _, err := s.Activities(ctx, you, bad[0], bad[1], false, 10); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("before=%q kind=%q: %v", bad[0], bad[1], err)
		}
	}

	// A new mention, then you leave: the feed and its count drop it.
	_, _ = s.SendMessage(ctx, me, g.ID, "@Other three", "", nil, []string{you.ID})
	if _, n := feedOf(t, s, you, "", false); n != 1 {
		t.Fatalf("before leaving: %d", n)
	}
	if err := s.Leave(ctx, you, g.ID); err != nil {
		t.Fatal(err)
	}
	if items, n := feedOf(t, s, you, "", false); len(items) != 0 || n != 0 {
		t.Fatalf("a conversation you left is still in your feed: n=%d %s", n, kindsOf(items))
	}
}

// Being added to a team is activity; joining yourself is not; deleting the
// team takes it.
func TestActivityTeamAdds(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	tm, err := s.CreateTeam(ctx, me, "Design", "", TeamPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTeamMembers(ctx, me, tm.ID, []string{you.Email}); err != nil {
		t.Fatal(err)
	}
	// Added again: nothing new happened to them.
	if _, err := s.AddTeamMembers(ctx, me, tm.ID, []string{you.Email}); err != nil {
		t.Fatal(err)
	}
	items, n := feedOf(t, s, you, "", false)
	if len(items) != 1 || n != 1 || items[0].Kind != ActivityTeam || items[0].TeamID != tm.ID || items[0].TeamName != "Design" {
		t.Fatalf("added to a team: n=%d %+v", n, items)
	}
	pub, _ := s.CreateTeam(ctx, me, "Open", "", TeamPublic)
	if _, err := s.JoinTeam(ctx, third, pub.ID); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, third, "", false); len(items) != 0 {
		t.Fatalf("joining yourself is activity: %s", kindsOf(items))
	}
	// A mention in a team channel names the team.
	gen := teamChannel(t, myTeam(t, s, me, tm.ID), GeneralChannel)
	if _, err := s.SendMessage(ctx, me, gen.ID, "@Other welcome", "", nil, []string{you.ID}); err != nil {
		t.Fatal(err)
	}
	if items, _ := feedOf(t, s, you, ActivityMention, false); len(items) != 1 || items[0].TeamName != "Design" || items[0].ConvTitle != GeneralChannel {
		t.Fatalf("a team channel mention: %+v", items)
	}
	if err := s.DeleteTeam(ctx, me, tm.ID); err != nil {
		t.Fatal(err)
	}
	if items, n := feedOf(t, s, you, "", false); len(items) != 0 || n != 0 {
		t.Fatalf("a deleted team's activity stays: n=%d %s", n, kindsOf(items))
	}
}

// The feed is capped, pages with a total order (rows written in the same
// millisecond are neither lost nor repeated), and every query is
// index-backed.
func TestActivityCapPagingAndPlans(t *testing.T) {
	s, me, you, _, _ := channelService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	tm, _ := s.CreateTeam(ctx, me, "Big", "", TeamPrivate)

	at := time.UnixMilli(1_800_000_000_000).UTC()
	var rows []activityRow
	for i := range MaxActivity + 1 {
		// Three to a millisecond, so pages break inside a tie.
		rows = append(rows, activityRow{id: fmt.Sprintf("r%04d", i), userID: you.ID, kind: ActivityTeam, actorID: me.ID, actorName: "Me", teamID: tm.ID, at: at.Add(time.Duration(i/3) * time.Millisecond)})
	}
	if err := repo.AddActivity(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, n := feedOf(t, s, you, "", false); n != MaxActivity {
		t.Fatalf("cap: %d rows kept, want %d", n, MaxActivity)
	}
	// The oldest went.
	seen := map[string]bool{}
	before := ""
	for page := 0; page < 100; page++ {
		items, _, err := s.Activities(ctx, you, before, "", false, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		for _, x := range items {
			if seen[x.ID] {
				t.Fatalf("row %s listed twice", x.ID)
			}
			seen[x.ID] = true
		}
		last := items[len(items)-1]
		before = fmt.Sprintf("%d:%s", last.At.UnixMilli(), last.ID)
	}
	if len(seen) != MaxActivity || seen["r0000"] || !seen["r0500"] {
		t.Fatalf("paging saw %d rows (oldest kept: %v, newest: %v)", len(seen), seen["r0000"], seen["r0500"])
	}

	for _, q := range []string{
		activitySelect + ` WHERE a.user_id = 'u' AND ` + activityVisible + ` AND (a.created_at < 5 OR (a.created_at = 5 AND a.id < 'x')) ORDER BY a.created_at DESC, a.id DESC LIMIT 50`,
		`DELETE FROM messaging_activity WHERE message_id = 'm'`,
		`DELETE FROM messaging_activity WHERE team_id = 't'`,
		`SELECT COUNT(*) FROM messaging_activity a LEFT JOIN messaging_messages m ON m.id = a.message_id WHERE a.user_id = 'u' AND a.read = 0 AND ` + activityVisible,
	} {
		rows, err := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			plan += d + "; "
		}
		rows.Close()
		if strings.Contains(plan, "SCAN a") || strings.Contains(plan, "SCAN messaging_activity") || strings.Contains(plan, "SCAN m ") {
			t.Fatalf("not index-backed: %s\n-> %s", q, plan)
		}
	}
}

// The pushed item carries everything the feed draws, and the read and gone
// frames carry the ids and the count: read off a real socket.
func TestActivityPushFrames(t *testing.T) {
	noLeaks(t)
	h := NewHub(quietLog())
	sk, cli, rx := newSock(t, h, "viewer", 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sk.writePump(ctx)

	it := &Activity{ID: "act-1", Kind: ActivityReply, ActorID: "u-a", ActorName: "Ada", ConvID: "c-1", ConvKind: "channel",
		ConvTitle: "Reviews", TeamID: "t-1", TeamName: "Design", MessageID: "m-2", RootID: "m-1", Emoji: "",
		Snippet: "looks good to me", At: time.UnixMilli(1_800_000_000_123).UTC()}
	h.PushActivity("viewer", it, 4)
	h.PushActivityRead("viewer", []string{"act-1", "act-0"}, 2)
	h.PushActivityGone("viewer", "act-9", 1)

	in := &frames{cli: cli}
	read := func() *wire.Reader {
		rc, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		data, err := in.next(rc)
		if err != nil {
			t.Fatal(err)
		}
		r := wire.NewReader(data, rx)
		if hdr, err := r.ReadHeader(); err != nil || hdr.Op != wire.OpActivity {
			t.Fatalf("header: %v %v", hdr.Op, err)
		}
		return r
	}

	r := read()
	if k := r.Bits(2); k != actItem {
		t.Fatalf("kind %d", k)
	}
	r.Bits(6)
	var got []string
	for range 12 {
		got = append(got, r.Str())
	}
	want := []string{"act-1", "reply", "u-a", "Ada", "c-1", "channel", "Reviews", "t-1", "Design", "m-2", "m-1", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("item fields:\n got %q\nwant %q", got, want)
	}
	if at := r.Uvarint(); at != 1_800_000_000_123 {
		t.Fatalf("time %d", at)
	}
	if r.Bool() {
		t.Fatal("read")
	}
	r.Bits(7)
	if s := string(r.Blob()); s != "looks good to me" {
		t.Fatalf("snippet %q", s)
	}
	if n := r.Uvarint(); n != 4 || r.Err() != nil {
		t.Fatalf("unread %d err %v", n, r.Err())
	}

	r = read()
	if k := r.Bits(2); k != actRead {
		t.Fatalf("kind %d", k)
	}
	r.Bits(6)
	if c := r.Uvarint(); c != 2 {
		t.Fatalf("count %d", c)
	}
	if a, b := r.Str(), r.Str(); a != "act-1" || b != "act-0" {
		t.Fatalf("ids %q %q", a, b)
	}
	if n := r.Uvarint(); n != 2 {
		t.Fatalf("unread %d", n)
	}

	r = read()
	if k := r.Bits(2); k != actGone {
		t.Fatalf("kind %d", k)
	}
	r.Bits(6)
	if id, n := r.Str(), r.Uvarint(); id != "act-9" || n != 1 || r.Err() != nil {
		t.Fatalf("gone %q %d %v", id, n, r.Err())
	}
}
