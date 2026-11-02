package messaging

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// Scale: the shapes a 50,000-person deployment produces, measured.
//
// This is not a microbenchmark of one query; it is the read paths a client
// hits on every screen -- the conversation list, a page of history, the
// shared view -- against a database with the row counts such a deployment
// accumulates, with the answer checked for correctness as well as timed.
// Skipped under -short. Thresholds are loose enough for a laptop and tight
// enough that a full table scan cannot pass them.
//
//   go test -run Scale -v ./internal/modules/messaging/

func TestScaleReadPathsStayFlat(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("scale test; run without -short and without -race (timings only)")
	}
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)

	const (
		people       = 50_000
		convs        = 6_000 // ~ one active thread per eight people
		perConv      = 60    // messages in each
		busyPersonID = "u-busy"
	)
	// Seed straight through the repository in big transactions: the point is
	// the read paths, and 360k inserts through the service would take the
	// rest of the afternoon.
	start := time.Now()
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-30 * 24 * time.Hour)
	insConv, _ := tx.PrepareContext(ctx, `INSERT INTO messaging_conversations (id, kind, title, created_by, created_at, last_message_at) VALUES (?,?,?,?,?,?)`)
	insMem, _ := tx.PrepareContext(ctx, `INSERT INTO messaging_members (conversation_id, user_id, email, name, joined_at, last_read) VALUES (?,?,?,?,?,?)`)
	// One placeholder per column, however many msgCols has grown to: the
	// statement silently failed to prepare when the column list outgrew a
	// hand-counted VALUES list, and the first Exec dereferenced a nil.
	insMsg, err := tx.PrepareContext(ctx, `INSERT INTO messaging_messages (`+msgCols+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(strings.Split(msgCols, ","))), ",")+`)`)
	if err != nil {
		t.Fatal(err)
	}
	busyConvs := 0
	for c := 0; c < convs; c++ {
		cid := fmt.Sprintf("c%05d", c)
		a := fmt.Sprintf("u%05d", (c*7)%people)
		b := fmt.Sprintf("u%05d", (c*13+1)%people)
		// The busy person is in every 30th conversation: two hundred threads.
		if c%30 == 0 {
			b = busyPersonID
			busyConvs++
		}
		created := now.Add(time.Duration(c) * time.Minute)
		last := created.Add(time.Duration(perConv) * time.Second)
		if _, err := insConv.ExecContext(ctx, cid, "direct", "", a, created.UnixMilli(), last.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{a, b} {
			if _, err := insMem.ExecContext(ctx, cid, u, u+"@example.com", "Name "+u, created.UnixMilli(), created.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < perConv; i++ {
			from := a
			if i%2 == 1 {
				from = b
			}
			at := created.Add(time.Duration(i) * time.Second)
			if _, err := insMsg.ExecContext(ctx, fmt.Sprintf("%s-m%03d", cid, i), cid, from, "Name "+from,
				fmt.Sprintf("message %d in %s https://example.com/%d", i, cid, i%7), at.UnixMilli(), nil, 0, "", 0, 0, "", nil, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d conversations, %d members, %d messages in %s",
		convs, 2*convs, convs*perConv, time.Since(start).Round(time.Millisecond))

	busy := Actor{ID: busyPersonID, Email: "busy@example.com", Name: "Busy"}
	// A few hidden and saved rows for the busy person so those joins run.
	for i := 0; i < 50; i++ {
		_ = repo.Hide(ctx, busy.ID, fmt.Sprintf("c%05d-m%03d", i*30, i%perConv))
		_ = repo.Save(ctx, busy.ID, fmt.Sprintf("c%05d-m%03d", i*30, (i+1)%perConv), true)
	}

	measure := func(name string, runs int, fn func(i int)) {
		fn(0) // warm
		ds := make([]time.Duration, 0, runs)
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			fn(i)
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		p50, p95, max := ds[len(ds)/2], ds[len(ds)*95/100], ds[len(ds)-1]
		t.Logf("%-42s p50 %8s  p95 %8s  max %8s", name, p50, p95, max)
		if p95 > 50*time.Millisecond {
			t.Errorf("%s: p95 %s is over 50 ms -- a scan, not an index", name, p95)
		}
	}

	measure("conversation list (200 threads, decorated)", 30, func(int) {
		list, err := s.List(ctx, busy)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != busyConvs {
			t.Fatalf("busy person sees %d conversations, want %d", len(list), busyConvs)
		}
	})
	measure("history page (60 msgs, hidden+saved+react)", 100, func(i int) {
		cid := fmt.Sprintf("c%05d", (i*30)%convs)
		ms, err := s.Messages(ctx, busy, cid, time.Time{}, 60)
		if err != nil {
			t.Fatal(err)
		}
		s.Decorate(ctx, busy, ms)
		if len(ms) < perConv-1 {
			t.Fatalf("page of %d, want ~%d", len(ms), perConv)
		}
	})
	measure("older page (before cursor)", 100, func(i int) {
		cid := fmt.Sprintf("c%05d", (i*30)%convs)
		mid := now.Add(time.Duration((i*30)%convs)*time.Minute + 30*time.Second)
		if _, err := s.Messages(ctx, busy, cid, mid, 60); err != nil {
			t.Fatal(err)
		}
	})
	measure("shared links (scan of 500 newest)", 30, func(i int) {
		cid := fmt.Sprintf("c%05d", (i*30)%convs)
		links, err := s.SharedLinks(ctx, busy, cid, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(links) == 0 {
			t.Fatal("no links found in a thread full of them")
		}
	})
	measure("unread counts for the list", 30, func(int) {
		if _, err := s.List(ctx, busy); err != nil {
			t.Fatal(err)
		}
	})
}

var _ = os.Stat // kept for the file-size log a later version may print
