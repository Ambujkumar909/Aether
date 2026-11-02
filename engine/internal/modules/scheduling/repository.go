package scheduling

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// SQLRepository owns scheduling_meetings and scheduling_invitees.
type SQLRepository struct {
	db *database.DB
}

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

const cols = `id, room, title, description, category, status,
	starts_at, ends_at, organizer_id, organizer_email, created_at, updated_at,
	admission`

// Create writes the meeting and its invitees in one transaction. A meeting
// that exists without its invitee rows is a meeting nobody was asked to,
// which is worse than no meeting at all.
func (r *SQLRepository) Create(ctx context.Context, m *Meeting) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO scheduling_meetings (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Room, m.Title, m.Description, string(m.Category), string(m.Status),
		m.StartsAt.UnixMilli(), m.EndsAt.UnixMilli(),
		m.OrganizerID, m.OrganizerEmail,
		m.CreatedAt.UnixMilli(), m.UpdatedAt.UnixMilli(),
		NormaliseAdmission(m.Admission)); err != nil {
		return fmt.Errorf("insert meeting: %w", err)
	}
	if err := replaceInvitees(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLRepository) Update(ctx context.Context, m *Meeting) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE scheduling_meetings SET
			room=?, title=?, description=?, category=?, status=?,
			starts_at=?, ends_at=?, updated_at=?, admission=?
		WHERE id=?`,
		m.Room, m.Title, m.Description, string(m.Category), string(m.Status),
		m.StartsAt.UnixMilli(), m.EndsAt.UnixMilli(),
		m.UpdatedAt.UnixMilli(), NormaliseAdmission(m.Admission), m.ID)
	if err != nil {
		return fmt.Errorf("update meeting: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if m.Invitees != nil {
		if err := replaceInvitees(ctx, tx, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func replaceInvitees(ctx context.Context, tx *sql.Tx, m *Meeting) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scheduling_invitees WHERE meeting_id=?`, m.ID); err != nil {
		return fmt.Errorf("clear invitees: %w", err)
	}
	for _, inv := range m.Invitees {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scheduling_invitees (meeting_id, email, user_id, response, co_organizer)
			VALUES (?,?,?,?,?)`,
			m.ID, inv.Email, inv.UserID, string(inv.Response), inv.CoOrganizer); err != nil {
			return fmt.Errorf("insert invitee: %w", err)
		}
	}
	return nil
}

func (r *SQLRepository) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM scheduling_meetings WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete meeting: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLRepository) ByID(ctx context.Context, id string) (*Meeting, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+cols+` FROM scheduling_meetings WHERE id=?`, id)
	m, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := r.loadInvitees(ctx, []*Meeting{m}); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *SQLRepository) List(ctx context.Context, f Filter) ([]*Meeting, error) {
	var cond []string
	var args []any

	if f.Participant != "" {
		// Organiser or invitee. The subquery keeps this one round trip
		// rather than a join that would duplicate meeting rows per invitee.
		cond = append(cond,
			`(organizer_email = ? OR id IN (SELECT meeting_id FROM scheduling_invitees WHERE email = ?))`)
		args = append(args, strings.ToLower(f.Participant), strings.ToLower(f.Participant))
	}
	if !f.From.IsZero() {
		cond = append(cond, "ends_at >= ?")
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		cond = append(cond, "starts_at <= ?")
		args = append(args, f.To.UnixMilli())
	}
	if f.Status != "" {
		cond = append(cond, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Category != "" {
		cond = append(cond, "category = ?")
		args = append(args, string(f.Category))
	}

	q := `SELECT ` + cols + ` FROM scheduling_meetings`
	if len(cond) > 0 {
		q += " WHERE " + strings.Join(cond, " AND ")
	}
	q += " ORDER BY starts_at ASC LIMIT ?"
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}
	defer rows.Close()

	var out []*Meeting
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.loadInvitees(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadInvitees fills invitee lists for a page of meetings in one query,
// rather than one query per meeting.
func (r *SQLRepository) loadInvitees(ctx context.Context, ms []*Meeting) error {
	if len(ms) == 0 {
		return nil
	}
	byID := make(map[string]*Meeting, len(ms))
	ph := make([]string, 0, len(ms))
	args := make([]any, 0, len(ms))
	for _, m := range ms {
		byID[m.ID] = m
		ph = append(ph, "?")
		args = append(args, m.ID)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT meeting_id, email, user_id, response, co_organizer FROM scheduling_invitees
		 WHERE meeting_id IN (`+strings.Join(ph, ",")+`) ORDER BY rowid`, args...)
	if err != nil {
		return fmt.Errorf("load invitees: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var mid, email, uid, resp string
		var co bool
		if err := rows.Scan(&mid, &email, &uid, &resp, &co); err != nil {
			return err
		}
		if m := byID[mid]; m != nil {
			m.Invitees = append(m.Invitees, Invitee{
				Email: email, UserID: uid, Response: Response(resp), CoOrganizer: co,
			})
		}
	}
	return rows.Err()
}

