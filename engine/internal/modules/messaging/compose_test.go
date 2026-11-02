package messaging

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// Composition and search. What these hold: priorities are the two words and
// nothing else; a scheduled message is the author's alone until its time,
// then everybody's at once -- counted once, pushed once, placed at delivery
// time; deleting it before then is the end of it; meta is a bounded JSON
// object carried untouched; search finds what you can read and nothing you
// cannot, as an index lookup.

func TestPriorityAndMetaAreValidatedAndCarried(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{Priority: "loud"}); !errors.Is(err, ErrBadPriority) {
		t.Fatalf("bad priority: err=%v", err)
	}
	if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{Meta: "[1,2]"}); !errors.Is(err, ErrBadMeta) {
		t.Fatalf("meta that is not an object: err=%v", err)
	}
	if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{Meta: `{"a":"` + strings.Repeat("x", MaxMetaBytes) + `"}`}); !errors.Is(err, ErrBadMeta) {
		t.Fatalf("oversized meta: err=%v", err)
	}
	m, err := s.Compose(ctx, me, conv, "ship it", "", nil, nil, SendOptions{Priority: PriorityUrgent, Meta: `{"praise":{"badge":"Thank you"}}`})
	if err != nil {
		t.Fatal(err)
	}
	got := bodiesFull(t, s, you, conv)
	if len(got) != 1 || got[0].Priority != PriorityUrgent || got[0].Meta != `{"praise":{"badge":"Thank you"}}` || got[0].ID != m.ID {
		t.Fatalf("read back: %+v", got)
	}
}

