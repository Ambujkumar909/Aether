package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// SQLRepository is the SQL implementation of Repository.
//
// It owns the audit_events table and nothing else, per the module contract.
type SQLRepository struct {
	db *database.DB
}

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

// Append inserts one event and reads back the sequence the database assigned.
func (r *SQLRepository) Append(ctx context.Context, e *Event) error {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_events
			(id, at, actor_id, actor_email, action, subject, outcome,
			 ip, user_agent, detail, prev_hash, hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.At.UTC().UnixMilli(), e.ActorID, e.ActorEmail, string(e.Action),
		e.Subject, string(e.Outcome), e.IP, e.UserAgent, e.Detail,
		e.PrevHash, e.Hash)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("read audit sequence: %w", err)
	}
	e.Seq = seq
	return nil
}

// where builds the filter clause shared by List and Count, so the two can
// never disagree about what matches -- a paginated view whose total is
// computed by different logic than its rows is a bug waiting for a support
// ticket.
func where(f Filter) (string, []any) {
	var cond []string
	var args []any

	if f.ActorID != "" {
		cond = append(cond, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.Action != "" {
		cond = append(cond, "action = ?")
		args = append(args, string(f.Action))
	}
	if f.Outcome != "" {
		cond = append(cond, "outcome = ?")
		args = append(args, string(f.Outcome))
	}
	if !f.Since.IsZero() {
		cond = append(cond, "at >= ?")
		args = append(args, f.Since.UTC().UnixMilli())
	}
	if !f.Until.IsZero() {
		cond = append(cond, "at <= ?")
		args = append(args, f.Until.UTC().UnixMilli())
	}
	if f.Search != "" {
		// Parameterised LIKE. The wildcards are ours; the needle is bound,
		// so a search for "%" matches a literal percent sign rather than
		// everything.
		needle := "%" + escapeLike(f.Search) + "%"
		cond = append(cond,
			"(actor_email LIKE ? ESCAPE '\\' OR subject LIKE ? ESCAPE '\\' "+
				"OR detail LIKE ? ESCAPE '\\' OR ip LIKE ? ESCAPE '\\')")
		args = append(args, needle, needle, needle, needle)
	}
	if f.Before > 0 {
		cond = append(cond, "seq < ?")
		args = append(args, f.Before)
	}
	if len(cond) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(cond, " AND "), args
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

func (r *SQLRepository) List(ctx context.Context, f Filter) ([]Event, error) {
	clause, args := where(f)
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT seq, id, at, actor_id, actor_email, action, subject, outcome,
	             ip, user_agent, detail, prev_hash, hash
	      FROM audit_events` + clause + ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *SQLRepository) Count(ctx context.Context, f Filter) (int64, error) {
	// Pagination cursor is not a filter for counting purposes.
	f.Before = 0
	clause, args := where(f)
	var n int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_events`+clause, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count audit events: %w", err)
	}
	return n, nil
}

func (r *SQLRepository) Head(ctx context.Context) (int64, string, error) {
	var seq sql.NullInt64
	var hash sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1`).
		Scan(&seq, &hash)
	if err == sql.ErrNoRows {
		return 0, GenesisHash, nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("read audit head: %w", err)
	}
	return seq.Int64, hash.String, nil
}

// Walk streams every event in order. Verification of a long-lived log must
// not require holding it all in memory.
func (r *SQLRepository) Walk(ctx context.Context, from int64, fn func(Event) error) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, id, at, actor_id, actor_email, action, subject, outcome,
		       ip, user_agent, detail, prev_hash, hash
		FROM audit_events WHERE seq > ? ORDER BY seq ASC`, from)
	if err != nil {
		return fmt.Errorf("walk audit events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(s scanner) (Event, error) {
	var e Event
	var at int64
	var action, outcome string
	err := s.Scan(&e.Seq, &e.ID, &at, &e.ActorID, &e.ActorEmail, &action,
		&e.Subject, &outcome, &e.IP, &e.UserAgent, &e.Detail,
		&e.PrevHash, &e.Hash)
	if err != nil {
		return e, fmt.Errorf("scan audit event: %w", err)
	}
	e.At = time.UnixMilli(at).UTC()
	e.Action = Action(action)
	e.Outcome = Outcome(outcome)
	return e, nil
}
