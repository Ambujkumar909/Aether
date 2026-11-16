package messaging

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// Settings are a closed set per kind, read back as stored, and one
// person's are not another's.
func TestNotificationSettingsAreValidatedAndMine(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	def := DefaultNotifyPrefs()
	if got, err := s.NotifyPrefsOf(ctx, me); err != nil || got != def {
		t.Fatalf("with nothing chosen: %+v err=%v", got, err)
	}
	if def.Direct != NotifyAlert || def.Group != NotifyAlert || def.Mention != NotifyAlert || def.Reply != NotifyAlert || def.Reaction != NotifyQuiet || !def.Preview {
		t.Fatalf("the defaults are what Aether did before settings: %+v", def)
	}
	bad := map[string]func(p *NotifyPrefs){
		"a direct chat cannot be off":     func(p *NotifyPrefs) { p.Direct = NotifyOff },
		"a group cannot be off":           func(p *NotifyPrefs) { p.Group = NotifyOff },
		"a mention cannot be off":         func(p *NotifyPrefs) { p.Mention = NotifyOff },
		"a reaction cannot alert":         func(p *NotifyPrefs) { p.Reaction = NotifyAlert },
		"a level nobody defined":          func(p *NotifyPrefs) { p.Reply = "loud" },
		"a field left out is not a level": func(p *NotifyPrefs) { p.Group = "" },
	}
	for name, spoil := range bad {
		p := def
		spoil(&p)
		if _, err := s.SetNotifyPrefs(ctx, me, p); !errors.Is(err, ErrBadNotify) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	if got, _ := s.NotifyPrefsOf(ctx, me); got != def {
		t.Fatalf("a refused setting was stored: %+v", got)
	}
	want := NotifyPrefs{Direct: NotifyQuiet, Group: NotifyQuiet, Mention: NotifyQuiet, Reply: NotifyOff, Reaction: NotifyOff, Preview: false, Email: "off"}
	if got, err := s.SetNotifyPrefs(ctx, me, want); err != nil || got != want {
		t.Fatalf("every lowest level at once: %+v err=%v", got, err)
	}
	if got, _ := s.NotifyPrefsOf(ctx, me); got != want {
		t.Fatalf("read back: %+v", got)
	}
	if got, _ := s.NotifyPrefsOf(ctx, you); got != def {
		t.Fatalf("someone else's changed with mine: %+v", got)
	}
	// Changing again replaces, it does not add a row.
	if _, err := s.SetNotifyPrefs(ctx, me, def); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = s.repo.(*SQLRepository).db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_notify_prefs`).Scan(&rows)
	if got, _ := s.NotifyPrefsOf(ctx, me); got != def || rows != 1 {
		t.Fatalf("after setting the defaults back: %+v, %d row(s)", got, rows)
	}
}

// The push to a closed Aether follows the settings of the person it is
// for: a kind set quiet sends nothing, a mention outranks where it was
// written, a reply is addressed to its author, urgent is not a setting,
// and with preview off the words do not leave the server.
func TestNotificationSettingsDecideThePush(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	fn := &fakeNotifier{}
	s.UseNotifier(fn)
	set := func(who Actor, change func(p *NotifyPrefs)) {
		t.Helper()
		p := DefaultNotifyPrefs()
		change(&p)
		if _, err := s.SetNotifyPrefs(ctx, who, p); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	fn.take()
	send := func(what string, f func() error) []string {
		t.Helper()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return fn.take()
	}
	direct := func(body string) func() error {
		return func() error { _, err := s.Send(ctx, me, dm.ID, body); return err }
	}

	// Direct chats quiet: nothing. Somebody else's setting is not mine.
	set(you, func(p *NotifyPrefs) { p.Direct = NotifyQuiet })
	if got := send("direct, quiet", direct("hello")); len(got) != 0 {
		t.Fatalf("direct chats set quiet still pushed: %v", got)
	}
	set(third, func(p *NotifyPrefs) { p.Direct = NotifyQuiet })
	set(you, func(p *NotifyPrefs) {})
	if got := send("direct, default", direct("hello again")); len(got) != 1 || !strings.HasPrefix(got[0], you.ID+"|Me|hello again|") {
		t.Fatalf("back at the default, with someone else's set quiet: %v", got)
	}

	// A mention outranks the conversation it is written in...
	set(you, func(p *NotifyPrefs) { p.Direct = NotifyQuiet })
	if got := send("a mention in a quiet direct chat", func() error {
		_, err := s.SendMessage(ctx, me, dm.ID, "@Other look", "", nil, []string{you.ID})
		return err
	}); len(got) != 1 {
		t.Fatalf("a mention in a direct chat set quiet: %v", got)
	}
	// ...and is its own setting.
	set(you, func(p *NotifyPrefs) { p.Mention = NotifyQuiet })
	if got := send("a mention, mentions quiet", func() error {
		_, err := s.SendMessage(ctx, me, g.ID, "@Other look", "", nil, []string{you.ID})
		return err
	}); len(got) != 0 {
		t.Fatalf("mentions set quiet still pushed: %v", got)
	}

	// A reply in a group is addressed to the author of what it answers,
	// and to nobody else in the group.
	set(you, func(p *NotifyPrefs) {})
	mine, err := s.Send(ctx, you, g.ID, "draft is up")
	if err != nil {
		t.Fatal(err)
	}
	fn.take()
	reply := func() error { _, err := s.SendMessage(ctx, me, g.ID, "looks right", mine.ID, nil, nil); return err }
	if got := send("a reply", reply); len(got) != 1 || !strings.HasPrefix(got[0], you.ID+"|Me in Launch|looks right|") {
		t.Fatalf("a reply to an absent author: %v", got)
	}
	for _, lvl := range []string{NotifyQuiet, NotifyOff} {
		set(you, func(p *NotifyPrefs) { p.Reply = lvl })
		if got := send("a reply, replies "+lvl, reply); len(got) != 0 {
			t.Fatalf("replies set %s still pushed: %v", lvl, got)
		}
	}

	// Urgent is not a setting: everything quiet, and it still arrives.
	set(you, func(p *NotifyPrefs) {
		p.Direct, p.Group, p.Mention, p.Reply = NotifyQuiet, NotifyQuiet, NotifyQuiet, NotifyOff
	})
	got := send("urgent", func() error {
		_, err := s.postWith(ctx, me, dm.ID, "server is down", "", false, nil, nil, SendOptions{Priority: "urgent"})
		return err
	})
	if len(got) != 1 || !strings.Contains(got[0], "|URGENT: server is down|") || !strings.HasSuffix(got[0], "|urgent") {
		t.Fatalf("an urgent message with everything quiet: %v", got)
	}

	// Preview off: who and where, never what.
	set(you, func(p *NotifyPrefs) { p.Preview = false })
	if got := send("preview off", direct("the password is hunter2")); len(got) != 1 || got[0] != you.ID+"|Me|New message|/?chat="+dm.ID+"|conv-"+dm.ID+"|normal" {
		t.Fatalf("preview off: %v", got)
	}
	got = send("preview off, urgent", func() error {
		_, err := s.postWith(ctx, me, dm.ID, "the door code is 4411", "", false, nil, nil, SendOptions{Priority: "urgent"})
		return err
	})
	if len(got) != 1 || !strings.Contains(got[0], "|URGENT: New message|") || strings.Contains(got[0], "4411") {
		t.Fatalf("preview off, urgent: %v", got)
	}
}

// The feed: off means the row is never written; quiet still writes it; a
// mention is always there.
func TestNotificationSettingsDecideTheFeed(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := s.Send(ctx, you, g.ID, "draft is up")
	if err != nil {
		t.Fatal(err)
	}
	kinds := func() string {
		t.Helper()
		items, _, err := s.Activities(ctx, you, "", "", false, 50)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Kind)
		}
		sort.Strings(out) // two rows written in one millisecond have no order worth asserting
		return strings.Join(out, ",")
	}
	act := func() {
		t.Helper()
		if err := s.React(ctx, me, mine.ID, "👍", true); err != nil {
			t.Fatal(err)
		}
		if err := s.React(ctx, me, mine.ID, "👍", false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SendMessage(ctx, me, g.ID, "looks right", mine.ID, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SendMessage(ctx, third, g.ID, "@Other see above", "", nil, []string{you.ID}); err != nil {
			t.Fatal(err)
		}
	}
	clear := func() {
		t.Helper()
		if _, err := s.repo.(*SQLRepository).db.ExecContext(ctx, `DELETE FROM messaging_activity`); err != nil {
			t.Fatal(err)
		}
	}

	// A reaction taken back takes its row with it, so the default leaves a
	// reply and a mention; that is the baseline the settings subtract from.
	act()
	base := kinds()
	if !strings.Contains(base, ActivityReply) || !strings.Contains(base, ActivityMention) {
		t.Fatalf("with nothing chosen the feed has the reply and the mention: %q", base)
	}
	clear()

	// Quiet writes the row all the same.
	if _, err := s.SetNotifyPrefs(ctx, you, NotifyPrefs{Direct: NotifyQuiet, Group: NotifyQuiet, Mention: NotifyQuiet, Reply: NotifyQuiet, Reaction: NotifyQuiet, Preview: true, Email: "off"}); err != nil {
		t.Fatal(err)
	}
	act()
	if got := kinds(); got != base {
		t.Fatalf("everything quiet: feed %q, want %q", got, base)
	}
	clear()

	// Off: no reply row, no reaction row (checked while the reaction is
	// still on the message); the mention stays.
	p := DefaultNotifyPrefs()
	p.Reply, p.Reaction = NotifyOff, NotifyOff
	if _, err := s.SetNotifyPrefs(ctx, you, p); err != nil {
		t.Fatal(err)
	}
	if err := s.React(ctx, me, mine.ID, "🎉", true); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); got != "" {
		t.Fatalf("reactions off, a reaction on my message: feed %q", got)
	}
	act()
	if got := kinds(); got != ActivityMention {
		t.Fatalf("replies and reactions off: feed %q, want only the mention", got)
	}
	clear()

	// On again: the reaction is back in the feed.
	p.Reaction = NotifyQuiet
	if _, err := s.SetNotifyPrefs(ctx, you, p); err != nil {
		t.Fatal(err)
	}
	if err := s.React(ctx, third, mine.ID, "👀", true); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); got != ActivityReaction {
		t.Fatalf("reactions back to quiet: feed %q", got)
	}
}
