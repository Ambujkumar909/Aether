package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// SQLRepository implements Repository. It owns identity_users and
// identity_sessions and touches nothing else.
type SQLRepository struct {
	db *database.DB
}

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

const userCols = `id, email, display_name, password_hash, role, status,
	created_at, updated_at, last_login_at, failed_logins, locked_until`

func (r *SQLRepository) CreateUser(ctx context.Context, u *User) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO identity_users (`+userCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Email, u.DisplayName, u.passwordHash, string(u.Role), string(u.Status),
		u.CreatedAt.UnixMilli(), u.UpdatedAt.UnixMilli(),
		nullMillis(u.LastLoginAt), u.FailedLogins, nullMillis(u.LockedUntil))
	if err != nil {
		if isUnique(err) {
			return ErrEmailTaken
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *SQLRepository) UpdateUser(ctx context.Context, u *User) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE identity_users SET
			email=?, display_name=?, password_hash=?, role=?, status=?,
			updated_at=?, last_login_at=?, failed_logins=?, locked_until=?
		WHERE id=?`,
		u.Email, u.DisplayName, u.passwordHash, string(u.Role), string(u.Status),
		u.UpdatedAt.UnixMilli(), nullMillis(u.LastLoginAt),
		u.FailedLogins, nullMillis(u.LockedUntil), u.ID)
	if err != nil {
		if isUnique(err) {
			return ErrEmailTaken
		}
		return fmt.Errorf("update user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLRepository) DeleteUser(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM identity_users WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLRepository) UserByID(ctx context.Context, id string) (*User, error) {
	return r.oneUser(ctx, `SELECT `+userCols+` FROM identity_users WHERE id=?`, id)
}

func (r *SQLRepository) UserByEmail(ctx context.Context, email string) (*User, error) {
	// The column is COLLATE NOCASE, so this comparison is case-insensitive
	// in the same way the unique index is. Doing it any other way would let
	// two accounts exist that the login path cannot tell apart.
	return r.oneUser(ctx, `SELECT `+userCols+` FROM identity_users WHERE email=?`, email)
}

func (r *SQLRepository) oneUser(ctx context.Context, q string, args ...any) (*User, error) {
	row := r.db.QueryRowContext(ctx, q, args...)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func userWhere(f UserFilter) (string, []any) {
	var cond []string
	var args []any
	if f.Search != "" {
		needle := "%" + escapeLike(f.Search) + "%"
		cond = append(cond,
			`(email LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\')`)
		args = append(args, needle, needle)
	}
	if f.Role != "" {
		cond = append(cond, "role = ?")
		args = append(args, string(f.Role))
	}
	if f.Status != "" {
		cond = append(cond, "status = ?")
		args = append(args, string(f.Status))
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

func (r *SQLRepository) ListUsers(ctx context.Context, f UserFilter) ([]*User, error) {
	clause, args := userWhere(f)
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM identity_users`+clause+
			` ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *SQLRepository) CountUsers(ctx context.Context, f UserFilter) (int, error) {
	clause, args := userWhere(f)
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM identity_users`+clause, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// SearchDirectory finds active accounts by name or address.
//
// Suspended accounts are excluded: somebody who has been shut out should not
// still be offered as a person to start a conversation with.
// FoundingAdminEmail returns the oldest administrator's address.
func (r *SQLRepository) FoundingAdminEmail(ctx context.Context) (string, error) {
	var email string
	err := r.db.QueryRowContext(ctx,
		`SELECT email FROM identity_users
		  WHERE role = ? AND status = ?
		  ORDER BY created_at ASC, rowid ASC LIMIT 1`,
		string(RoleAdmin), string(StatusActive)).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("founding admin: %w", err)
	}
	return email, nil
}

// DirectoryByDomains is the organisation's address book: active accounts
// whose address is in one of the declared domains, narrowed by what was
// typed. See directory_query.go for how it stays an index lookup.
func (r *SQLRepository) DirectoryByDomains(ctx context.Context, domains []string, q string, limit int) ([]DirectoryEntry, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	if len(domains) > 20 {
		domains = domains[:20]
	}
	lower := make([]string, len(domains))
	for i, d := range domains {
		lower[i] = strings.ToLower(d)
	}
	holes, args := inList(lower)
	return r.directorySearch(ctx, ` AND u.email_domain IN (`+holes+`)`, args, q, limit)
}

// DirectoryByIDs is the contacts view: a bounded set of accounts, narrowed
// by what was typed.
func (r *SQLRepository) DirectoryByIDs(ctx context.Context, ids []string, q string, limit int) ([]DirectoryEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// A bound on the IN list, so a person with thousands of conversations
	// cannot turn one keystroke into an unbounded query.
	if len(ids) > 500 {
		ids = ids[:500]
	}
	holes, args := inList(ids)
	return r.directorySearch(ctx, ` AND u.id IN (`+holes+`)`, args, q, limit)
}

// SearchDirectory is the published address book: every active account,
// narrowed by what was typed. Only reachable when an administrator has set
// the directory scope to "everyone".
func (r *SQLRepository) SearchDirectory(ctx context.Context, q string, limit int) ([]DirectoryEntry, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if cleanQuery(q) == "" {
		// An empty query lists the most recent accounts rather than nothing,
		// so a fresh install still shows the two people who are on it.
		return r.directoryRows(ctx,
			`SELECT id, display_name, email FROM identity_users
			 WHERE status = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
			string(StatusActive), limit)
	}
	return r.directorySearch(ctx, "", nil, q, limit)
}

func (r *SQLRepository) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM identity_users WHERE role=? AND status=?`,
		string(RoleAdmin), string(StatusActive)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(s rowScanner) (*User, error) {
	var u User
	var role, status string
	var created, updated int64
	var lastLogin, lockedUntil sql.NullInt64

	err := s.Scan(&u.ID, &u.Email, &u.DisplayName, &u.passwordHash, &role, &status,
		&created, &updated, &lastLogin, &u.FailedLogins, &lockedUntil)
	if err != nil {
		return nil, err
	}
	u.Role = Role(role)
	u.Status = Status(status)
	u.CreatedAt = time.UnixMilli(created).UTC()
	u.UpdatedAt = time.UnixMilli(updated).UTC()
	if lastLogin.Valid {
		t := time.UnixMilli(lastLogin.Int64).UTC()
		u.LastLoginAt = &t
	}
	if lockedUntil.Valid {
		t := time.UnixMilli(lockedUntil.Int64).UTC()
		u.LockedUntil = &t
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessCols = `id, token_hash, user_id, created_at, expires_at, last_seen,
	ip, user_agent, revoked_at`

func (r *SQLRepository) CreateSession(ctx context.Context, s *Session) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO identity_sessions (`+sessCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ID, s.tokenHash, s.UserID, s.CreatedAt.UnixMilli(),
		s.ExpiresAt.UnixMilli(), s.LastSeen.UnixMilli(),
		s.IP, s.UserAgent, nullMillis(s.RevokedAt))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *SQLRepository) SessionByTokenHash(ctx context.Context, hash string) (*Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sessCols+` FROM identity_sessions WHERE token_hash=?`, hash)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *SQLRepository) TouchSession(ctx context.Context, id string, seen time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE identity_sessions SET last_seen=? WHERE id=?`, seen.UnixMilli(), id)
	return err
}

func (r *SQLRepository) RevokeSession(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE identity_sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
		at.UnixMilli(), id)
	return err
}

func (r *SQLRepository) RevokeUserSessions(ctx context.Context, userID string, at time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE identity_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`,
		at.UnixMilli(), userID)
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *SQLRepository) ListSessions(ctx context.Context, userID string) ([]*Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessCols+` FROM identity_sessions WHERE user_id=?
		 ORDER BY created_at DESC, rowid DESC LIMIT 100`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PurgeExpiredSessions removes rows that can no longer authenticate anything.
// Revoked rows are kept for a grace period so that "who was signed in when"
// remains answerable shortly after an incident.
func (r *SQLRepository) PurgeExpiredSessions(ctx context.Context, before time.Time) (int, error) {
	cutoff := before.Add(-7 * 24 * time.Hour).UnixMilli()
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM identity_sessions WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM identity_password_resets WHERE expires_at < ?`, before.UnixMilli()); err != nil {
		return 0, fmt.Errorf("purge password resets: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanSession(sc rowScanner) (*Session, error) {
	var s Session
	var created, expires, seen int64
	var revoked sql.NullInt64
	err := sc.Scan(&s.ID, &s.tokenHash, &s.UserID, &created, &expires, &seen,
		&s.IP, &s.UserAgent, &revoked)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	s.ExpiresAt = time.UnixMilli(expires).UTC()
	s.LastSeen = time.UnixMilli(seen).UTC()
	if revoked.Valid {
		t := time.UnixMilli(revoked.Int64).UTC()
		s.RevokedAt = &t
	}
	return &s, nil
}

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
