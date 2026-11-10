package scheduling

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Recurring meetings.
//
// A series is one row: the first occurrence's start, a rule, and the end of
// the last occurrence stored where a single meeting keeps its end, so every
// query that asks "what overlaps this window" finds series rows with the
// same index and no second code path. Occurrences are computed on the way
// out, inside the window asked for, in the organiser's own time zone so a
// weekly 09:00 stays 09:00 across a clock change. Nothing is materialised:
// a two-year daily series is one row, not 730, and changing it is one write.
//
// What a single occurrence can do on its own is be cancelled: its start is
// recorded in the rule's skipped list. Everything else -- the title, the
// people, the time of day -- belongs to the series, which is also how the
// invitations work: an answer is to the series.

// Recurrence is the rule on a series.
type Recurrence struct {
	// Freq is daily, weekly or monthly.
	Freq string `json:"freq"`
	// Interval is "every N" days, weeks or months; 1 when omitted.
	Interval int `json:"interval,omitempty"`
	// Days are the weekdays of a weekly rule, 0 = Sunday; the first
	// occurrence's weekday when omitted.
	Days []int `json:"days,omitempty"`
	// Until is the last day an occurrence may start on, inclusive, read in
	// TimeZone. Required, and at most MaxHorizon ahead.
	Until time.Time `json:"until"`
	// TimeZone is an IANA name; occurrences keep their wall-clock time in it.
	// UTC when omitted.
	TimeZone string `json:"timeZone,omitempty"`
	// Skipped are occurrence starts (Unix milliseconds) cancelled on their own.
	Skipped []int64 `json:"skipped,omitempty"`
	// Duration of one occurrence, in minutes. Set from the first occurrence
	// when the series is made; stored because the row's end is the series'.
	Duration int `json:"duration"`
}

const (
	FreqDaily   = "daily"
	FreqWeekly  = "weekly"
	FreqMonthly = "monthly"

	maxInterval = 99
	maxSkipped  = 1000
	// maxOccurrences bounds one series in one answer, and the scan that
	// finds a series' last occurrence. A daily series over the two-year
	// horizon is 731.
	maxOccurrences = 800
)

var ErrBadRecurrence = errors.New("scheduling: the repeat rule is not valid")

// location resolves the rule's zone, UTC when none.
func (r *Recurrence) location() (*time.Location, error) {
	if r.TimeZone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(r.TimeZone)
}

// check validates and normalises the rule against the first occurrence.
func (r *Recurrence) check(first, firstEnd time.Time, now time.Time) error {
	switch r.Freq {
	case FreqDaily, FreqWeekly, FreqMonthly:
	default:
		return fmt.Errorf("%w: freq", ErrBadRecurrence)
	}
	if r.Interval == 0 {
		r.Interval = 1
	}
	if r.Interval < 1 || r.Interval > maxInterval {
		return fmt.Errorf("%w: interval", ErrBadRecurrence)
	}
	loc, err := r.location()
	if err != nil {
		return fmt.Errorf("%w: time zone", ErrBadRecurrence)
	}
	if r.Freq == FreqWeekly {
		if len(r.Days) == 0 {
			r.Days = []int{int(first.In(loc).Weekday())}
		}
		seen := map[int]bool{}
		var days []int
		for _, d := range r.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("%w: days", ErrBadRecurrence)
			}
			if !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		r.Days = days
		// The first occurrence is on one of the days, or the rule describes
		// a series that does not include the meeting it was made from.
		if !seen[int(first.In(loc).Weekday())] {
			return fmt.Errorf("%w: the first meeting must fall on one of the days", ErrBadRecurrence)
		}
	} else {
		r.Days = nil
	}
	if r.Until.IsZero() {
		return fmt.Errorf("%w: until", ErrBadRecurrence)
	}
	fl := first.In(loc)
	r.Until = endOfDay(r.Until.In(loc))
	if r.Until.Before(fl) {
		return fmt.Errorf("%w: until is before the first meeting", ErrBadRecurrence)
	}
	if r.Until.Sub(now) > MaxHorizon {
		return ErrTooFar
	}
	if len(r.Skipped) > maxSkipped {
		return fmt.Errorf("%w: skipped", ErrBadRecurrence)
	}
	r.Duration = int(firstEnd.Sub(first) / time.Minute)
	if r.Duration <= 0 {
		return ErrBadTimeRange
	}
	return nil
}

func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 23, 59, 59, 999_000_000, t.Location())
}

