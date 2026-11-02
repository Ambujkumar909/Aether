package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A channel reads as roots with folded replies; a reply to a reply joins the
// root; a chat is untouched by any of it.
func TestChannelThreadsFoldRepliesUnderTheirRoot(t *testing.T) {
	s, me, you, direct := twoPeople(t)
	ctx := context.Background()
	ch, err := s.CreateChannel(ctx, me, "threads", "", VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinChannel(ctx, you, ch.ID); err != nil {
		t.Fatal(err)
	}
	root, _ := s.Send(ctx, me, ch.ID, "Release plan")
	other, _ := s.Send(ctx, you, ch.ID, "Unrelated post")
	r1, _ := s.SendReply(ctx, you, ch.ID, "Looks good", root.ID)
	r2, _ := s.SendReply(ctx, me, ch.ID, "Thanks", r1.ID) // a reply to a reply
	if r2.ReplyTo != root.ID {
		t.Fatalf("a reply to a reply should hang off the root, got %q", r2.ReplyTo)
	}
	feed, _ := s.Messages(ctx, you, ch.ID, time.Time{}, 50)
	var ids []string
	for _, m := range feed {
		if !m.System {
			ids = append(ids, m.ID)
		}
	}
	if strings.Join(ids, ",") != root.ID+","+other.ID {
		t.Fatalf("channel feed should be the roots only: %v", ids)
	}
	var rootRow *Message
	for _, m := range feed {
		if m.ID == root.ID {
			rootRow = m
		}
	}
	if rootRow.Thread == nil || rootRow.Thread.Count != 2 || rootRow.Thread.LastAt == nil {
		t.Fatalf("root summary: %+v", rootRow.Thread)
	}
	if strings.Join(rootRow.Thread.LastNames, ",") != "Me,Other" {
		t.Fatalf("most recent repliers first: %v", rootRow.Thread.LastNames)
	}
	for _, m := range feed {
		if m.ID == other.ID && m.Thread != nil {
			t.Fatalf("a post with no replies has no summary: %+v", m.Thread)
		}
	}
	replies, err := s.Replies(ctx, you, root.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 || replies[0].ID != r1.ID || replies[1].ID != r2.ID {
		t.Fatalf("thread, oldest first: %+v", replies)
	}
	if replies[0].Quoted == nil {
		t.Fatal("replies are decorated like any page")
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, err := s.Replies(ctx, stranger, root.ID, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read a thread: %v", err)
	}
	// A direct chat still reads as one line, replies included.
	d1, _ := s.Send(ctx, me, direct, "q")
	s.SendReply(ctx, you, direct, "a", d1.ID)
	line, _ := s.Messages(ctx, me, direct, time.Time{}, 50)
	if len(line) != 2 || line[1].ReplyTo != d1.ID {
		t.Fatalf("a chat is one line: %+v", line)
	}
	if line[0].Thread != nil {
		t.Fatal("chats carry no thread summaries")
	}
}

// Edits keep their history, oldest first, readable by members and nobody
// else; a deleted message has none to show.
func TestEditHistoryIsKeptAndMembersOnly(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, conv, "first")
	if h, _ := s.History(ctx, you, m.ID); len(h) != 0 {
		t.Fatalf("unedited message has history: %+v", h)
	}
	s.Edit(ctx, me, m.ID, "second")
	time.Sleep(2 * time.Millisecond)
	s.Edit(ctx, me, m.ID, "third")
	h, err := s.History(ctx, you, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].Body != "first" || h[1].Body != "second" {
		t.Fatalf("history: %+v", h)
	}
	if !h[1].EditedAt.After(h[0].EditedAt) {
		t.Fatal("history out of order")
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, err := s.History(ctx, stranger, m.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read history: %v", err)
	}
	s.Delete(ctx, me, m.ID)
	if h, _ := s.History(ctx, you, m.ID); len(h) != 0 {
		t.Fatalf("a deleted message still shows its history: %+v", h)
	}
}

func TestAnnouncementsAreBounded(t *testing.T) {
	s, me, _, conv := twoPeople(t)
	ctx := context.Background()
	for _, meta := range []string{
		`{"announcement":{"headline":""}}`,
		`{"announcement":{"headline":"` + strings.Repeat("h", 81) + `"}}`,
		`{"announcement":{"headline":"ok","sub":"` + strings.Repeat("s", 201) + `"}}`,
		`{"announcement":{"headline":"ok","color":"chartreuse"}}`,
	} {
		if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{Meta: meta}); !errors.Is(err, ErrBadAnnouncement) {
			t.Fatalf("%s: %v", meta, err)
		}
	}
	if _, err := s.Compose(ctx, me, conv, "All hands Friday", "", nil, nil, SendOptions{Meta: `{"announcement":{"headline":"All hands Friday","sub":"3pm, main room","color":"amber"}}`}); err != nil {
		t.Fatal(err)
	}
}