func bodiesFull(t *testing.T, s *Service, a Actor, conv string) []*Message {
	t.Helper()
	ms, err := s.Messages(context.Background(), a, conv, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	s.Decorate(context.Background(), a, ms)
	return ms
}

func TestAScheduledMessageIsTheAuthorsAloneUntilItsTime(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)

	soon := time.Now().UTC().Add(MinScheduleLead + time.Second)
	if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{DeliverAt: ptr(time.Now().Add(5 * time.Second))}); !errors.Is(err, ErrBadSchedule) {
		t.Fatalf("too soon: err=%v", err)
	}
	if _, err := s.Compose(ctx, me, conv, "x", "", nil, nil, SendOptions{DeliverAt: ptr(time.Now().Add(31 * 24 * time.Hour))}); !errors.Is(err, ErrBadSchedule) {
		t.Fatalf("too far: err=%v", err)
	}
	m, err := s.Compose(ctx, me, conv, "good morning", "", nil, []string{you.ID}, SendOptions{DeliverAt: &soon})
	if err != nil {
		t.Fatal(err)
	}
	if m.DeliverAt == nil {
		t.Fatal("no deliverAt on the returned message")
	}
	// The author sees it, marked; the other person does not; nothing is counted.
	mine := bodiesFull(t, s, me, conv)
	if len(mine) != 1 || mine[0].DeliverAt == nil {
		t.Fatalf("author's view: %+v", mine)
	}
	if theirs := bodiesFull(t, s, you, conv); len(theirs) != 0 {
		t.Fatalf("the recipient sees a scheduled message early: %+v", theirs)
	}
	if list, _ := s.List(ctx, you); list[0].Unread != 0 || list[0].Mentions != 0 || list[0].Preview != "" {
		t.Fatalf("counted or previewed before delivery: unread %d mentions %d preview %q", list[0].Unread, list[0].Mentions, list[0].Preview)
	}
	if n := s.DeliverDueNow(ctx); n != 0 {
		t.Fatalf("delivered %d before its time", n)
	}

	// Time comes (the row's due time is pulled back to just after it was
	// written -- still "pending" by the row's own terms, and now in the past
	// -- rather than the test waiting thirty seconds).
	if _, err := repo.db.ExecContext(ctx, `UPDATE messaging_messages SET deliver_at = created_at + 1 WHERE id = ? AND ? > 0`,
		m.ID, 1); err != nil {
		t.Fatal(err)
	}
	// created_at + 1 ms is due only once that millisecond has passed; on a
	// fast run the clock has not moved yet.
	time.Sleep(3 * time.Millisecond)
	deliveredAt := time.Now().UTC()
	if n := s.DeliverDueNow(ctx); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	if n := s.DeliverDueNow(ctx); n != 0 {
		t.Fatalf("delivered again: %d", n)
	}
	theirs := bodiesFull(t, s, you, conv)
	if len(theirs) != 1 || theirs[0].Body != "good morning" || theirs[0].Mentions[0] != you.ID {
		t.Fatalf("after delivery: %+v", theirs)
	}
	if theirs[0].CreatedAt.Before(deliveredAt.Add(-time.Second)) {
		t.Fatalf("a delivered message keeps its composition time (%s); it should be stamped at delivery (%s)",
			theirs[0].CreatedAt, deliveredAt)
	}
	list, _ := s.List(ctx, you)
	if list[0].Unread != 1 || list[0].Mentions != 1 || list[0].Preview != "good morning" {
		t.Fatalf("after delivery: unread %d mentions %d preview %q", list[0].Unread, list[0].Mentions, list[0].Preview)
	}
	truth, _ := repo.UnreadCount(ctx, conv, you.ID)
	if truth != 1 {
		t.Fatalf("truth after delivery: %d", truth)
	}

	// A scheduled message deleted before its time never arrives.
	later := time.Now().UTC().Add(time.Hour)
	m2, _ := s.Compose(ctx, me, conv, "never", "", nil, nil, SendOptions{DeliverAt: &later})
	if err := s.Delete(ctx, me, m2.ID); err != nil {
		t.Fatal(err)
	}
	repo.db.ExecContext(ctx, `UPDATE messaging_messages SET deliver_at = created_at + 1 WHERE id = ? AND ? > 0`, m2.ID, 1)
	if n := s.DeliverDueNow(ctx); n != 0 {
		t.Fatalf("a deleted scheduled message was delivered")
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestSearchFindsWhatYouCanReadAndNothingElse(t *testing.T) {
	s, me, you, conv := twoPeople(t)
	ctx := context.Background()
	s.Send(ctx, you, conv, "The invoice for the Sharma account is attached")
	s.Send(ctx, me, conv, "invoices go to finance")
	hidden, _ := s.Send(ctx, you, conv, "another INVOICE you will hide")
	s.Hide(ctx, me, hidden.ID)
	gone, _ := s.Send(ctx, you, conv, "invoice deleted for everyone")
	s.Delete(ctx, you, gone.ID)
	// Somebody else's conversation with the same word.
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	own, _ := s.OpenSelf(ctx, stranger)
	s.Send(ctx, stranger, own.ID, "my private invoice")

	got, err := s.SearchMessages(ctx, me, "invoice", 20)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, m := range got {
		bodies = append(bodies, m.Body)
	}
	sort.Strings(bodies)
	want := []string{"The invoice for the Sharma account is attached", "invoices go to finance"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("search: %v, want %v (mine and theirs in my chats; not hidden, not deleted, not a stranger's)", bodies, want)
	}
	// Substring, case-insensitive, and too short returns nothing.
	if got, _ := s.SearchMessages(ctx, me, "HARM", 20); len(got) != 1 {
		t.Fatalf("mid-word substring: %d results", len(got))
	}
	if got, _ := s.SearchMessages(ctx, me, "in", 20); got != nil {
		t.Fatalf("two characters should search nothing, got %d", len(got))
	}
	for _, q := range []string{`inv" OR "priv`, `NOT invoice`, `invoice*`, "{body}: inv"} {
		if _, err := s.SearchMessages(ctx, me, q, 20); err != nil {
			t.Fatalf("%q: a typed query must never error: %v", q, err)
		}
	}
	// A rename of the text follows into the index.
	edited, _ := s.Send(ctx, me, conv, "draft wording")
	s.Edit(ctx, me, edited.ID, "final wording")
	if got, _ := s.SearchMessages(ctx, me, "draft", 20); len(got) != 0 {
		t.Fatal("the old text is still indexed after an edit")
	}
	if got, _ := s.SearchMessages(ctx, me, "final wording", 20); len(got) != 1 {
		t.Fatal("the new text is not indexed after an edit")
	}
}

// The same examples as web/src/format.test.mjs: the two plainText functions
// must agree, or a message found in one place is missed in the other.
func TestPlainTextMirrorsTheClient(t *testing.T) {
	for in, want := range map[string]string{
		"ship **today** with `go build`": "ship today with go build",
		"```js\nlet x\n```\ndone":        "let x done",
		"2*3*4":                          "2*3*4",
		"~~old~~ *new*":                  "old new",
		"  spaced   out  ":               "spaced out",
	} {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchSeesThroughFormatting(t *testing.T) {
	s, me, _, conv := twoPeople(t)
	ctx := context.Background()
	m, _ := s.Send(ctx, me, conv, "ship **today** with `go build`")
	got, _ := s.SearchMessages(ctx, me, "ship tod", 10)
	if len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("searching the rendered words: %d results", len(got))
	}
	if got, _ := s.SearchMessages(ctx, me, "**tod", 10); len(got) != 0 {
		t.Fatal("the marks themselves are not indexed")
	}
	// Editing re-indexes the plain text; deleting drops it.
	s.Edit(ctx, me, m.ID, "ship *tomorrow*")
	if got, _ := s.SearchMessages(ctx, me, "ship tomorrow", 10); len(got) != 1 {
		t.Fatal("edited text not found")
	}
	s.Delete(ctx, me, m.ID)
	if got, _ := s.SearchMessages(ctx, me, "ship tomorrow", 10); len(got) != 0 {
		t.Fatal("deleted text still found")
	}
	// A rebuild from an emptied index restores it.
	repo := s.repo.(*SQLRepository)
	s.Send(ctx, me, conv, "**kept** message")
	repo.db.ExecContext(ctx, `DELETE FROM messaging_messages_fts`)
	if n, err := repo.ReindexIfNeeded(ctx); err != nil || n != 1 {
		t.Fatalf("reindex: n=%d err=%v", n, err)
	}
	if got, _ := s.SearchMessages(ctx, me, "kept mess", 10); len(got) != 1 {
		t.Fatal("not found after reindex")
	}
	if n, _ := repo.ReindexIfNeeded(ctx); n != 0 {
		t.Fatal("reindex ran again on a filled index")
	}
}

func TestSearchAtScaleIsAnIndexLookup(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("timings only")
	}
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, _ := s.OpenDirect(ctx, me, you.Email)
	tx, _ := repo.db.BeginTx(ctx, nil)
	ins, _ := tx.PrepareContext(ctx, `INSERT INTO messaging_messages (`+msgCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	words := []string{"invoice", "meeting", "deploy", "coffee", "budget", "design", "review", "launch"}
	now := time.Now().UTC().Add(-100 * time.Hour)
	for i := 0; i < 200_000; i++ {
		body := fmt.Sprintf("%s number %d about the %s", words[i%8], i, words[(i/8)%8])
		if _, err := ins.ExecContext(ctx, fmt.Sprintf("m%06d", i), c.ID, me.ID, "Me", body,
			now.Add(time.Duration(i)*time.Second).UnixMilli(), nil, 0, "", 0, 0, "", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	measure := func(label, q string) {
		var ds []time.Duration
		for i := 0; i < 20; i++ {
			t0 := time.Now()
			if _, err := s.SearchMessages(ctx, me, q, 30); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		p95 := ds[len(ds)*95/100]
		t.Logf("%-32s p50 %8s p95 %8s", label, ds[len(ds)/2], p95)
		if p95 > 25*time.Millisecond {
			t.Errorf("%s: p95 %s over 200k messages", label, p95)
		}
	}
	measure("common word (25k matches)", "invoice")
	measure("rare number", "number 199999")
	measure("no match", "zebra crossing")
}
