package messaging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reactions, quoted replies and saved messages.
//
// All three share one property that shapes the code: they are read for a page
// of messages at a time, never for one message. A conversation opens with
// fifty messages, and doing a query per message for each of three features is
// a hundred and fifty round trips to answer one screen. Each of these loads a
// whole page in a single statement keyed on the message ids, and the service
// stitches them on.

// React adds or removes one emoji from one person on one message.
//
// Idempotent in both directions. The same emoji twice is the same fact, and
// removing one that is not there is not an error: two clicks racing each other
// must not produce a failure somebody has to make sense of.
func (r *SQLRepository) React(ctx context.Context, messageID, userID, emoji string, on bool) (bool, error) {
	if messageID == "" || userID == "" || emoji == "" {
		return false, ErrNotFound
	}
	if !on {
		_, err := r.db.ExecContext(ctx,
			`DELETE FROM messaging_reactions
			  WHERE message_id=? AND user_id=? AND emoji=?`,
			messageID, userID, emoji)
		if err != nil {
			return false, fmt.Errorf("remove reaction: %w", err)
		}
		return false, nil
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_reactions
		   (message_id, user_id, emoji, created_at) VALUES (?,?,?,?)`,
		messageID, userID, emoji, time.Now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("add reaction: %w", err)
	}
	return true, nil
}

// ReactionsFor groups every reaction on a page of messages.
//
// Grouped by emoji rather than returned per person, because that is how it is
// read: "four thumbs up, one of them mine" is the question on screen, and a
// flat list the client has to group is the data shaped for the database
// instead of for the reader.
func (r *SQLRepository) ReactionsFor(ctx context.Context, userID string, ids []string) (map[string][]Reaction, error) {
	if len(ids) == 0 {
		return map[string][]Reaction{}, nil
	}
	holes, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id, emoji, user_id FROM messaging_reactions
		  WHERE message_id IN (`+holes+`)
		  ORDER BY created_at ASC, rowid ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("reactions: %w", err)
	}
	defer rows.Close()

	// message -> emoji -> group, keeping first-seen order so the row does not
	// reshuffle every time somebody reacts.
	byMsg := map[string][]Reaction{}
	index := map[string]map[string]int{}
	for rows.Next() {
		var mid, emoji, uid string
		if err := rows.Scan(&mid, &emoji, &uid); err != nil {
			return nil, err
		}
		if index[mid] == nil {
			index[mid] = map[string]int{}
		}
		at, ok := index[mid][emoji]
		if !ok {
			byMsg[mid] = append(byMsg[mid], Reaction{Emoji: emoji})
			at = len(byMsg[mid]) - 1
			index[mid][emoji] = at
		}
		g := &byMsg[mid][at]
		g.Count++
		if uid == userID {
			g.Mine = true
		}
		if len(g.Who) < 12 {
			g.Who = append(g.Who, uid)
		}
	}
	return byMsg, rows.Err()
}

// QuotesFor loads the messages a page of replies points at.
func (r *SQLRepository) QuotesFor(ctx context.Context, ids []string) (map[string]*Quote, error) {
	if len(ids) == 0 {
		return map[string]*Quote{}, nil
	}
	holes, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, sender_id, sender_name, body, deleted FROM messaging_messages
		  WHERE id IN (`+holes+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("quotes: %w", err)
	}
	defer rows.Close()

	out := map[string]*Quote{}
	for rows.Next() {
		var q Quote
		var deleted int
		if err := rows.Scan(&q.ID, &q.SenderID, &q.SenderName, &q.Body, &deleted); err != nil {
			return nil, err
		}
		q.Deleted = deleted != 0
		if q.Deleted {
			// A quote of a deleted message says so rather than showing what
			// was withdrawn. Deleting a message has to delete it everywhere it
			// appears, or the delete is decorative.
			q.Body = ""
		}
		q.Body = truncate(q.Body, 160)
		out[q.ID] = &q
	}
	return out, rows.Err()
}

// Save marks or unmarks a message for one person.
func (r *SQLRepository) Save(ctx context.Context, userID, messageID string, on bool) error {
	if userID == "" || messageID == "" {
		return ErrNotFound
	}
	if !on {
		_, err := r.db.ExecContext(ctx,
			`DELETE FROM messaging_saved WHERE user_id=? AND message_id=?`,
			userID, messageID)
		if err != nil {
			return fmt.Errorf("unsave: %w", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_saved (user_id, message_id, created_at)
		 VALUES (?,?,?)`, userID, messageID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// SavedIDs reports which of a page of messages this person has saved.
func (r *SQLRepository) SavedIDs(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	if userID == "" || len(ids) == 0 {
		return map[string]bool{}, nil
	}
	holes, args := placeholders(ids)
	args = append([]any{userID}, args...)
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id FROM messaging_saved
		  WHERE user_id=? AND message_id IN (`+holes+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("saved ids: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SavedMessages is the person's saved list, newest saved first.
func (r *SQLRepository) SavedMessages(ctx context.Context, userID string, limit int) ([]*Message, error) {
	if userID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+prefixed(msgCols, "m")+` FROM messaging_saved s
		   JOIN messaging_messages m ON m.id = s.message_id
		  WHERE s.user_id=? AND m.deleted = 0
		  ORDER BY s.created_at DESC, s.rowid DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("saved messages: %w", err)
	}
	defer rows.Close()

	var out []*Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		m.Saved = true
		out = append(out, m)
	}
	return out, rows.Err()
}

// placeholders builds "?,?,?" and the arguments to go with it, bounded.
//
// The bound is the point. A page of messages is fifty; anything asking about
// five hundred ids at once is a bug or an attempt, and either way the query
// planner should not have to find out.
func placeholders(ids []string) (string, []any) {
	if len(ids) > 200 {
		ids = ids[:200]
	}
	parts := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		parts[i] = "?"
		args[i] = id
	}
	return strings.Join(parts, ","), args
}

var _ = errors.Is
var _ = sql.ErrNoRows
