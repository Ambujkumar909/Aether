package messaging

import (
	"context"
	"fmt"
	"time"
)

// Delete for me.
//
// Two deletes, because they answer two different questions. "Delete for
// everyone" is about the message: the sender takes it back, and every member
// sees a tombstone (Service.Delete, a soft delete on the row). "Delete for me"
// is about one reader's view: the message leaves their thread and nobody
// else's, and the sender need not be the one doing it. So it is not a fact
// about the message and does not live on the row -- it is a (user, message)
// pair, exactly like saving one, and it is applied when a page of history is
// read rather than when the row is written.

// Hide removes a message from one person's view of a conversation.
func (r *SQLRepository) Hide(ctx context.Context, userID, messageID string) error {
	if userID == "" || messageID == "" {
		return ErrNotFound
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_hidden (user_id, message_id, created_at)
		 VALUES (?, ?, ?)`, userID, messageID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("hide: %w", err)
	}
	if k, _ := res.RowsAffected(); k > 0 {
		// Hidden while still unread: one less to read, for this person only.
		if _, err := tx.ExecContext(ctx, `
			UPDATE messaging_members SET unread = unread - 1
			 WHERE user_id = ? AND unread > 0
			   AND conversation_id = (SELECT conversation_id FROM messaging_messages WHERE id=?)
			   AND last_read < (SELECT created_at FROM messaging_messages WHERE id=?)
			   AND ? != (SELECT sender_id FROM messaging_messages WHERE id=?)
			   AND 0 = (SELECT deleted FROM messaging_messages WHERE id=?)
			   AND 0 = (SELECT system FROM messaging_messages WHERE id=?)`,
			userID, messageID, messageID, userID, messageID, messageID, messageID); err != nil {
			return fmt.Errorf("hide: adjust unread: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE messaging_members SET mentions = mentions - 1
			 WHERE user_id = ? AND mentions > 0
			   AND conversation_id = (SELECT conversation_id FROM messaging_messages WHERE id=?)
			   AND EXISTS (SELECT 1 FROM messaging_mentions mn WHERE mn.message_id=? AND mn.user_id=?)
			   AND last_read < (SELECT created_at FROM messaging_messages WHERE id=?)
			   AND ? != (SELECT sender_id FROM messaging_messages WHERE id=?)
			   AND 0 = (SELECT deleted FROM messaging_messages WHERE id=?)`,
			userID, messageID, messageID, userID, messageID, userID, messageID, messageID); err != nil {
			return fmt.Errorf("hide: adjust mentions: %w", err)
		}
	}
	return tx.Commit()
}

// HiddenIDs says which of a page of messages this person has hidden.
func (r *SQLRepository) HiddenIDs(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	if userID == "" || len(ids) == 0 {
		return map[string]bool{}, nil
	}
	holes, args := placeholders(ids)
	args = append([]any{userID}, args...)
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id FROM messaging_hidden
		  WHERE user_id=? AND message_id IN (`+holes+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("hidden ids: %w", err)
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

// Hide is "delete for me": the message leaves the actor's thread and nobody
// else's. Any member may do it to any message in the conversation -- it is
// their view they are editing, not the record.
func (s *Service) Hide(ctx context.Context, a Actor, messageID string) error {
	m, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		return err
	}
	ok, err := s.repo.IsMember(ctx, m.ConversationID, a.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return s.repo.Hide(ctx, a.ID, messageID)
}

// withoutHidden drops what this person has deleted for themselves. Applied
// to a page after it is read, so a hidden message still counts against the
// page size -- a page can come back one short, which is fine, rather than
// the filter living in every history query, which is not.
func (s *Service) withoutHidden(ctx context.Context, a Actor, msgs []*Message) []*Message {
	if len(msgs) == 0 {
		return msgs
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	hidden, err := s.repo.HiddenIDs(ctx, a.ID, ids)
	if err != nil {
		s.log.Warn("could not read hidden messages", "err", err)
		return msgs
	}
	if len(hidden) == 0 {
		return msgs
	}
	out := msgs[:0]
	for _, m := range msgs {
		if !hidden[m.ID] {
			out = append(out, m)
		}
	}
	return out
}
