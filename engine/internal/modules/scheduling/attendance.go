package scheduling

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Attendance: who was actually in which meeting, and when.
//
// The calendar knows what was scheduled. It does not know what happened: an
// ad-hoc room somebody opened from the home page, a scheduled meeting that
// nobody joined, a participant who dropped after four minutes. "Recent" on the
// dashboard has to be built from what happened, so it is built from this --
// one row per participant per visit, written by the meeting gateway on join
// and closed on leave.
//
// Written by user account, not participant id: the participant id is minted
// per connection and is meaningless the moment the socket closes. A guest with
// no account leaves no row here; there is nothing durable to attribute it to,
// and a dashboard is for people who can sign in to see it.

// Attendance is one visit to one room by one account.
type Attendance struct {
	ID       string    `json:"id"`
	UserID   string    `json:"userId"`
	Room     string    `json:"room"`
	Name     string    `json:"name"` // display name used in the meeting
	Title    string    `json:"title"`
	JoinedAt time.Time `json:"joinedAt"`
	LeftAt   time.Time `json:"leftAt,omitempty"`
	// Seconds in the meeting; zero while still present.
	Seconds int64 `json:"seconds"`
}

// RecordJoin opens a visit. Returns the row id, which RecordLeave closes.
func (r *SQLRepository) RecordJoin(ctx context.Context, userID, room, name string, at time.Time) (string, error) {
	id := newID()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO scheduling_attendance (id, user_id, room, name, joined_at, left_at)
		VALUES (?,?,?,?,?,NULL)`,
		id, userID, room, name, at.UnixMilli())
	if err != nil {
		return "", fmt.Errorf("record join: %w", err)
	}
	return id, nil
}

// RecordLeave closes a visit. Closing an already-closed or unknown visit is
// not an error: a socket can be torn down twice on the way out, and the second
// time has nothing to say.
func (r *SQLRepository) RecordLeave(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduling_attendance SET left_at = ? WHERE id = ? AND left_at IS NULL`,
		at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("record leave: %w", err)
	}
	return nil
}

// History lists an account's visits, most recent first, with the meeting's
// title where the room was a scheduled one. An ad-hoc room has no title; the
// room id stands in, which is what the person saw in the address bar.
func (r *SQLRepository) History(ctx context.Context, userID string, limit int) ([]*Attendance, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.user_id, a.room, a.name, a.joined_at, a.left_at,
		       COALESCE((SELECT m.title FROM scheduling_meetings m WHERE m.room = a.room
		                 ORDER BY m.starts_at DESC LIMIT 1), '') AS title
		FROM scheduling_attendance a
		WHERE a.user_id = ?
		  AND a.room NOT IN (SELECT room FROM messaging_calls)
		ORDER BY a.joined_at DESC
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	defer rows.Close()

	var out []*Attendance
	for rows.Next() {
		var a Attendance
		var joined int64
		var left sql.NullInt64
		if err := rows.Scan(&a.ID, &a.UserID, &a.Room, &a.Name, &joined, &left, &a.Title); err != nil {
			return nil, err
		}
		a.JoinedAt = time.UnixMilli(joined).UTC()
		if left.Valid {
			a.LeftAt = time.UnixMilli(left.Int64).UTC()
			a.Seconds = (left.Int64 - joined) / 1000
		}
		if a.Title == "" {
			a.Title = a.Room
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// Service-level wrappers, so the gateway and the handler depend on the
// service and never on the repository.

// Joined records that an account entered a room and returns the visit id
// for Left. A guest (empty userID) is not recorded; there is no account to
// show it to later. A write failure is logged and yields an empty id: the
// meeting must not fail to start because the history table did.
func (s *Service) Joined(ctx context.Context, userID, room, name string) string {
	if userID == "" {
		return ""
	}
	id, err := s.repo.RecordJoin(ctx, userID, room, name, time.Now().UTC())
	if err != nil {
		s.log.Warn("attendance: could not record join", "user", userID, "room", room, "err", err)
		return ""
	}
	return id
}

// Left closes a visit opened by Joined. Safe with an empty id.
func (s *Service) Left(ctx context.Context, visitID string) {
	if visitID == "" {
		return
	}
	if err := s.repo.RecordLeave(ctx, visitID, time.Now().UTC()); err != nil &&
		!errors.Is(err, context.Canceled) {
		s.log.Warn("attendance: could not close visit", "id", visitID, "err", err)
	}
}

// History is the caller's own recent meetings.
func (s *Service) History(ctx context.Context, a Actor, limit int) ([]*Attendance, error) {
	return s.repo.History(ctx, a.ID, limit)
}
