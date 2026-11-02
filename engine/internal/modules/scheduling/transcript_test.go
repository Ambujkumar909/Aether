package scheduling

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Lines are kept in order per session; the latest session is the default;
// whoever is in the meeting or attended it reads it, nobody else does.
func TestTranscriptKeepsLinesAndChecksWhoReads(t *testing.T) {
	s, _ := qnaFixture(t) // room "r": u-host, u-a, u-b live; u-x outside
	ctx := context.Background()
	s1 := time.UnixMilli(1_700_000_000_000)
	s2 := s1.Add(2 * time.Hour)
	for i, l := range []string{"Good morning", "Let's start", "Agreed"} {
		if err := s.AppendTranscript(ctx, "r", s1, "u-a", "Asha", l, s1.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendTranscript(ctx, "r", s2, "u-b", "Ben", "Second meeting", s2.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Refused quietly: empty, too long, not UTF-8. Nothing stored.
	for _, bad := range []string{"  ", strings.Repeat("x", MaxTranscriptLineLen+1), "\xff\xfe"} {
		_ = s.AppendTranscript(ctx, "r", s2, "u-b", "Ben", bad, s2)
	}

	sess, lines, err := s.Transcript(ctx, Actor{ID: "u-b"}, "r", 0)
	if err != nil || sess != s2.UnixMilli() || len(lines) != 1 || lines[0].Text != "Second meeting" {
		t.Fatalf("latest session: %d %+v %v", sess, lines, err)
	}
	_, lines, err = s.Transcript(ctx, Actor{ID: "u-host"}, "r", s1.UnixMilli())
	if err != nil || len(lines) != 3 {
		t.Fatalf("first session: %+v %v", lines, err)
	}
	for i, l := range lines {
		if l.N != i+1 || l.Name != "Asha" {
			t.Fatalf("line %d: %+v", i, l)
		}
	}
	if lines[2].Text != "Agreed" {
		t.Fatalf("out of order: %+v", lines)
	}
	if _, _, err := s.Transcript(ctx, Actor{ID: "u-x"}, "r", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read it: %v", err)
	}
	if _, _, err := s.Transcript(ctx, Actor{}, "r", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nobody read it: %v", err)
	}
	// Someone who attended, after they left, still can.
	s.Joined(ctx, "u-gone", "r", "Gus")
	if _, _, err := s.Transcript(ctx, Actor{ID: "u-gone"}, "r", 0); err != nil {
		t.Fatalf("an attendee who left: %v", err)
	}
	// ...but attending another room is not attending this one.
	s.Joined(ctx, "u-other", "elsewhere", "Oli")
	if _, _, err := s.Transcript(ctx, Actor{ID: "u-other"}, "r", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("attended a different room: %v", err)
	}
}

// Two speakers at once never take the same number, and the bound holds one
// over the line.
func TestTranscriptNumbersAndBound(t *testing.T) {
	s, _ := qnaFixture(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	sess := time.UnixMilli(1_700_000_000_000)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := s.AppendTranscript(ctx, "r", sess, "u-a", "Asha", "line", sess); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	_, lines, _ := s.Transcript(ctx, Actor{ID: "u-a"}, "r", sess.UnixMilli())
	if len(lines) != 200 {
		t.Fatalf("%d lines kept of 200", len(lines))
	}
	for i, l := range lines {
		if l.N != i+1 {
			t.Fatalf("line %d numbered %d", i+1, l.N)
		}
	}

	// Fill the session to the bound in one statement, then one over.
	if _, err := repo.db.ExecContext(ctx, `
		WITH RECURSIVE k(n) AS (SELECT 201 UNION ALL SELECT n+1 FROM k WHERE n < ?)
		INSERT INTO meeting_transcript (room, session, n, user_id, name, body, at)
		SELECT 'r', ?, n, 'u-a', 'Asha', 'x', ? FROM k`, MaxTranscriptLines-1, sess.UnixMilli(), sess.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTranscript(ctx, "r", sess, "u-a", "Asha", "the last one", sess); err != nil {
		t.Fatalf("line %d: %v", MaxTranscriptLines, err)
	}
	if err := s.AppendTranscript(ctx, "r", sess, "u-a", "Asha", "one too many", sess); !errors.Is(err, ErrTranscriptFull) {
		t.Fatalf("line %d: %v", MaxTranscriptLines+1, err)
	}

	for _, q := range []string{
		`SELECT n, name, body, at FROM meeting_transcript WHERE room = 'r' AND session = 1 ORDER BY n`,
		`SELECT MAX(session) FROM meeting_transcript WHERE room = 'r'`,
		`SELECT 1 FROM scheduling_attendance WHERE room = 'r' AND user_id = 'u' LIMIT 1`,
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
		if strings.Contains(plan, "SCAN") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("not index-backed: %s -> %s", q, plan)
		}
	}
}

// The downloads: cue times from the session start, each cue ending where
// the next begins, names and text escaped so a line cannot forge a cue.
func TestTranscriptDownloads(t *testing.T) {
	sess := time.UnixMilli(1_700_000_000_000).UTC()
	lines := []TranscriptLine{
		{N: 1, Name: "Asha", Text: "Hello <b>team</b> & all", At: sess.Add(61500 * time.Millisecond)},
		{N: 2, Name: "Ben\n\n2\n00:00:00.000 --> 00:00:01.000", Text: "fine -->\nreally", At: sess.Add(3723 * time.Second)},
	}
	vtt := string(TranscriptVTT(sess.UnixMilli(), lines))
	want := "WEBVTT\n" +
		"\n1\n00:01:01.500 --> 01:02:03.000\n<v Asha>Hello &lt;b&gt;team&lt;/b&gt; &amp; all</v>\n" +
		"\n2\n01:02:03.000 --> 01:02:08.000\n<v Ben  2 00:00:00.000 --&gt; 00:00:01.000>fine --&gt; really</v>\n"
	if vtt != want {
		t.Fatalf("vtt:\n%q\nwant\n%q", vtt, want)
	}
	txt := string(TranscriptText(sess.UnixMilli(), lines[:1]))
	if txt != "[00:01:01] Asha: Hello <b>team</b> & all\n" {
		t.Fatalf("txt: %q", txt)
	}
}
