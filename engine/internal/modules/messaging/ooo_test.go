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

// A line and a window, each bound tested one over the line; a start in the
// past means now.
func TestOutOfOfficeIsBoundedAndWindowed(t *testing.T) {
	s, me, _, _ := twoPeople(t)
	ctx := context.Background()
	now := time.Now()
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	bad := []struct {
		name        string
		message     string
		from, until *time.Time
	}{
		{"no message", "   ", nil, nil},
		{"a message one over the bound", strings.Repeat("x", MaxOOOMessage+1), nil, nil},
		{"an end before its start", "Away", at(2 * time.Hour), at(time.Hour)},
		{"an end in the past", "Away", nil, at(-time.Minute)},
		{"a start more than a year ahead", "Away", at(MaxOOOAhead + time.Hour), nil},
		{"an end more than a year ahead", "Away", nil, at(MaxOOOAhead + time.Hour)},
	}
	for _, c := range bad {
		if _, err := s.SetOOO(ctx, me, c.message, c.from, c.until); !errors.Is(err, ErrBadOOO) {
			t.Fatalf("%s: err=%v", c.name, err)
		}
	}
	if _, ok, _ := s.MyOOO(ctx, me); ok {
		t.Fatal("a refused out of office was stored")
	}
	o, err := s.SetOOO(ctx, me, "  "+strings.Repeat("x", MaxOOOMessage)+" ", at(-time.Hour), at(time.Hour))
	if err != nil {
		t.Fatalf("a message at the bound: %v", err)
	}
	if len(o.Message) != MaxOOOMessage || !o.Active || o.From == nil || o.From.Before(now.Add(-time.Minute)) || o.Until == nil {
		t.Fatalf("stored: %+v", o)
	}
}

