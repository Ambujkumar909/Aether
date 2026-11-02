package scheduling

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func readCSV(t *testing.T, b []byte) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(string(b)))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The formatting: one row per person however many times they rejoined,
// durations summed, the latest name, "In the meeting" for someone still
// there, the summary's counts, and Teams' column names.
func TestAttendanceCSVAggregates(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	visits := []Visit{
		{UserID: "a", Name: "Asha", JoinedAt: at(0), LeftAt: at(10)},
		{UserID: "b", Name: "Ben", JoinedAt: at(2), LeftAt: at(5)},
		{UserID: "a", Name: "Asha K", JoinedAt: at(12), LeftAt: at(30)}, // rejoined, renamed
		{UserID: "c", Name: "Cy", JoinedAt: at(20)},                     // still here
	}
	rows := readCSV(t, attendanceCSV("Weekly sync", t0, at(40), visits))
	find := func(k string) []string {
		for _, r := range rows {
			if r[0] == k {
				return r
			}
		}
		t.Fatalf("no row %q in %v", k, rows)
		return nil
	}
	if got := find("Meeting title"); got[1] != "Weekly sync" {
		t.Fatalf("title %v", got)
	}
	if got := find("Attended participants"); got[1] != "3" {
		t.Fatalf("attended %v", got)
	}
	if got := find("Meeting duration"); got[1] != "0:40:00" {
		t.Fatalf("meeting duration %v (Cy is still in: the end is now)", got)
	}
	// (28 + 3 + 20) min / 3 = 17 min
	if got := find("Average attendance time"); got[1] != "0:17:00" {
		t.Fatalf("average %v", got)
	}
	if got := find("Name"); strings.Join(got, "|") != "Name|First join|Last leave|In-meeting duration" {
		t.Fatalf("header %v", got)
	}
	asha := find("Asha K")
	if asha[1] != at(0).Format(time.RFC3339) || asha[2] != at(30).Format(time.RFC3339) || asha[3] != "0:28:00" {
		t.Fatalf("Asha %v: want first join 10:00, last leave 10:30, 28 min over two visits", asha)
	}
	if cy := find("Cy"); cy[2] != "In the meeting" || cy[3] != "0:20:00" {
		t.Fatalf("Cy %v", cy)
	}
}

// A display name is not allowed to become a spreadsheet formula.
func TestAttendanceCSVDefusesFormulas(t *testing.T) {
	t0 := time.Now().UTC()
	for _, evil := range []string{"=HYPERLINK(\"http://x\")", "+1+1", "-2+3", "@SUM(A1)"} {
		rows := readCSV(t, attendanceCSV("t", t0, t0.Add(time.Minute), []Visit{{UserID: "x", Name: evil, JoinedAt: t0, LeftAt: t0.Add(time.Minute)}}))
		last := rows[len(rows)-1]
		if last[0] != "'"+evil {
			t.Fatalf("%q exported as %q", evil, last[0])
		}
	}
}

// Who may download, decided by the meeting transport, not the caller; and
// only the meeting in progress, not the room's earlier sessions.
func TestAttendanceAccessAndScope(t *testing.T) {
	s := attendanceService(t)
	ctx := context.Background()
	// An earlier session in the same room, long over.
	old := s.Joined(ctx, "u-old", "meet-x", "Earlier person")
	s.Left(ctx, old)
	time.Sleep(5 * time.Millisecond)
	since := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	s.Joined(ctx, "u-host", "meet-x", "Host")
	s.Joined(ctx, "u-guestish", "meet-x", "Attendee")

	if _, err := s.AttendanceCSV(ctx, Actor{ID: "u-host"}, "meet-x", time.Now()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no transport hook: %v, want forbidden", err)
	}
	s.LiveHost = func(room, user string) (time.Time, bool) { return since, room == "meet-x" && user == "u-host" }

	for _, c := range []struct{ who, room string }{
		{"u-guestish", "meet-x"},                        // present, not a host
		{"", "meet-x"},                                  // no account
		{"u-host", "meet-y"},                            // a host, of another room
		{"u-host", strings.Repeat("r", MaxRoomIDLen+1)}, // one over the bound
	} {
		if _, err := s.AttendanceCSV(ctx, Actor{ID: c.who}, c.room, time.Now()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%q on %.20q: %v, want forbidden", c.who, c.room, err)
		}
	}
	b, err := s.AttendanceCSV(ctx, Actor{ID: "u-host"}, "meet-x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Earlier person") {
		t.Fatal("an earlier session's attendee is in this meeting's list")
	}
	if !strings.Contains(string(b), "Host") || !strings.Contains(string(b), "Attendee") {
		t.Fatalf("this meeting's people missing:\n%s", b)
	}
}

// Rule 7: the room query is an index lookup at scale, not a scan. 200,000
// visits across 2,000 rooms; one room's list must come back in milliseconds
// and the plan must name the index.
func TestAttendanceListAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	s := attendanceService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-48 * time.Hour).UnixMilli()
	for i := 0; i < 200000; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO scheduling_attendance (id,user_id,room,name,joined_at,left_at) VALUES (?,?,?,?,?,?)`,
			fmt.Sprint("v", i), fmt.Sprint("u", i%5000), fmt.Sprint("room-", i%2000), "n", base+int64(i), base+int64(i)+60000); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var plan string
	rows, err := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT user_id, name, joined_at, left_at FROM scheduling_attendance WHERE room = ? AND joined_at >= ? ORDER BY joined_at LIMIT ?`, "room-7", 0, MaxAttendanceRows)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b, c int
		var d string
		_ = rows.Scan(&a, &b, &c, &d)
		plan += d + "; "
	}
	rows.Close()
	if !strings.Contains(plan, "scheduling_attendance_room") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("not served by the index, or sorted afterwards: %s", plan)
	}
	var worst time.Duration
	for i := 0; i < 50; i++ {
		t0 := time.Now()
		v, err := repo.RoomAttendance(ctx, fmt.Sprint("room-", i*37%2000), time.UnixMilli(base), MaxAttendanceRows)
		if err != nil || len(v) != 100 {
			t.Fatalf("room %d: %d rows, %v", i, len(v), err)
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	t.Logf("plan: %s worst of 50 lookups over 200k rows: %v", plan, worst)
	if worst > 50*time.Millisecond {
		t.Fatalf("worst lookup %v", worst)
	}
}