func (r *SQLRepository) SetResponse(ctx context.Context, meetingID, email string, resp Response) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE scheduling_invitees SET response=? WHERE meeting_id=? AND email=?`,
		string(resp), meetingID, strings.ToLower(email))
	if err != nil {
		return fmt.Errorf("set response: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Conflicts finds the user's overlapping, still-scheduled meetings.
//
// Two intervals overlap when each starts before the other ends. Written that
// way rather than with a pile of cases, because the pile of cases is where
// off-by-one errors in calendar code invariably live.
func (r *SQLRepository) Conflicts(ctx context.Context, email string, from, to time.Time, exclude string) ([]*Meeting, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+cols+` FROM scheduling_meetings
		WHERE status = ?
		  AND id != ?
		  AND starts_at < ?
		  AND ends_at > ?
		  AND (organizer_email = ? OR id IN
		       (SELECT meeting_id FROM scheduling_invitees WHERE email = ?))
		ORDER BY starts_at ASC LIMIT 20`,
		string(StatusScheduled), exclude,
		to.UnixMilli(), from.UnixMilli(),
		strings.ToLower(email), strings.ToLower(email))
	if err != nil {
		return nil, fmt.Errorf("conflicts: %w", err)
	}
	defer rows.Close()

	var out []*Meeting
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(s rowScanner) (*Meeting, error) {
	var m Meeting
	var cat, status string
	var starts, ends, created, updated int64
	var admission string
	err := s.Scan(&m.ID, &m.Room, &m.Title, &m.Description, &cat, &status,
		&starts, &ends, &m.OrganizerID, &m.OrganizerEmail, &created, &updated,
		&admission)
	if err != nil {
		return nil, err
	}
	// Normalised on the way out as well as in. A row written by an older
	// version, or edited by hand, must not be able to open a meeting.
	m.Admission = NormaliseAdmission(admission)
	m.Category = Category(cat)
	m.Status = Status(status)
	m.StartsAt = time.UnixMilli(starts).UTC()
	m.EndsAt = time.UnixMilli(ends).UTC()
	m.CreatedAt = time.UnixMilli(created).UTC()
	m.UpdatedAt = time.UnixMilli(updated).UTC()
	return &m, nil
}

// ByRoom finds the meeting a room link belongs to.
//
// A room may be reused across a recurring series, so the most recently
// scheduled one wins: that is the meeting somebody following the link now is
// joining.
// IsInvited is the door check for a scheduled room: organiser or invitee,
// matched on account id or on email, case-insensitively because an address
// typed into an invite is not always the address stored on the account.
//
// A cancelled meeting invites nobody. Everything else about the meeting --
// title, description, who else is coming -- stays behind this boolean.
// IsCoOrganizer: is this account a co-organizer of the meeting in room?
// Index-backed by the room's meeting and the invitees' primary key.
func (r *SQLRepository) IsCoOrganizer(ctx context.Context, room, userID, email string) (bool, error) {
	if room == "" || (userID == "" && email == "") {
		return false, nil
	}
	var one int
	err := r.db.QueryRowContext(ctx, `
		SELECT 1 FROM scheduling_meetings m
		JOIN scheduling_invitees i ON i.meeting_id = m.id
		WHERE m.room = ? AND m.status != ? AND i.co_organizer = 1
		  AND ((? != '' AND i.user_id = ?) OR (? != '' AND lower(i.email) = lower(?)))
		LIMIT 1`,
		room, string(StatusCancelled), userID, userID, email, email).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (r *SQLRepository) IsInvited(ctx context.Context, room, userID, email string) (bool, error) {
	if room == "" || (userID == "" && email == "") {
		return false, nil
	}
	var one int
	err := r.db.QueryRowContext(ctx, `
		SELECT 1 FROM scheduling_meetings m
		WHERE m.room = ? AND m.status != ?
		  AND (
		        (? != '' AND m.organizer_id = ?)
		     OR (? != '' AND lower(m.organizer_email) = lower(?))
		     OR EXISTS (SELECT 1 FROM scheduling_invitees i
		                WHERE i.meeting_id = m.id
		                  AND ((? != '' AND i.user_id = ?)
		                    OR (? != '' AND lower(i.email) = lower(?))))
		  )
		LIMIT 1`,
		room, string(StatusCancelled),
		userID, userID, email, email, userID, userID, email, email).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("invited: %w", err)
	}
	return true, nil
}

// CompleteRoom closes the meeting behind a room. Only a scheduled meeting
// that has already started is closed: people who gather early and disperse
// have not held the meeting, and a cancelled one stays cancelled.
func (r *SQLRepository) CompleteRoom(ctx context.Context, room string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduling_meetings
		   SET status = ?, updated_at = ?
		 WHERE room = ? AND status = ? AND starts_at <= ?`,
		string(StatusCompleted), at.UnixMilli(), room,
		string(StatusScheduled), at.UnixMilli())
	if err != nil {
		return fmt.Errorf("complete room: %w", err)
	}
	return nil
}

func (r *SQLRepository) ByRoom(ctx context.Context, room string) (*Meeting, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM scheduling_meetings
		 WHERE room = ? ORDER BY starts_at DESC LIMIT 1`, room)
	m, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}