// Peers are shown it only while it is in effect: not before its window, not
// after, never a stranger; its owner sees the window.
func TestOutOfOfficeIsShownOnlyWhileInEffect(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	peer := func() *OOO {
		got, err := s.Statuses(ctx, you, []string{me.ID})
		if err != nil {
			t.Fatal(err)
		}
		return got[me.ID].OOO
	}
	from, until := time.Now().Add(time.Hour), time.Now().Add(48*time.Hour)
	if _, err := s.SetOOO(ctx, me, "Back on Monday", &from, &until); err != nil {
		t.Fatal(err)
	}
	if o := peer(); o != nil {
		t.Fatalf("announced before it began: %+v", o)
	}
	mine, ok, _ := s.MyOOO(ctx, me)
	if !ok || mine.Active || mine.Message != "Back on Monday" || mine.From == nil || mine.Until == nil {
		t.Fatalf("my own view of a planned one: %+v ok=%v", mine, ok)
	}

	set := func(col string, at time.Time) {
		if _, err := repo.db.ExecContext(ctx, `UPDATE messaging_ooo SET `+col+` = ? WHERE user_id = ?`, at.UnixMilli(), me.ID); err != nil {
			t.Fatal(err)
		}
	}
	set("starts_at", time.Now().Add(-time.Second))
	o := peer()
	if o == nil || o.Message != "Back on Monday" || o.Until == nil || o.From != nil {
		t.Fatalf("in effect, as a peer sees it: %+v", o)
	}
	// It stands beside the status, whatever the status is.
	if _, err := s.SetStatus(ctx, me, StatusBusy, "heads down", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Statuses(ctx, you, []string{me.ID}); got[me.ID].Status != StatusBusy || got[me.ID].OOO == nil {
		t.Fatalf("beside a picked status: %+v", got[me.ID])
	}
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if got, _ := s.Statuses(ctx, stranger, []string{me.ID}); len(got) != 0 {
		t.Fatalf("a stranger read it: %+v", got)
	}

	set("ends_at", time.Now().Add(-time.Millisecond))
	if o := peer(); o != nil {
		t.Fatalf("shown after it ended: %+v", o)
	}
	if _, ok, _ := s.MyOOO(ctx, me); ok {
		t.Fatal("an ended one is still mine to see")
	}

	// Open-ended, then turned off.
	if _, err := s.SetOOO(ctx, me, "On leave", nil, nil); err != nil {
		t.Fatal(err)
	}
	if o := peer(); o == nil || o.Until != nil {
		t.Fatalf("open-ended: %+v", o)
	}
	if err := s.ClearOOO(ctx, me); err != nil {
		t.Fatal(err)
	}
	if o := peer(); o != nil {
		t.Fatalf("shown after it was turned off: %+v", o)
	}
}

// statusFrame is a status push as the client reads it.
type statusFrame struct {
	who, status, note string
	hasOOO            bool
	ooo               string
	oooUntil          uint64
	left              int
}

func readStatusFrame(t *testing.T, b []byte, rx *wire.Dict) statusFrame {
	t.Helper()
	r := wire.NewReader(b, rx)
	h, err := r.ReadHeader()
	if err != nil || h.Op != wire.OpStatus {
		t.Fatalf("not a status frame: op=%v err=%v", h.Op, err)
	}
	f := statusFrame{who: r.Str(), status: r.Str(), note: r.Str()}
	r.Uvarint() // until
	r.Bool()    // auto
	r.Bool()    // online
	f.hasOOO = r.Bool()
	r.Bits(5)
	r.Uvarint() // last seen
	if f.hasOOO {
		f.ooo = string(r.Blob())
		f.oooUntil = r.Uvarint()
	}
	if r.Err() != nil {
		t.Fatalf("status frame: %v", r.Err())
	}
	f.left = r.Left()
	return f
}

// The push carries what the view draws: the line and its end ride on the
// status frame while it is in effect, and cost one bit when there is none.
// Turning it on later tells nobody now.
func TestOutOfOfficeRidesTheStatusPush(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	tx, rx := wire.NewPreloadedPair()
	yours := &sock{userID: you.ID, tx: tx, out: make(chan []byte, 64)}
	s.hub.add(yours)
	defer s.hub.remove(yours)
	next := func() (statusFrame, bool) {
		select {
		case b := <-yours.out:
			return readStatusFrame(t, b, rx), true
		default:
			return statusFrame{}, false
		}
	}

	later := time.Now().Add(time.Hour)
	if _, err := s.SetOOO(ctx, me, "Next week", &later, nil); err != nil {
		t.Fatal(err)
	}
	if f, got := next(); got {
		t.Fatalf("a planned one was pushed: %+v", f)
	}

	until := time.Now().Add(24 * time.Hour)
	if _, err := s.SetOOO(ctx, me, "Back tomorrow. Asha is covering.", nil, &until); err != nil {
		t.Fatal(err)
	}
	f, got := next()
	if !got || f.who != me.ID || !f.hasOOO || f.ooo != "Back tomorrow. Asha is covering." || f.oooUntil != uint64(until.UnixMilli()) || f.left != 0 {
		t.Fatalf("turned on: got=%v %+v", got, f)
	}
	withLen := 0
	// A status change while away still carries it.
	if _, err := s.SetStatus(ctx, me, StatusBusy, "", nil); err != nil {
		t.Fatal(err)
	}
	if f, got = next(); !got || f.status != StatusBusy || !f.hasOOO {
		t.Fatalf("a status change while away: got=%v %+v", got, f)
	}

	if err := s.ClearOOO(ctx, me); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-yours.out:
		f = readStatusFrame(t, b, rx)
		withLen = len(b)
	default:
		t.Fatal("turning it off told nobody")
	}
	if f.hasOOO || f.ooo != "" || f.left != 0 {
		t.Fatalf("turned off: %+v", f)
	}
	// Without one, the frame is exactly what it was before this feature:
	// header, three interned strings, the flag byte and two varints.
	if _, err := s.SetStatus(ctx, me, StatusBusy, "", nil); err != nil {
		t.Fatal(err)
	}
	b := <-yours.out
	if len(b) != withLen {
		t.Fatalf("a status frame with no out of office is %d B, the one before it %d B", len(b), withLen)
	}
	// Clearing what is not set tells nobody.
	if err := s.ClearOOO(ctx, me); err != nil {
		t.Fatal(err)
	}
	if f, got := next(); got {
		t.Fatalf("clearing nothing pushed: %+v", f)
	}
}

