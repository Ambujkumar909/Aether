package messaging

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// Who is told by push: a member with no socket open, for a direct message
// or a mention or an urgent message, never the sender, never someone
// connected (the socket tells them), never for a muted conversation.

type fakeNotifier struct {
	mu    sync.Mutex
	calls []string // "user|title|body|url|tag|urgent"
}

func (f *fakeNotifier) Notify(userID, title, body, url, tag string, urgent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := "normal"
	if urgent {
		u = "urgent"
	}
	f.calls = append(f.calls, strings.Join([]string{userID, title, body, url, tag, u}, "|"))
}

func (f *fakeNotifier) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func TestAbsentMembersAreNotifiedAndOnlyThem(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	fn := &fakeNotifier{}
	s.UseNotifier(fn)

	// A direct message to someone with no socket: one notice, to them,
	// naming the sender, opening the chat.
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, me, dm.ID, "are you there?"); err != nil {
		t.Fatal(err)
	}
	got := fn.take()
	if len(got) != 1 || got[0] != you.ID+"|Me|are you there?|/?chat="+dm.ID+"|conv-"+dm.ID+"|normal" {
		t.Fatalf("direct message notices: %v", got)
	}

	// The recipient connected: the socket carries it, no notice.
	sock, _, _ := newSock(t, s.hub, you.ID, 8)
	if _, err := s.Send(ctx, me, dm.ID, "still there?"); err != nil {
		t.Fatal(err)
	}
	if got := fn.take(); len(got) != 0 {
		t.Fatalf("a connected member was notified: %v", got)
	}
	s.hub.remove(sock)

	// Muted: nothing, however direct.
	muted := true
	if err := s.repo.SetPrefs(ctx, dm.ID, you.ID, nil, &muted, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, me, dm.ID, "psst"); err != nil {
		t.Fatal(err)
	}
	if got := fn.take(); len(got) != 0 {
		t.Fatalf("a muted conversation produced a notice: %v", got)
	}

	// A group: a plain message tells nobody; an urgent one tells everyone
	// absent; a mention tells the one named. The sender is never told.
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, me, g.ID, "morning all"); err != nil {
		t.Fatal(err)
	}
	if got := fn.take(); len(got) != 0 {
		t.Fatalf("a plain group message produced notices: %v", got)
	}
	if _, err := s.postWith(ctx, me, g.ID, "server is down", "", false, nil, nil, SendOptions{Priority: "urgent"}); err != nil {
		t.Fatal(err)
	}
	got = fn.take()
	if len(got) != 2 {
		t.Fatalf("urgent group message: %v", got)
	}
	for _, c := range got {
		if !strings.HasPrefix(c, you.ID+"|") && !strings.HasPrefix(c, third.ID+"|") {
			t.Fatalf("notice to the wrong person: %s", c)
		}
		if !strings.Contains(c, "|Me in Launch|URGENT: server is down|") || !strings.HasSuffix(c, "|urgent") {
			t.Fatalf("urgent notice words: %s", c)
		}
	}
	if _, err := s.SendMessage(ctx, me, g.ID, "@Third can you look?", "", nil, []string{third.ID}); err != nil {
		t.Fatal(err)
	}
	got = fn.take()
	if len(got) != 1 || !strings.HasPrefix(got[0], third.ID+"|") || strings.HasSuffix(got[0], "|urgent") {
		t.Fatalf("mention notices: %v", got)
	}
}
