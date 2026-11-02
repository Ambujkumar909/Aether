package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type pushed struct {
	room       string
	body       map[string]any
	restricted bool
	author     string
}

// qnaFixture: a room "r" with host "u-host", attendees "u-a" and "u-b"; "u-x"
// is not in the meeting. Pushes are recorded.
func qnaFixture(t *testing.T) (*Service, *[]pushed) {
	t.Helper()
	s := attendanceService(t)
	people := map[string]struct {
		name string
		host bool
	}{"u-host": {"Hana", true}, "u-a": {"Asha", false}, "u-b": {"Ben", false}}
	s.LiveMember = func(room, user string) (string, bool, bool) {
		p, ok := people[user]
		return p.name, p.host, ok && room == "r"
	}
	var mu sync.Mutex
	var log []pushed
	s.QnAPush = func(room string, body []byte, restricted bool, author string) {
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		log = append(log, pushed{room, m, restricted, author})
		mu.Unlock()
	}
	return s, &log
}

func ids(qs []*Question) []string {
	out := []string{}
	for _, q := range qs {
		out = append(out, q.Body)
	}
	return out
}

func TestQnAOnlyForPeopleInTheMeeting(t *testing.T) {
	s, _ := qnaFixture(t)
	ctx := context.Background()
	x := Actor{ID: "u-x"}
	if _, _, err := s.QnA(ctx, x, "r"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read Q&A: %v", err)
	}
	if _, err := s.Ask(ctx, x, "r", "hi?", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger asked: %v", err)
	}
	if _, err := s.Ask(ctx, Actor{ID: "u-a"}, "other-room", "hi?", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("asked in a meeting they are not in: %v", err)
	}
	q, _ := s.Ask(ctx, Actor{ID: "u-a"}, "r", "real question?", false)
	if _, err := s.Like(ctx, x, "r", q.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger liked: %v", err)
	}
	if err := s.ReplyTo(ctx, x, "r", q.ID, "me too", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger replied: %v", err)
	}
	if err := s.Moderate(ctx, Actor{ID: "u-a"}, "r", q.ID, "pin"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an attendee moderated: %v", err)
	}
	if err := s.SetQnA(ctx, Actor{ID: "u-a"}, "r", QnASettings{Closed: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an attendee changed the settings: %v", err)
	}
}

func TestQnAPostingRules(t *testing.T) {
	s, log := qnaFixture(t)
	ctx := context.Background()
	a, b, host := Actor{ID: "u-a"}, Actor{ID: "u-b"}, Actor{ID: "u-host"}

	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxQuestionLen+1)} {
		if _, err := s.Ask(ctx, a, "r", bad, false); !errors.Is(err, ErrQnABody) {
			t.Fatalf("body of %d: %v", len(bad), err)
		}
	}
	if _, err := s.Ask(ctx, a, "r", strings.Repeat("é", MaxQuestionLen), false); err != nil {
		t.Fatalf("1000 characters refused: %v", err)
	}

	// Anonymous: no author for anyone, the host included; the author knows it is theirs.
	anon, err := s.Ask(ctx, a, "r", "anonymous one?", true)
	if err != nil || anon.Author != "" || !anon.Mine {
		t.Fatalf("anonymous ask: %+v %v", anon, err)
	}
	if last := (*log)[len(*log)-1]; last.body["question"].(map[string]any)["author"] != "" || last.restricted {
		t.Fatalf("the push named an anonymous author or was restricted: %+v", last)
	}
	for _, who := range []Actor{host, b, a} {
		qs, _, _ := s.QnA(ctx, who, "r")
		for _, q := range qs {
			if q.Anonymous && q.Author != "" {
				t.Fatalf("%s sees the anonymous author %q", who.ID, q.Author)
			}
			if q.ID == anon.ID && q.Mine != (who.ID == "u-a") {
				t.Fatalf("%s: mine=%v", who.ID, q.Mine)
			}
		}
	}
	if err := s.SetQnA(ctx, host, "r", QnASettings{Anonymous: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ask(ctx, a, "r", "again?", true); !errors.Is(err, ErrQnAAnon) {
		t.Fatalf("anonymous with it off: %v", err)
	}

	// Moderation: an attendee's question is pending; the host's is not.
	s.SetQnA(ctx, host, "r", QnASettings{Moderation: true})
	pend, _ := s.Ask(ctx, b, "r", "held for review?", false)
	if pend.Status != "pending" {
		t.Fatalf("status %q under moderation", pend.Status)
	}
	if last := (*log)[len(*log)-1]; !last.restricted || last.author != "u-b" {
		t.Fatalf("a pending question was pushed to everyone: %+v", last)
	}
	if hq, _ := s.Ask(ctx, host, "r", "host asks", false); hq.Status != "published" {
		t.Fatal("the host's own question was held")
	}
	see := func(who Actor) []string { qs, _, _ := s.QnA(ctx, who, "r"); return ids(qs) }
	if !contains(see(host), "held for review?") || !contains(see(b), "held for review?") || contains(see(a), "held for review?") {
		t.Fatalf("pending visibility: host %v b %v a %v", see(host), see(b), see(a))
	}
	// Dismissed: the host and the author still see it; others never did.
	if err := s.Moderate(ctx, host, "r", pend.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	if !contains(see(b), "held for review?") || contains(see(a), "held for review?") {
		t.Fatal("dismissed visibility")
	}
	// Closed.
	s.SetQnA(ctx, host, "r", QnASettings{Closed: true})
	if _, err := s.Ask(ctx, a, "r", "late?", false); !errors.Is(err, ErrQnAClosed) {
		t.Fatalf("asked after Close Q&A: %v", err)
	}
	if last := (*log)[len(*log)-1]; last.body["settings"] == nil || last.restricted {
		t.Fatalf("settings change not pushed to the room: %+v", last)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestQnALikesRepliesModeration(t *testing.T) {
	s, _ := qnaFixture(t)
	ctx := context.Background()
	a, b, host := Actor{ID: "u-a"}, Actor{ID: "u-b"}, Actor{ID: "u-host"}
	q, _ := s.Ask(ctx, a, "r", "what's next?", false)

	if liked, err := s.Like(ctx, b, "r", q.ID); !liked || err != nil {
		t.Fatalf("like: %v %v", liked, err)
	}
	s.Like(ctx, host, "r", q.ID)
	get := func(who Actor) *Question {
		qs, _, _ := s.QnA(ctx, who, "r")
		for _, x := range qs {
			if x.ID == q.ID {
				return x
			}
		}
		return nil
	}
	if g := get(b); g.Likes != 2 || !g.Liked {
		t.Fatalf("likes %d liked %v", g.Likes, g.Liked)
	}
	if liked, _ := s.Like(ctx, b, "r", q.ID); liked || get(b).Likes != 1 || get(b).Liked {
		t.Fatal("a second like did not take it back")
	}

	if err := s.ReplyTo(ctx, b, "r", q.ID, "same here", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplyTo(ctx, a, "r", q.ID, "anon reply", true); err != nil {
		t.Fatal(err)
	}
	g := get(host)
	if len(g.Replies) != 2 || g.Replies[0].Author != "Ben" || g.Replies[1].Author != "" {
		t.Fatalf("replies %+v", g.Replies)
	}
	if err := s.ReplyTo(ctx, b, "r", q.ID, strings.Repeat("x", MaxReplyLen+1), false); !errors.Is(err, ErrQnABody) {
		t.Fatalf("over-long reply: %v", err)
	}

	for _, act := range []string{"pin", "answered"} {
		if err := s.Moderate(ctx, host, "r", q.ID, act); err != nil {
			t.Fatal(err)
		}
	}
	if g := get(a); !g.Pinned || !g.Answered {
		t.Fatalf("pin/answered not applied: %+v", g)
	}
	if err := s.Moderate(ctx, host, "r", q.ID, "explode"); !errors.Is(err, ErrQnAAction) {
		t.Fatalf("unknown action: %v", err)
	}
	if err := s.Moderate(ctx, host, "r", "no-such-id", "pin"); !errors.Is(err, ErrQnANotFound) {
		t.Fatalf("a question that does not exist: %v", err)
	}
	// A pending question cannot be liked or replied to by others.
	s.SetQnA(ctx, host, "r", QnASettings{Moderation: true})
	p, _ := s.Ask(ctx, b, "r", "pending one", false)
	if _, err := s.Like(ctx, a, "r", p.ID); !errors.Is(err, ErrQnANotFound) {
		t.Fatalf("liked a pending question: %v", err)
	}
	if err := s.ReplyTo(ctx, a, "r", p.ID, "hm", false); !errors.Is(err, ErrQnANotFound) {
		t.Fatalf("replied to a pending question: %v", err)
	}
}

// The bounds: a thousand questions a meeting, two hundred replies a question.
func TestQnABounds(t *testing.T) {
	s, _ := qnaFixture(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	now := time.Now()
	for i := 0; i < MaxQuestionsPerRoom; i++ {
		if err := repo.InsertQuestion(ctx, "r", &Question{ID: fmt.Sprint("q", i), Body: "b", Status: "published", CreatedAt: now, authorID: "u-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Ask(ctx, Actor{ID: "u-a"}, "r", "one more?", false); !errors.Is(err, ErrQnAFull) {
		t.Fatalf("question 1001: %v", err)
	}
	for i := 0; i < MaxRepliesPerQuestion; i++ {
		if err := repo.InsertReply(ctx, "q0", "u-a", &Reply{ID: fmt.Sprint("r", i), Body: "x", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplyTo(ctx, Actor{ID: "u-b"}, "r", "q0", "one more", false); !errors.Is(err, ErrQnAFull) {
		t.Fatalf("reply 201: %v", err)
	}
	// Index-backed reads.
	for _, q := range []string{
		`EXPLAIN QUERY PLAN SELECT id FROM meeting_qna_questions WHERE room = 'r' ORDER BY created_at`,
		`EXPLAIN QUERY PLAN SELECT id FROM meeting_qna_replies WHERE question_id = 'q0' ORDER BY created_at`,
	} {
		rows, _ := repo.db.QueryContext(ctx, q)
		plan := ""
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			plan += d + "; "
		}
		rows.Close()
		if !strings.Contains(plan, "USING INDEX") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("not index-backed: %s", plan)
		}
	}
}
