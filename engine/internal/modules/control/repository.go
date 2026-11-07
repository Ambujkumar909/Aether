package control

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

type SQLRepository struct{ db *database.DB }

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

const sessionCols = `id, meeting_id, controller_id, controller_email, controller_name,
	subject_id, subject_email, subject_name, scope, state,
	requested_at, decided_at, expires_at, ended_at, ended_by, reason`

func ms(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func (r *SQLRepository) Create(ctx context.Context, s *Session) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO control_sessions (`+sessionCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.MeetingID, s.ControllerID, s.ControllerEmail, s.ControllerName,
		s.SubjectID, s.SubjectEmail, s.SubjectName, string(s.Scope), string(s.State),
		s.RequestedAt.UnixMilli(), ms(s.DecidedAt), ms(s.ExpiresAt), ms(s.EndedAt),
		s.EndedBy, s.Reason)
	return err
}

func (r *SQLRepository) Update(ctx context.Context, s *Session) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE control_sessions SET
			scope = ?, state = ?, decided_at = ?, expires_at = ?,
			ended_at = ?, ended_by = ?, reason = ?
		WHERE id = ?`,
		string(s.Scope), string(s.State), ms(s.DecidedAt), ms(s.ExpiresAt),
		ms(s.EndedAt), s.EndedBy, s.Reason, s.ID)
	return err
}

func scan(rows interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var scope, state string
	var requested int64
	var decided, expires, ended sql.NullInt64
	if err := rows.Scan(&s.ID, &s.MeetingID, &s.ControllerID, &s.ControllerEmail,
		&s.ControllerName, &s.SubjectID, &s.SubjectEmail, &s.SubjectName,
		&scope, &state, &requested, &decided, &expires, &ended,
		&s.EndedBy, &s.Reason); err != nil {
		return nil, err
	}
	s.Scope = Scope(scope)
	s.State = State(state)
	s.RequestedAt = time.UnixMilli(requested).UTC()
	s.DecidedAt = fromMs(decided)
	s.ExpiresAt = fromMs(expires)
	s.EndedAt = fromMs(ended)
	return &s, nil
}

func (r *SQLRepository) ByID(ctx context.Context, id string) (*Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM control_sessions WHERE id = ?`, id)
	s, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func (r *SQLRepository) LiveForSubject(ctx context.Context, subjectID string, now time.Time) (*Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM control_sessions
		 WHERE subject_id = ? AND state = ? AND expires_at > ?
		 ORDER BY requested_at DESC, rowid DESC LIMIT 1`,
		subjectID, string(StateGranted), now.UnixMilli())
	s, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func (r *SQLRepository) PendingFor(ctx context.Context, subjectID string) ([]*Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM control_sessions
		 WHERE subject_id = ? AND state = ?
		 ORDER BY requested_at DESC, rowid DESC`,
		subjectID, string(StateRequested))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

func (r *SQLRepository) ForMeeting(ctx context.Context, meetingID string, limit int) ([]*Session, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM control_sessions
		 WHERE meeting_id = ? ORDER BY requested_at DESC, rowid DESC LIMIT ?`,
		meetingID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// ExpireOverdue flips granted sessions whose deadline has passed.
//
// Selected before updating rather than after, so the caller receives exactly
// the sessions this call expired. Doing it the other way round loses the race
// against a concurrent revoke and would audit an expiry for a session the user
// had already stopped.
func (r *SQLRepository) ExpireOverdue(ctx context.Context, now time.Time) ([]*Session, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM control_sessions
		 WHERE state = ? AND expires_at IS NOT NULL AND expires_at <= ?`,
		string(StateGranted), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	due, err := collect(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, tx.Commit()
	}

	for _, s := range due {
		end := now.UTC()
		s.State = StateExpired
		s.EndedAt = &end
		s.Reason = "grant expired"
		if _, err := tx.ExecContext(ctx,
			`UPDATE control_sessions SET state = ?, ended_at = ?, reason = ?
			 WHERE id = ? AND state = ?`,
			string(StateExpired), end.UnixMilli(), s.Reason, s.ID, string(StateGranted)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return due, nil
}

func collect(rows *sql.Rows) ([]*Session, error) {
	var out []*Session
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