// The window's edges are noticed by the status loop: when it begins the
// peer and the person are told, and again when it ends, with nobody having
// done anything at either moment.
func TestOutOfOfficeEdgesAreNoticedByTheLoop(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	repo := s.repo.(*SQLRepository)
	type push struct {
		to string
		st Status
	}
	got := make(chan push, 64)
	s.autoPushed = func(to string, st Status) { got <- push{to, st} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.RunAutoStatus(ctx, time.Hour); close(done) }()
	wait := func(what string) map[string]Status {
		out := map[string]Status{}
		deadline := time.After(5 * time.Second)
		for len(out) < 2 {
			select {
			case p := <-got:
				out[p.to] = p.st
			case <-deadline:
				t.Fatalf("%s: waited for 2 pushes, got %d: %v", what, len(out), out)
			}
		}
		return out
	}
	quiet := func(what string) {
		select {
		case p := <-got:
			t.Fatalf("%s: pushed %+v", what, p)
		case <-time.After(150 * time.Millisecond):
		}
	}

	// One that starts now is told when it is set, and that is the telling:
	// the loop must not find its start as an edge and say it again. (Found
	// on the wire: a second, identical 106 B frame at the next tick.) The
	// loop has to be running first, as it is in a server: a first version
	// of this check passed against the broken code because the loop had
	// not yet read the clock when the out of office was set.
	s.NudgeAutoStatus()
	quiet("nothing has happened yet")
	if _, err := s.SetOOO(ctx, me, "Stepping out", nil, nil); err != nil {
		t.Fatal(err)
	}
	if seen := wait("turned on now"); seen[you.ID].OOO == nil {
		t.Fatalf("turned on now: %+v", seen)
	}
	s.NudgeAutoStatus()
	quiet("one that starts now is told once")
	if err := s.ClearOOO(ctx, me); err != nil {
		t.Fatal(err)
	}
	wait("turned off")

	from := time.Now().Add(300 * time.Millisecond)
	if _, err := s.SetOOO(ctx, me, "At the dentist", &from, nil); err != nil {
		t.Fatal(err)
	}
	s.NudgeAutoStatus()
	quiet("before the window")

	time.Sleep(time.Until(from) + 30*time.Millisecond)
	s.NudgeAutoStatus()
	seen := wait("the window began")
	if seen[you.ID].OOO == nil || seen[you.ID].OOO.Message != "At the dentist" || seen[me.ID].OOO == nil {
		t.Fatalf("began: %+v", seen)
	}
	s.NudgeAutoStatus()
	quiet("an edge is told once")

	end := time.Now().Add(100 * time.Millisecond)
	if _, err := repo.db.ExecContext(ctx, `UPDATE messaging_ooo SET ends_at = ? WHERE user_id = ?`, end.UnixMilli(), me.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(end) + 30*time.Millisecond)
	s.NudgeAutoStatus()
	seen = wait("the window ended")
	if seen[you.ID].OOO != nil || seen[me.ID].OOO != nil {
		t.Fatalf("ended: %+v", seen)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}

// Fifty thousand people, one in ten of them away: the tick's question (whose
// window turned since the last one) and the list's (who among these 200 is
// away) are index reads, measured.
func TestOutOfOfficeAtScaleIsIndexReads(t *testing.T) {
	s := serviceForTest(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	for _, q := range []string{
		`SELECT user_id FROM messaging_ooo WHERE starts_at > 5 AND starts_at <= 9 AND starts_at > updated_at`,
		`SELECT user_id FROM messaging_ooo WHERE ends_at > 5 AND ends_at <= 9`,
		`SELECT user_id, message, ends_at FROM messaging_ooo WHERE user_id IN ('a','b') AND starts_at <= 9 AND (ends_at = 0 OR ends_at > 9)`,
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
		if strings.Contains(plan, "SCAN") {
			t.Fatalf("not index-backed: %s\n-> %s", q, plan)
		}
	}
	if testing.Short() || raceEnabled {
		t.Skip("timings only without -short and without -race")
	}
	const away = 5_000 // of 50,000
	now := time.Now().UTC()
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range away {
		// Windows spread over a month either side of now.
		start := now.Add(time.Duration(i-away/2) * 10 * time.Minute)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messaging_ooo(user_id, message, starts_at, ends_at, updated_at) VALUES (?, 'Away', ?, ?, ?)`,
			fmt.Sprintf("u%05d", i), start.UnixMilli(), start.Add(72*time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%05d", i*25)
	}
	const rounds = 200
	t0 := time.Now()
	var crossed int
	for range rounds {
		got, err := repo.OOOCrossed(ctx, now.Add(-15*time.Second), now)
		if err != nil {
			t.Fatal(err)
		}
		crossed = len(got)
	}
	tick := time.Since(t0) / rounds
	t0 = time.Now()
	var active int
	for range rounds {
		got, err := repo.OOOActive(ctx, ids, now)
		if err != nil {
			t.Fatal(err)
		}
		active = len(got)
	}
	list := time.Since(t0) / rounds
	t.Logf("%d away of 50,000: tick (edges in 15 s) %v, %d found; who of 200 is away %v, %d found", away, tick, crossed, list, active)
	if tick > 2*time.Millisecond || list > 5*time.Millisecond {
		t.Fatalf("too slow for a scan-free read: tick %v, list %v", tick, list)
	}
	if active == 0 || active == len(ids) {
		t.Fatalf("the seed should leave some of the 200 away and some not: %d", active)
	}
}
