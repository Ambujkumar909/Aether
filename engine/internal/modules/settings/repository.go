package settings

import (
	"context"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

type SQLRepository struct{ db *database.DB }

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

func (r *SQLRepository) All(ctx context.Context) (map[Key]Setting, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value, updated_at, updated_by FROM org_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[Key]Setting{}
	for rows.Next() {
		var s Setting
		var k string
		var updated int64
		if err := rows.Scan(&k, &s.Value, &updated, &s.UpdatedBy); err != nil {
			return nil, err
		}
		s.Key = Key(k)
		s.UpdatedAt = time.UnixMilli(updated).UTC()
		// Anything not in the catalogue is ignored rather than returned. A row
		// left behind by an older version, or written by hand, must not become
		// a setting this version half-honours.
		if _, ok := specFor(s.Key); ok {
			out[s.Key] = s
		}
	}
	return out, rows.Err()
}

func (r *SQLRepository) Put(ctx context.Context, s Setting) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO org_settings (key, value, updated_at, updated_by)
		VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		string(s.Key), s.Value, s.UpdatedAt.UnixMilli(), s.UpdatedBy)
	return err
}
