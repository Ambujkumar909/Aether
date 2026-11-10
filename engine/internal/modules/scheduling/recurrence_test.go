package scheduling

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Recurring meetings: the rule expands to the right occurrences, in the
// organiser's zone, through a clock change; a series is one row that every
// window query finds; one occurrence can be cancelled on its own; free/busy
// says when, and only when.

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no zone data for", name)
	}
	return loc
}

func TestWeeklyOccurrencesKeepTheirWallClockAcrossAClockChange(t *testing.T) {
	loc := mustLoc(t, "Europe/Berlin")
	// Mondays 09:00 Berlin from 2026-03-16; Berlin moves to summer time on
	// 2026-03-29, so the UTC start shifts from 08:00 to 07:00 and the local
	// time must not.
	first := time.Date(2026, 3, 16, 9, 0, 0, 0, loc)
	r := &Recurrence{Freq: FreqWeekly, Until: time.Date(2026, 4, 13, 0, 0, 0, 0, loc), TimeZone: "Europe/Berlin"}
	if err := r.check(first, first.Add(30*time.Minute), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got := r.occurrences(first, time.Time{}, time.Time{}, 100)
	if len(got) != 5 {
		t.Fatalf("got %d occurrences, want 5 (16 Mar to 13 Apr inclusive): %v", len(got), got)
	}
	for i, o := range got {
		if l := o.In(loc); l.Hour() != 9 || l.Weekday() != time.Monday {
			t.Fatalf("occurrence %d is %s, not Monday 09:00 Berlin", i, l)
		}
	}
	if got[1].Hour() != 8 || got[2].Hour() != 7 {
		t.Fatalf("UTC hours %d then %d: the clock change was not honoured", got[1].Hour(), got[2].Hour())
	}
	if r.Duration != 30 {
		t.Fatalf("duration %d", r.Duration)
	}
}

func TestWeeklyOnSeveralDaysAndEveryOtherWeek(t *testing.T) {
	first := time.Date(2026, 1, 6, 10, 0, 0, 0, time.UTC) // a Tuesday
	r := &Recurrence{Freq: FreqWeekly, Interval: 2, Days: []int{4, 2}, Until: time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)}
	if err := r.check(first, first.Add(time.Hour), first.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, o := range r.occurrences(first, time.Time{}, time.Time{}, 100) {
		days = append(days, o.Format("Mon 02 Jan"))
	}
	want := "[Tue 06 Jan Thu 08 Jan Tue 20 Jan Thu 22 Jan Tue 03 Feb Thu 05 Feb]"
	if got := "[" + joinStrings(days, " ") + "]"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if r.Days[0] != 2 || r.Days[1] != 4 {
		t.Fatalf("days not normalised: %v", r.Days)
	}
	// A first meeting on a day the rule does not include is refused.
	bad := &Recurrence{Freq: FreqWeekly, Days: []int{1}, Until: first.AddDate(0, 1, 0)}
	if err := bad.check(first, first.Add(time.Hour), first); !errors.Is(err, ErrBadRecurrence) {
		t.Fatalf("a rule that excludes its own first meeting was accepted: %v", err)
	}
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func TestMonthlyOnThe31stTakesTheLastDayOfShorterMonths(t *testing.T) {
	first := time.Date(2026, 1, 31, 14, 0, 0, 0, time.UTC)
	r := &Recurrence{Freq: FreqMonthly, Until: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)}
	if err := r.check(first, first.Add(time.Hour), first); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range r.occurrences(first, time.Time{}, time.Time{}, 100) {
		got = append(got, o.Format("Jan 02"))
	}
	if s := joinStrings(got, ","); s != "Jan 31,Feb 28,Mar 31,Apr 30,May 31" {
		t.Fatalf("got %s", s)
	}
}

func TestDailyWindowAndSkipsAndCaps(t *testing.T) {
	first := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	r := &Recurrence{Freq: FreqDaily, Until: time.Date(2028, 5, 30, 0, 0, 0, 0, time.UTC)}
	if err := r.check(first, first.Add(time.Hour), first); err != nil {
		t.Fatal(err)
	}
	// A window: only what overlaps it, in order.
	from := time.Date(2026, 6, 10, 9, 30, 0, 0, time.UTC) // the 10th is in progress at from
	to := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	got := r.occurrences(first, from, to, 100)
	if len(got) != 3 || got[0].Day() != 10 || got[2].Day() != 12 {
		t.Fatalf("window: %v", got)
	}
	// A skipped occurrence is left out, and the last end ignores skips.
	r.Skipped = []int64{time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC).UnixMilli()}
	got = r.occurrences(first, from, to, 100)
	if len(got) != 2 || got[1].Day() != 12 {
		t.Fatalf("skip: %v", got)
	}
	if end := r.lastEnd(first); !end.Equal(time.Date(2028, 5, 30, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("last end %s", end)
	}
	// Two years of a daily series is one row's worth of expansion, capped.
	all := r.occurrences(first, time.Time{}, time.Time{}, maxOccurrences)
	if len(all) != 729 { // 730 days, one skipped
		t.Fatalf("%d occurrences in two years", len(all))
	}
	if got := r.occurrences(first, time.Time{}, time.Time{}, 5); len(got) != 5 {
		t.Fatalf("cap: %d", len(got))
	}
	// Bounds.
	for _, bad := range []Recurrence{
		{Freq: "yearly", Until: first.AddDate(0, 1, 0)},
		{Freq: FreqDaily, Interval: 100, Until: first.AddDate(0, 1, 0)},
		{Freq: FreqDaily},
		{Freq: FreqDaily, Until: first.AddDate(0, 0, -1)},
		{Freq: FreqDaily, Until: first.AddDate(0, 1, 0), TimeZone: "Mars/Olympus"},
		{Freq: FreqWeekly, Days: []int{7}, Until: first.AddDate(0, 1, 0)},
	} {
		b := bad
		if err := b.check(first, first.Add(time.Hour), first); !errors.Is(err, ErrBadRecurrence) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	far := Recurrence{Freq: FreqDaily, Until: first.AddDate(3, 0, 0)}
	if err := far.check(first, first.Add(time.Hour), first); !errors.Is(err, ErrTooFar) {
		t.Fatalf("three years ahead: %v", err)
	}
}

func TestASeriesIsOneRowThatEveryWindowFindsAndCanLoseOneOccurrence(t *testing.T) {
	s := authzService(t)
	ctx := context.Background()
	// Started two days ago, so a room emptying today has an occurrence
	// behind it to (wrongly) complete.
	first := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	m, err := s.Create(ctx, owner, &Meeting{
		Title: "Standup", Category: CatInternal,
		StartsAt: first, EndsAt: first.Add(15 * time.Minute),
		Invitees:   []Invitee{{Email: invitee.Email}},
		Recurrence: &Recurrence{Freq: FreqDaily, Until: first.AddDate(0, 0, 9)},
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.EndsAt.Equal(first.Add(15 * time.Minute)) {
		t.Fatalf("create returned the series end %s, not the first occurrence's", m.EndsAt)
	}
	// Three weeks of calendar: ten occurrences, each with its own times and
	// the series' id, in order.
	ms, err := s.List(ctx, invitee, Filter{From: first.Add(-time.Hour), To: first.AddDate(0, 0, 21)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 10 {
		t.Fatalf("%d occurrences listed, want 10", len(ms))
	}
	for i, o := range ms {
		want := first.AddDate(0, 0, i)
		if o.ID != m.ID || !o.StartsAt.Equal(want) || !o.EndsAt.Equal(want.Add(15*time.Minute)) || o.Occurrence != want.UnixMilli() {
			t.Fatalf("occurrence %d: %s-%s occ %d id %s", i, o.StartsAt, o.EndsAt, o.Occurrence, o.ID)
		}
	}
	// A window in the middle of the series finds it through the row's end.
	mid, _ := s.List(ctx, invitee, Filter{From: first.AddDate(0, 0, 5), To: first.AddDate(0, 0, 7)})
	if len(mid) != 2 {
		t.Fatalf("mid-series window: %d", len(mid))
	}
	// Conflicts see occurrences too.
	cf, _ := s.Conflicts(ctx, invitee, first.AddDate(0, 0, 3).Add(5*time.Minute), first.AddDate(0, 0, 3).Add(10*time.Minute), "")
	if len(cf) != 1 || cf[0].Occurrence != first.AddDate(0, 0, 3).UnixMilli() {
		t.Fatalf("conflicts: %v", cf)
	}
	// By id: the series, with the first occurrence's times.
	got, err := s.ByID(ctx, invitee, m.ID)
	if err != nil || got.Recurrence == nil || !got.EndsAt.Equal(first.Add(15*time.Minute)) {
		t.Fatalf("by id: %v %v", got, err)
	}

	// Cancel the fourth occurrence: the invitee may not, a made-up time is
	// refused, the organiser can, the list loses exactly that one, and the
	// room emptying does not complete the series.
	fourth := first.AddDate(0, 0, 3).UnixMilli()
	if err := s.CancelOccurrence(ctx, invitee, m.ID, fourth, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("invitee cancelled an occurrence: %v", err)
	}
	if err := s.CancelOccurrence(ctx, owner, m.ID, fourth+60_000, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a made-up occurrence was accepted: %v", err)
	}
	if err := s.CancelOccurrence(ctx, owner, m.ID, fourth, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelOccurrence(ctx, owner, m.ID, fourth, "", ""); err != nil {
		t.Fatalf("cancelling twice: %v", err)
	}
	ms, _ = s.List(ctx, invitee, Filter{From: first.Add(-time.Hour), To: first.AddDate(0, 0, 21)})
	if len(ms) != 9 {
		t.Fatalf("%d after cancelling one", len(ms))
	}
	for _, o := range ms {
		if o.Occurrence == fourth {
			t.Fatal("the cancelled occurrence is still listed")
		}
	}
	s.RoomEmptied(ctx, m.Room)
	after, _ := s.repo.ByID(ctx, m.ID)
	if after.Status != StatusScheduled || len(after.Recurrence.Skipped) != 1 {
		t.Fatalf("after the room emptied: %s, skipped %v", after.Status, after.Recurrence.Skipped)
	}
	// Moving the series resets answers and keeps the cancelled occurrence.
	if _, err := s.Respond(ctx, invitee, m.ID, RespAccepted, "", ""); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Update(ctx, owner, m.ID, &Meeting{
		Title: "Standup", Category: CatInternal,
		StartsAt: first.Add(time.Hour), EndsAt: first.Add(75 * time.Minute),
		Recurrence: &Recurrence{Freq: FreqDaily, Until: first.AddDate(0, 0, 9)},
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Recurrence.Skipped) != 1 || moved.Invitees[1].Response != RespPending {
		t.Fatalf("after moving: skipped %v, invitee %s", moved.Recurrence.Skipped, moved.Invitees[1].Response)
	}
	// The whole series cancels like any meeting.
	if err := s.Cancel(ctx, owner, m.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	ms, _ = s.List(ctx, invitee, Filter{From: first.Add(-time.Hour), To: first.AddDate(0, 0, 21), Status: StatusScheduled})
	if len(ms) != 0 {
		t.Fatalf("%d occurrences of a cancelled series", len(ms))
	}
}

func TestFreeBusyAnswersSpansOnlyAndIsBounded(t *testing.T) {
	s := authzService(t)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	mk := func(title string, start, end time.Time, invs ...string) *Meeting {
		var inv []Invitee
		for _, e := range invs {
			inv = append(inv, Invitee{Email: e})
		}
		m, err := s.Create(ctx, owner, &Meeting{Title: title, Category: CatInternal, StartsAt: start, EndsAt: end, Invitees: inv}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	mk("Secret board matter", at(9), at(10), invitee.Email)
	declined := mk("Optional", at(11), at(12), invitee.Email)
	if _, err := s.Respond(ctx, invitee, declined.ID, RespDeclined, "", ""); err != nil {
		t.Fatal(err)
	}
	mk("Owner only", at(14), at(15))
	// A series the invitee is on, daily; its occurrence that day counts.
	if _, err := s.Create(ctx, owner, &Meeting{Title: "Standup", Category: CatInternal,
		StartsAt: at(-24 + 16), EndsAt: at(-24 + 16).Add(15 * time.Minute),
		Invitees:   []Invitee{{Email: invitee.Email}},
		Recurrence: &Recurrence{Freq: FreqDaily, Until: at(24 * 5)}}, "", ""); err != nil {
		t.Fatal(err)
	}

	busy, err := s.FreeBusy(ctx, stranger, []string{invitee.Email, " OWNER@example.com ", "nobody@example.com"}, at(0), at(24))
	if err != nil {
		t.Fatal(err)
	}
	inv := busy[invitee.Email]
	if len(inv) != 2 || !inv[0].From.Equal(at(9)) || !inv[0].To.Equal(at(10)) || !inv[1].From.Equal(at(16)) {
		t.Fatalf("invitee's spans: %+v (want 9-10 and the 16:00 standup; declined 11-12 left out)", inv)
	}
	if own := busy["owner@example.com"]; len(own) != 4 {
		t.Fatalf("owner's spans: %+v", own)
	}
	if nb, ok := busy["nobody@example.com"]; !ok || len(nb) != 0 {
		t.Fatalf("an unknown address answers an empty list, not an error: %v %v", nb, ok)
	}
	// Bounds: 20 people, 14 days, a window that is one.
	many := make([]string, 21)
	for i := range many {
		many[i] = "p" + string(rune('a'+i)) + "@example.com"
	}
	if _, err := s.FreeBusy(ctx, stranger, many, at(0), at(24)); !errors.Is(err, ErrBadTimeRange) {
		t.Fatalf("21 people: %v", err)
	}
	if _, err := s.FreeBusy(ctx, stranger, []string{invitee.Email}, at(0), at(24*15)); !errors.Is(err, ErrBadTimeRange) {
		t.Fatalf("15 days: %v", err)
	}
	if _, err := s.FreeBusy(ctx, stranger, []string{invitee.Email}, at(24), at(0)); !errors.Is(err, ErrBadTimeRange) {
		t.Fatalf("backwards window: %v", err)
	}
}
