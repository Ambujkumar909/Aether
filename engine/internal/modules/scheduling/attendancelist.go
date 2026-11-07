package scheduling

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
	"time"
)

// "Download attendance list": Teams gives organizers and co-organizers a CSV
// of who came to the meeting in progress. Its columns, from the package's
// strings: a Summary (Meeting title, Attended participants, Start time, End
// time, Meeting duration, Average attendance time) and Participants (Name,
// First join, Last leave, In-meeting duration). Teams also lists each
// person's email (UPN); Aether does not, because its directory never hands
// out a list of accounts' addresses (ARCHITECTURE §6l).
//
// Guests have no account and leave no attendance row (attendance.go), so they
// are not in the list; the summary says so when it matters.

// MaxAttendanceRows bounds one export. A meeting is capped at 500 people
// (room.MaxParticipants); 5,000 visits is ten rejoins each.
const MaxAttendanceRows = 5000

// MaxRoomIDLen bounds the room parameter before it reaches a query.
const MaxRoomIDLen = 128

// Visit is one join-leave of one account, for the list.
type Visit struct {
	UserID   string
	Name     string
	JoinedAt time.Time
	LeftAt   time.Time // zero while still present
}

// RoomAttendance lists visits to a room since a moment, oldest first.
// Index-backed: scheduling_attendance_room (room, joined_at).
func (r *SQLRepository) RoomAttendance(ctx context.Context, room string, since time.Time, limit int) ([]Visit, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT user_id, name, joined_at, left_at
		FROM scheduling_attendance
		WHERE room = ? AND joined_at >= ?
		ORDER BY joined_at, rowid
		LIMIT ?`, room, since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("room attendance: %w", err)
	}
	defer rows.Close()
	var out []Visit
	for rows.Next() {
		var v Visit
		var joined int64
		var left sql.NullInt64
		if err := rows.Scan(&v.UserID, &v.Name, &joined, &left); err != nil {
			return nil, err
		}
		v.JoinedAt = time.UnixMilli(joined).UTC()
		if left.Valid {
			v.LeftAt = time.UnixMilli(left.Int64).UTC()
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AttendanceCSV builds the list for the meeting in progress in room. Only an
// organizer or co-organizer of that meeting, present now, may have it: the
// meeting transport answers that (LiveHost), not anything the caller says.
func (s *Service) AttendanceCSV(ctx context.Context, a Actor, room string, now time.Time) ([]byte, error) {
	if room == "" || len(room) > MaxRoomIDLen || a.ID == "" {
		return nil, ErrForbidden
	}
	if s.LiveHost == nil {
		return nil, ErrForbidden
	}
	since, ok := s.LiveHost(room, a.ID)
	if !ok {
		return nil, ErrForbidden
	}
	visits, err := s.repo.RoomAttendance(ctx, room, since, MaxAttendanceRows)
	if err != nil {
		return nil, err
	}
	return attendanceCSV(s.TitleForRoom(ctx, room), since, now, visits), nil
}

// attendanceCSV is the formatting, separate so it can be tested without a
// database. Times are UTC in RFC 3339; durations are h:mm:ss.
func attendanceCSV(title string, start, now time.Time, visits []Visit) []byte {
	type person struct {
		name        string
		first, last time.Time
		secs        int64
		present     bool
	}
	by := map[string]*person{}
	var order []string
	end := start
	for _, v := range visits {
		left := v.LeftAt
		open := left.IsZero()
		if open {
			left = now
		}
		p := by[v.UserID]
		if p == nil {
			p = &person{name: v.Name, first: v.JoinedAt}
			by[v.UserID] = p
			order = append(order, v.UserID)
		}
		if v.Name != "" {
			p.name = v.Name // the latest name they used
		}
		if v.JoinedAt.Before(p.first) {
			p.first = v.JoinedAt
		}
		if left.After(p.last) {
			p.last = left
		}
		if d := left.Sub(v.JoinedAt); d > 0 {
			p.secs += int64(d / time.Second)
		}
		p.present = p.present || open
		if left.After(end) {
			end = left
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]].first.Before(by[order[j]].first) })

	var total int64
	for _, id := range order {
		total += by[id].secs
	}
	avg := int64(0)
	if len(order) > 0 {
		avg = total / int64(len(order))
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	row := func(cells ...string) {
		for i, c := range cells {
			cells[i] = cellSafe(c)
		}
		_ = w.Write(cells)
	}
	row("1. Summary")
	row("Meeting title", title)
	row("Attended participants", fmt.Sprint(len(order)))
	row("Start time", start.UTC().Format(time.RFC3339))
	row("End time", end.UTC().Format(time.RFC3339))
	row("Meeting duration", hms(int64(end.Sub(start)/time.Second)))
	row("Average attendance time", hms(avg))
	row("")
	row("2. Participants")
	row("Name", "First join", "Last leave", "In-meeting duration")
	for _, id := range order {
		p := by[id]
		lastLeave := p.last.UTC().Format(time.RFC3339)
		if p.present {
			lastLeave = "In the meeting"
		}
		row(p.name, p.first.UTC().Format(time.RFC3339), lastLeave, hms(p.secs))
	}
	w.Flush()
	return buf.Bytes()
}

func hms(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d:%02d", secs/3600, secs/60%60, secs%60)
}

// cellSafe keeps a display name from becoming a spreadsheet formula when the
// file is opened (CSV injection): a cell starting with = + - @ or a control
// character is prefixed with an apostrophe, which spreadsheets show as text.
func cellSafe(c string) string {
	if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
		return "'" + c
	}
	return c
}
