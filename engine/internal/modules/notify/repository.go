package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// SQLRepository owns notify_subscriptions.
type SQLRepository struct{ db *database.DB }

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

func (r *SQLRepository) Upsert(ctx context.Context, s *Subscription) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notify_subscriptions (id, user_id, session_id, endpoint, p256dh, auth, created_at, fails)
		VALUES (?,?,?,?,?,?,?,0)
		ON CONFLICT(endpoint) DO UPDATE SET
		  user_id = excluded.user_id, session_id = excluded.session_id,
		  p256dh = excluded.p256dh, auth = excluded.auth, fails = 0`,
		s.ID, s.UserID, s.SessionID, s.Endpoint, s.P256dh, s.Auth, s.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("upsert subscription: %w", err)
	}
	return nil
}

func (r *SQLRepository) Delete(ctx context.Context, userID, endpoint string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM notify_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint)
	return err
}

func (r *SQLRepository) DeleteByID(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM notify_subscriptions WHERE id = ?`, id)
	return err
}

// ForUser joins the session so a revoked or expired sign-in's device stops
// hearing anything at the moment it stops being signed in, not when the
// session row is purged.
func (r *SQLRepository) ForUser(ctx context.Context, userID string, now time.Time) ([]*Subscription, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT n.id, n.user_id, n.session_id, n.endpoint, n.p256dh, n.auth, n.created_at, n.fails
		  FROM notify_subscriptions n
		  JOIN identity_sessions s ON s.id = n.session_id
		 WHERE n.user_id = ? AND s.revoked_at IS NULL AND s.expires_at > ?
		 ORDER BY n.created_at ASC LIMIT ?`, userID, now.UnixMilli(), MaxPerUser)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		var s Subscription
		var created int64
		if err := rows.Scan(&s.ID, &s.UserID, &s.SessionID, &s.Endpoint, &s.P256dh, &s.Auth, &created, &s.Fails); err != nil {
			return nil, err
		}
		s.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (r *SQLRepository) CountForUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_subscriptions WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func (r *SQLRepository) Failed(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM notify_subscriptions WHERE id = ? AND fails + 1 >= ?`, id, MaxFails)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	_, err = r.db.ExecContext(ctx, `UPDATE notify_subscriptions SET fails = fails + 1 WHERE id = ?`, id)
	return false, err
}

func (r *SQLRepository) Succeeded(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE notify_subscriptions SET fails = 0 WHERE id = ? AND fails != 0`, id)
	return err
}