// occurrences lists the starts of the series from first, in order, that end
// after from and start before to, skipping cancelled ones; at most max.
func (r *Recurrence) occurrences(first time.Time, from, to time.Time, max int) []time.Time {
	loc, err := r.location()
	if err != nil {
		loc = time.UTC
	}
	dur := time.Duration(r.Duration) * time.Minute
	skipped := map[int64]bool{}
	for _, s := range r.Skipped {
		skipped[s] = true
	}
	var out []time.Time
	emit := func(t time.Time) bool { // false when past the end
		if t.After(r.Until) || (!to.IsZero() && !t.Before(to)) {
			return false
		}
		if skipped[t.UnixMilli()] || (!from.IsZero() && !t.Add(dur).After(from)) {
			return true
		}
		out = append(out, t.UTC())
		return len(out) < max
	}
	fl := first.In(loc)
	y, mo, d := fl.Date()
	h, mi, s := fl.Clock()
	ns := fl.Nanosecond()
	at := func(yy int, mm time.Month, dd int) time.Time {
		return time.Date(yy, mm, dd, h, mi, s, ns, loc)
	}
	switch r.Freq {
	case FreqDaily:
		for n := 0; n < maxOccurrences; n++ {
			if !emit(at(y, mo, d+n*r.Interval)) {
				return out
			}
		}
	case FreqWeekly:
		// From the Sunday of the first occurrence's week, every Interval
		// weeks, each chosen day; before the first occurrence nothing.
		week0 := d - int(fl.Weekday())
		for w := 0; w < maxOccurrences; w++ {
			for _, wd := range r.Days {
				t := at(y, mo, week0+w*7*r.Interval+wd)
				if t.Before(fl) {
					continue
				}
				if !emit(t) {
					return out
				}
			}
		}
	case FreqMonthly:
		// The same day of each month; a month without it (the 31st) gets
		// its last day.
		for n := 0; n < maxOccurrences; n++ {
			t := at(y, mo+time.Month(n*r.Interval), d)
			if t.Day() != d {
				t = at(t.Year(), t.Month(), 0) // the day before the 1st: last of the wanted month
			}
			if !emit(t) {
				return out
			}
		}
	}
	return out
}

// lastEnd is when the series' last occurrence ends: what the row stores as
// its end, so window queries see the whole series.
func (r *Recurrence) lastEnd(first time.Time) time.Time {
	all := r.occurrencesIgnoringSkips(first)
	if len(all) == 0 {
		return first.Add(time.Duration(r.Duration) * time.Minute)
	}
	return all[len(all)-1].Add(time.Duration(r.Duration) * time.Minute)
}

func (r *Recurrence) occurrencesIgnoringSkips(first time.Time) []time.Time {
	plain := *r
	plain.Skipped = nil
	return plain.occurrences(first, time.Time{}, time.Time{}, maxOccurrences)
}

// isOccurrence reports whether at is a start of this series (cancelled or
// not), so a cancellation names a real occurrence.
func (r *Recurrence) isOccurrence(first, at time.Time) bool {
	for _, t := range r.occurrencesIgnoringSkips(first) {
		if t.Equal(at) {
			return true
		}
	}
	return false
}

// expand replaces each series in ms with its occurrences inside [from, to),
// in start order, each a copy of the meeting with its own times and
// Occurrence set. Single meetings pass through. The series row itself is
// what ByID returns; a list is what the calendar draws.
func expand(ms []*Meeting, from, to time.Time) []*Meeting {
	var out []*Meeting
	for _, m := range ms {
		if m.Recurrence == nil {
			out = append(out, m)
			continue
		}
		dur := time.Duration(m.Recurrence.Duration) * time.Minute
		for _, t := range m.Recurrence.occurrences(m.StartsAt, from, to, maxOccurrences) {
			o := *m
			o.StartsAt = t
			o.EndsAt = t.Add(dur)
			o.Occurrence = t.UnixMilli()
			out = append(out, &o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.Before(out[j].StartsAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// firstOccurrence turns a series row as stored (end = series end) into what
// callers see (end = the first occurrence's end).
func (m *Meeting) firstOccurrence() {
	if m.Recurrence != nil {
		m.EndsAt = m.StartsAt.Add(time.Duration(m.Recurrence.Duration) * time.Minute)
	}
}

// storedEnd is what the row keeps as ends_at: the series' last end, or the
// meeting's own.
func (m *Meeting) storedEnd() time.Time {
	if m.Recurrence != nil {
		return m.Recurrence.lastEnd(m.StartsAt)
	}
	return m.EndsAt
}
