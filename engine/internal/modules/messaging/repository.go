package messaging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// SQLRepository owns messaging_conversations, messaging_members, and
// messaging_messages.
type SQLRepository struct {
	db *database.DB
}

func NewSQLRepository(db *database.DB) *SQLRepository { return &SQLRepository{db: db} }

const convCols = `id, kind, title, created_by, created_at, last_message_at, topic, visibility, member_count, avatar_blob, pinned_message_id, meeting_room, team_id`

func (r *SQLRepository) CreateConversation(ctx context.Context, c *Conversation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	visibility := c.Visibility
	if visibility == "" {
		visibility = VisibilityPrivate
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messaging_conversations (`+convCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, string(c.Kind), c.Title, c.CreatedBy,
		c.CreatedAt.UnixMilli(), c.LastMessageAt.UnixMilli(),
		c.Topic, visibility, len(c.Members), "", "", c.MeetingRoom, c.TeamID); err != nil {
		return fmt.Errorf("insert conversation: %w", err)
	}
	for _, m := range c.Members {
		role := m.Role
		if role == "" {
			role = RoleMember
		}
		// The read cursor sits one millisecond before the join: the counters
		// count created_at > last_read, and a message written in the same
		// millisecond as the join must count. See samems_test.go.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messaging_members (conversation_id, user_id, email, name, joined_at, last_read, role)
			VALUES (?,?,?,?,?,?,?)`,
			c.ID, m.UserID, m.Email, m.Name,
			m.JoinedAt.UnixMilli(), m.LastRead.UnixMilli()-1, role); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}
	c.MemberCount = len(c.Members)
	return tx.Commit()
}

func (r *SQLRepository) ConversationByID(ctx context.Context, id string) (*Conversation, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+convCols+` FROM messaging_conversations WHERE id=?`, id)
	c, err := scanConv(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := r.loadMembers(ctx, []*Conversation{c}); err != nil {
		return nil, err
	}
	return c, nil
}

// DirectBetween finds the two-person thread shared by exactly these users.
//
// The HAVING clause is what makes it exact: a conversation containing both
// of them AND nobody else. Without the count check, a group chat that happens
// to include both would be returned as their private thread.
func (r *SQLRepository) DirectBetween(ctx context.Context, a, b string) (*Conversation, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT c.id FROM messaging_conversations c
		JOIN messaging_members m ON m.conversation_id = c.id
		WHERE c.kind = ? AND m.user_id IN (?, ?)
		GROUP BY c.id
		HAVING COUNT(DISTINCT m.user_id) = 2
		   AND (SELECT COUNT(*) FROM messaging_members x WHERE x.conversation_id = c.id) = 2
		LIMIT 1`, string(KindDirect), a, b).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find direct: %w", err)
	}
	return r.ConversationByID(ctx, id)
}

// SelfConversation finds the thread whose only member is this person.
//
// The count is the whole predicate: a direct conversation with exactly one
// member, and that member is you. It cannot collide with a two-person thread
// you are in, however the other person's account is later renamed or deleted.
func (r *SQLRepository) SelfConversation(ctx context.Context, userID string) (*Conversation, error) {
	if userID == "" {
		return nil, ErrNotFound
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT c.id FROM messaging_conversations c
		JOIN messaging_members m ON m.conversation_id = c.id
		WHERE c.kind = ? AND m.user_id = ?
		  AND (SELECT COUNT(*) FROM messaging_members x WHERE x.conversation_id = c.id) = 1
		LIMIT 1`, string(KindDirect), userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find self conversation: %w", err)
	}
	return r.ConversationByID(ctx, id)
}

func (r *SQLRepository) ListConversations(ctx context.Context, userID string) ([]*Conversation, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixed(convCols, "c")+`, m.role, m.favourite, m.muted, m.mentions FROM messaging_conversations c
		JOIN messaging_members m ON m.conversation_id = c.id
		WHERE m.user_id = ? AND m.hidden = 0
		ORDER BY c.last_message_at DESC LIMIT 200`, userID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []*Conversation
	for rows.Next() {
		var c Conversation
		var kind string
		var created, last int64
		var fav, muted int
		var avatar string
		if err := rows.Scan(&c.ID, &kind, &c.Title, &c.CreatedBy, &created, &last,
			&c.Topic, &c.Visibility, &c.MemberCount, &avatar, &c.PinnedMessageID, &c.MeetingRoom, &c.TeamID,
			&c.MyRole, &fav, &muted, &c.Mentions); err != nil {
			return nil, err
		}
		c.Avatar = avatar != ""
		c.Favourite = fav != 0
		c.Muted = muted != 0
		c.Kind = Kind(kind)
		c.CreatedAt = time.UnixMilli(created).UTC()
		c.LastMessageAt = time.UnixMilli(last).UTC()
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, r.loadMembers(ctx, out)
}

// loadMembers fills membership for a page in one query rather than per row.
func (r *SQLRepository) loadMembers(ctx context.Context, cs []*Conversation) error {
	if len(cs) == 0 {
		return nil
	}
	byID := make(map[string]*Conversation, len(cs))
	ph := make([]string, 0, len(cs))
	args := make([]any, 0, len(cs))
	for _, c := range cs {
		// A big channel ships its count, not its people; the people panel
		// pages through them on demand (MembersPage).
		if c.MemberCount > bigConversation {
			continue
		}
		byID[c.ID] = c
		ph = append(ph, "?")
		args = append(args, c.ID)
	}
	if len(ph) == 0 {
		return nil
	}
	q := `SELECT conversation_id, user_id, email, name, joined_at, last_read, role
	      FROM messaging_members WHERE conversation_id IN (` +
		strings.Join(ph, ",") + `) ORDER BY rowid`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("load members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid string
		var m Member
		var joined, read int64
		if err := rows.Scan(&cid, &m.UserID, &m.Email, &m.Name, &joined, &read, &m.Role); err != nil {
			return err
		}
		m.JoinedAt = time.UnixMilli(joined).UTC()
		m.LastRead = time.UnixMilli(read).UTC()
		if c := byID[cid]; c != nil {
			c.Members = append(c.Members, m)
		}
	}
	return rows.Err()
}

func (r *SQLRepository) IsMember(ctx context.Context, convID, userID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messaging_members WHERE conversation_id=? AND user_id=?`,
		convID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("membership check: %w", err)
	}
	return n > 0, nil
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

const msgCols = `id, conversation_id, sender_id, sender_name, body, created_at, edited_at, deleted, reply_to, forwarded, system, priority, deliver_at, meta`

// AddMessage writes the message and advances the conversation's activity
// stamp in one transaction, so the list ordering can never disagree with the
// history.
func (r *SQLRepository) AddMessage(ctx context.Context, m *Message) error {
	return r.AddMessageWithAttachments(ctx, m, nil)
}

const attCols = `id, message_id, conversation_id, uploader_id, name, mime, size, blob, created_at`

// AddMessageWithAttachments writes the message, its files and the
// conversation's activity stamp in one transaction: a message with half its
// files is not a state anybody should be able to read.
func (r *SQLRepository) AddMessageWithAttachments(ctx context.Context, m *Message, atts []*Attachment) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deliverAt any
	pending := false
	if m.DeliverAt != nil {
		deliverAt = m.DeliverAt.UnixMilli()
		pending = m.DeliverAt.After(m.CreatedAt)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messaging_messages (`+msgCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ConversationID, m.SenderID, m.SenderName, m.Body,
		m.CreatedAt.UnixMilli(), nil, 0, m.ReplyTo, boolInt(m.Forwarded), boolInt(m.System),
		m.Priority, deliverAt, m.Meta); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if err := indexMessage(ctx, tx, m.ID, m.Body, m.System); err != nil {
		return err
	}
	// A scheduled message is a row and nothing else until it is delivered:
	// no counters, no unhide, no activity stamp. Deliver does all of that.
	if pending {
		for _, a := range atts {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO messaging_attachments (`+attCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
				a.ID, m.ID, m.ConversationID, a.UploaderID, a.Name, a.MIME, a.Size, a.Blob,
				a.CreatedAt.UnixMilli()); err != nil {
				return fmt.Errorf("insert attachment: %w", err)
			}
		}
		for _, uid := range m.Mentions {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO messaging_mentions (message_id, user_id) VALUES (?, ?)`, m.ID, uid); err != nil {
				return fmt.Errorf("mention: %w", err)
			}
		}
		return tx.Commit()
	}
	// The unread counter: everybody but the sender has one more. Membership
	// events do not count -- nobody needs a badge to learn somebody joined.
	if !m.System {
		// The same predicate as unreadTruth: a member whose cursor already
		// sits at this millisecond has, by the cursor, read it.
		if _, err := tx.ExecContext(ctx,
			`UPDATE messaging_members SET unread = unread + 1
			  WHERE conversation_id=? AND user_id != ? AND last_read < ?`,
			m.ConversationID, m.SenderID, m.CreatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("bump unread: %w", err)
		}
		// A chat somebody hid comes back when somebody writes in it -- that is
		// what "hide" means, as opposed to "leave".
		if _, err := tx.ExecContext(ctx,
			`UPDATE messaging_members SET hidden = 0
			  WHERE conversation_id=? AND user_id != ? AND hidden = 1`,
			m.ConversationID, m.SenderID); err != nil {
			return fmt.Errorf("unhide: %w", err)
		}
		// Mentions: the rows, and one more for each named member whose cursor
		// is behind the message (the unread predicate again).
		for _, uid := range m.Mentions {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO messaging_mentions (message_id, user_id) VALUES (?, ?)`, m.ID, uid); err != nil {
				return fmt.Errorf("mention: %w", err)
			}
			if uid == m.SenderID {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE messaging_members SET mentions = mentions + 1
				  WHERE conversation_id=? AND user_id=? AND last_read < ?`,
				m.ConversationID, uid, m.CreatedAt.UnixMilli()); err != nil {
				return fmt.Errorf("bump mentions: %w", err)
			}
		}
	}
	// After the message: the foreign key is enforced, and a file row that
	// points at nothing is refused by the database, which is right.
	for _, a := range atts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messaging_attachments (`+attCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			a.ID, m.ID, m.ConversationID, a.UploaderID, a.Name, a.MIME, a.Size, a.Blob,
			a.CreatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("insert attachment: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_conversations SET last_message_at=? WHERE id=?`,
		m.CreatedAt.UnixMilli(), m.ConversationID); err != nil {
		return fmt.Errorf("touch conversation: %w", err)
	}
	return tx.Commit()
}

// Order is (created_at, rowid): two messages sent inside the same
// millisecond -- a fast typist, a paste, a test -- have equal timestamps, and
// a sort on the timestamp alone is then whatever the engine feels like, which
// showed up as [three two] for a thread that was sent [two three]. rowid is
// insertion order, so it settles every tie the way the sender saw it.
func (r *SQLRepository) ListMessages(ctx context.Context, convID string, before time.Time, limit int) ([]*Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+msgCols+` FROM messaging_messages
		 WHERE conversation_id=? AND created_at < ?
		 ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		convID, before.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []*Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanAtt(s rowScanner) (Attachment, error) {
	var a Attachment
	var created int64
	err := s.Scan(&a.ID, &a.MessageID, &a.ConversationID, &a.UploaderID,
		&a.Name, &a.MIME, &a.Size, &a.Blob, &created)
	a.CreatedAt = time.UnixMilli(created).UTC()
	return a, err
}

func (r *SQLRepository) AttachmentsFor(ctx context.Context, ids []string) (map[string][]Attachment, error) {
	out := map[string][]Attachment{}
	if len(ids) == 0 {
		return out, nil
	}
	holes, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+attCols+` FROM messaging_attachments
		  WHERE message_id IN (`+holes+`) ORDER BY created_at ASC, rowid ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			return nil, err
		}
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	return out, rows.Err()
}

func (r *SQLRepository) AttachmentByID(ctx context.Context, id string) (*Attachment, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+attCols+` FROM messaging_attachments WHERE id=?`, id)
	a, err := scanAtt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// SharedFiles: newest first, with the sender's name, skipping files whose
// message was deleted for everyone.
func (r *SQLRepository) SharedFiles(ctx context.Context, convID string, limit int) ([]Attachment, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+prefixed(attCols, "a")+`, m.sender_name
		   FROM messaging_attachments a
		   JOIN messaging_messages m ON m.id = a.message_id
		  WHERE a.conversation_id=? AND m.deleted=0
		  ORDER BY a.created_at DESC, a.rowid DESC LIMIT ?`, convID, limit)
	if err != nil {
		return nil, fmt.Errorf("shared files: %w", err)
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.MessageID, &a.ConversationID, &a.UploaderID,
			&a.Name, &a.MIME, &a.Size, &a.Blob, &created, &a.SenderName); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *SQLRepository) MessageByID(ctx context.Context, id string) (*Message, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messaging_messages WHERE id=?`, id)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (r *SQLRepository) SoftDeleteMessage(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Whoever had not read it yet has one less to read: same predicate as the
	// count, so the counter and the truth move together.
	if _, err := tx.ExecContext(ctx, `
		UPDATE messaging_members SET unread = unread - 1
		 WHERE unread > 0
		   AND conversation_id = (SELECT conversation_id FROM messaging_messages WHERE id=? AND deleted=0)
		   AND user_id != (SELECT sender_id FROM messaging_messages WHERE id=?)
		   AND last_read < (SELECT created_at FROM messaging_messages WHERE id=?)
		   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h WHERE h.user_id = messaging_members.user_id AND h.message_id = ?)`,
		id, id, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messaging_members SET mentions = mentions - 1
		 WHERE mentions > 0
		   AND conversation_id = (SELECT conversation_id FROM messaging_messages WHERE id=? AND deleted=0)
		   AND user_id IN (SELECT user_id FROM messaging_mentions WHERE message_id=?)
		   AND user_id != (SELECT sender_id FROM messaging_messages WHERE id=?)
		   AND last_read < (SELECT created_at FROM messaging_messages WHERE id=?)
		   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h WHERE h.user_id = messaging_members.user_id AND h.message_id = ?)`,
		id, id, id, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_messages SET deleted=1, body='' WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messaging_messages_fts WHERE id = ?`, id); err != nil {
		return err
	}
	// A deleted message is nobody's activity any more.
	if _, err := tx.ExecContext(ctx, `DELETE FROM messaging_activity WHERE message_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLRepository) EditMessage(ctx context.Context, id, body string, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// What it said until now goes into the history first, keyed by this
	// edit's time, so the versions read in order.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO messaging_message_edits(message_id, edited_at, body)
		SELECT id, ?, body FROM messaging_messages WHERE id=? AND deleted=0`, at.UnixMilli(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_messages SET body=?, edited_at=? WHERE id=? AND deleted=0`,
		body, at.UnixMilli(), id); err != nil {
		return err
	}
	if err := indexMessage(ctx, tx, id, body, false); err != nil {
		return err
	}
	return tx.Commit()
}

// EditHistory lists a message's earlier versions, oldest first.
func (r *SQLRepository) EditHistory(ctx context.Context, messageID string) ([]Edit, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT body, edited_at FROM messaging_message_edits WHERE message_id=? ORDER BY edited_at ASC LIMIT 100`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edit
	for rows.Next() {
		var e Edit
		var at int64
		if err := rows.Scan(&e.Body, &at); err != nil {
			return nil, err
		}
		e.EditedAt = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Threads.

// ListRoots is the channel feed: posts that are not replies (system events
// included), newest first, one index seek.
func (r *SQLRepository) ListRoots(ctx context.Context, convID string, before time.Time, limit int) ([]*Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+msgCols+` FROM messaging_messages
		 WHERE conversation_id=? AND reply_to='' AND created_at < ?
		 ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		convID, before.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("list roots: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListReplies is one thread, oldest first.
func (r *SQLRepository) ListReplies(ctx context.Context, rootID string, limit int) ([]*Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+msgCols+` FROM messaging_messages
		 WHERE reply_to=? ORDER BY created_at ASC, rowid ASC LIMIT ?`, rootID, limit)
	if err != nil {
		return nil, fmt.Errorf("list replies: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ThreadSummaries counts the replies under each root and names the three
// most recent repliers: one query over the reply_to index for the page.
func (r *SQLRepository) ThreadSummaries(ctx context.Context, rootIDs []string) (map[string]*ThreadSummary, error) {
	out := map[string]*ThreadSummary{}
	if len(rootIDs) == 0 {
		return out, nil
	}
	ph, args := placeholders(rootIDs)
	rows, err := r.db.QueryContext(ctx, `
		SELECT reply_to, sender_name, created_at FROM messaging_messages
		 WHERE reply_to IN (`+ph+`) AND deleted = 0
		 ORDER BY reply_to, created_at DESC, rowid DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("thread summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var root, name string
		var at int64
		if err := rows.Scan(&root, &name, &at); err != nil {
			return nil, err
		}
		t := out[root]
		if t == nil {
			last := time.UnixMilli(at).UTC()
			t = &ThreadSummary{LastAt: &last}
			out[root] = t
		}
		t.Count++
		if len(t.LastNames) < 3 && !contains(t.LastNames, name) {
			t.LastNames = append(t.LastNames, name)
		}
	}
	return out, rows.Err()
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// indexMessage keeps the search index in step with one message: the plain
// text, or nothing for a membership event or an empty body.
func indexMessage(ctx context.Context, tx *sql.Tx, id, body string, system bool) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM messaging_messages_fts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("unindex: %w", err)
	}
	if system {
		return nil
	}
	plain := plainText(body)
	if plain == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messaging_messages_fts(id, body) VALUES (?, ?)`, id, plain); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// ReindexIfNeeded refills the search index when it is empty and the table is
// not -- once, after the migration that emptied it, in one pass.
func (r *SQLRepository) ReindexIfNeeded(ctx context.Context) (int, error) {
	var indexed, total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_messages_fts`).Scan(&indexed); err != nil {
		return 0, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messaging_messages WHERE system = 0 AND deleted = 0 AND body != ''`).Scan(&total); err != nil {
		return 0, err
	}
	if indexed > 0 || total == 0 {
		return 0, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, body FROM messaging_messages WHERE system = 0 AND deleted = 0 AND body != ''`)
	if err != nil {
		return 0, err
	}
	type row struct{ id, body string }
	var all []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.body); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, x)
	}
	rows.Close()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n := 0
	for _, x := range all {
		if p := plainText(x.body); p != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO messaging_messages_fts(id, body) VALUES (?, ?)`, x.id, p); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, tx.Commit()
}

func (r *SQLRepository) LastMessage(ctx context.Context, convID string) (*Message, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messaging_messages
		 WHERE conversation_id=? AND (deliver_at IS NULL OR deliver_at <= created_at)
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`, convID)
	m, err := scanMsg(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// PeersOf finds everyone sharing a conversation with this user.
func (r *SQLRepository) PeersOf(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT other.user_id FROM messaging_members mine
		JOIN messaging_members other ON other.conversation_id = mine.conversation_id
		WHERE mine.user_id = ? AND other.user_id != ?
		LIMIT 500`, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("peers: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// compose: the repository half of compose.go.

// DueScheduled lists pending messages whose time has come.
func (r *SQLRepository) DueScheduled(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM messaging_messages
		  WHERE deliver_at IS NOT NULL AND deliver_at <= ? AND deliver_at > created_at AND deleted = 0
		  ORDER BY deliver_at LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("due scheduled: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Deliver turns a pending message into an ordinary one at `at`: stamped now,
// counted for everybody, the conversation touched, hidden chats brought back.
// Idempotent: a message already delivered (deliver_at <= created_at) is
// returned as nil.
func (r *SQLRepository) Deliver(ctx context.Context, id string, at time.Time) (*Message, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE messaging_messages SET created_at = ?, deliver_at = ?
		  WHERE id = ? AND deliver_at IS NOT NULL AND deliver_at > created_at AND deleted = 0`,
		at.UnixMilli(), at.UnixMilli(), id)
	if err != nil {
		return nil, fmt.Errorf("deliver: %w", err)
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return nil, nil
	}
	m, err := scanMsg(tx.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messaging_messages WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_conversations SET last_message_at=? WHERE id=? AND last_message_at < ?`,
		at.UnixMilli(), m.ConversationID, at.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_members SET unread = unread + 1
		  WHERE conversation_id=? AND user_id != ? AND last_read < ?`,
		m.ConversationID, m.SenderID, at.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_members SET hidden = 0 WHERE conversation_id=? AND user_id != ? AND hidden = 1`,
		m.ConversationID, m.SenderID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messaging_members SET mentions = mentions + 1
		 WHERE conversation_id=? AND user_id != ? AND last_read < ?
		   AND user_id IN (SELECT user_id FROM messaging_mentions WHERE message_id=?)`,
		m.ConversationID, m.SenderID, at.UnixMilli(), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return m, nil
}

// SearchMessages: the FTS candidates first (bounded), then only the ones in
// conversations this person is in and has not deleted for themselves,
// newest first. CROSS JOIN pins the candidates as the outer loop.
func (r *SQLRepository) SearchMessages(ctx context.Context, userID, q, convID string, limit int) ([]*Message, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixed(msgCols, "m")+`
		  FROM (SELECT id FROM messaging_messages_fts WHERE messaging_messages_fts MATCH ? LIMIT 2000) AS f
		  CROSS JOIN messaging_messages AS m ON m.id = f.id
		  JOIN messaging_members AS mem ON mem.conversation_id = m.conversation_id AND mem.user_id = ?
		 WHERE m.deleted = 0
		   AND (? = '' OR m.conversation_id = ?)
		   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h WHERE h.user_id = ? AND h.message_id = m.id)
		 ORDER BY m.created_at DESC, m.rowid DESC LIMIT ?`,
		ftsPhrase(q), userID, convID, convID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Status.

func (r *SQLRepository) SetStatus(ctx context.Context, st Status) error {
	var until int64
	if st.Until != nil {
		until = st.Until.UnixMilli()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_status(user_id, status, note, until, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET status=excluded.status, note=excluded.note, until=excluded.until, updated_at=excluded.updated_at`,
		st.UserID, st.Status, st.Note, until, st.UpdatedAt.UnixMilli())
	return err
}

func (r *SQLRepository) StatusesFor(ctx context.Context, ids []string) (map[string]Status, error) {
	out := map[string]Status{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id, status, note, until, updated_at FROM messaging_status WHERE user_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st Status
		var until, updated int64
		if err := rows.Scan(&st.UserID, &st.Status, &st.Note, &until, &updated); err != nil {
			return nil, err
		}
		st.UpdatedAt = time.UnixMilli(updated).UTC()
		if until > 0 {
			t := time.UnixMilli(until).UTC()
			st.Until = &t
		}
		out[st.UserID] = st
	}
	return out, rows.Err()
}

// Presence: when a person was last active.

func (r *SQLRepository) TouchSeen(ctx context.Context, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_presence(user_id, last_seen) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET last_seen = excluded.last_seen`,
		userID, at.UnixMilli())
	return err
}

func (r *SQLRepository) LastSeenFor(ctx context.Context, ids []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx, `SELECT user_id, last_seen FROM messaging_presence WHERE user_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ms int64
		if err := rows.Scan(&id, &ms); err != nil {
			return nil, err
		}
		out[id] = time.UnixMilli(ms).UTC()
	}
	return out, rows.Err()
}

// Polls.

func (r *SQLRepository) SetVotes(ctx context.Context, messageID, userID string, options []int, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messaging_poll_votes WHERE message_id=? AND user_id=?`, messageID, userID); err != nil {
		return err
	}
	for _, o := range options {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO messaging_poll_votes(message_id, user_id, option, voted_at) VALUES (?, ?, ?, ?)`,
			messageID, userID, o, at.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PollTalliesFor reads every vote on the given messages in one query and
// folds them: counts per option, up to twelve voters per option, the
// reader's own options, and the number of distinct voters. Only messages
// with at least one vote appear in the map.
func (r *SQLRepository) PollTalliesFor(ctx context.Context, userID string, messageIDs []string) (map[string]*PollTally, error) {
	out := map[string]*PollTally{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	ph, args := placeholders(messageIDs)
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id, user_id, option FROM messaging_poll_votes WHERE message_id IN (`+ph+`) ORDER BY message_id, voted_at, user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	voters := map[string]map[string]bool{}
	for rows.Next() {
		var mid, uid string
		var opt int
		if err := rows.Scan(&mid, &uid, &opt); err != nil {
			return nil, err
		}
		t := out[mid]
		if t == nil {
			t = &PollTally{}
			out[mid] = t
			voters[mid] = map[string]bool{}
		}
		for len(t.Counts) <= opt {
			t.Counts = append(t.Counts, 0)
			t.Voters = append(t.Voters, nil)
		}
		t.Counts[opt]++
		if len(t.Voters[opt]) < 12 {
			t.Voters[opt] = append(t.Voters[opt], uid)
		}
		if uid == userID {
			t.Mine = append(t.Mine, opt)
		}
		if !voters[mid][uid] {
			voters[mid][uid] = true
			t.Total++
		}
	}
	return out, rows.Err()
}

// DeleteConversation removes a conversation and everything under it, and
// returns the attachments whose blobs the caller should remove from disk.
func (r *SQLRepository) DeleteConversation(ctx context.Context, convID string) ([]Attachment, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+attCols+` FROM messaging_attachments WHERE conversation_id = ?`, convID)
	if err != nil {
		return nil, err
	}
	var atts []Attachment
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		atts = append(atts, a)
	}
	rows.Close()
	for _, q := range []string{
		`DELETE FROM messaging_poll_votes WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_mentions WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_hidden WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_reactions WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_saved WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_attachments WHERE message_id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_messages_fts WHERE id IN (SELECT id FROM messaging_messages WHERE conversation_id = ?)`,
		`DELETE FROM messaging_messages WHERE conversation_id = ?`,
		`DELETE FROM messaging_members WHERE conversation_id = ?`,
		`DELETE FROM messaging_conversations WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, convID); err != nil {
			return nil, fmt.Errorf("delete conversation: %w", err)
		}
	}
	return atts, tx.Commit()
}

// Blocks.

func (r *SQLRepository) Block(ctx context.Context, userID, blockedID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_blocks(user_id, blocked_id, created_at) VALUES (?, ?, ?)`,
		userID, blockedID, at.UnixMilli())
	return err
}

func (r *SQLRepository) Unblock(ctx context.Context, userID, blockedID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM messaging_blocks WHERE user_id = ? AND blocked_id = ?`, userID, blockedID)
	return err
}

func (r *SQLRepository) BlockedIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT blocked_id FROM messaging_blocks WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BlockedEither: has either of two people blocked the other. Two primary-key
// probes.
func (r *SQLRepository) BlockedEither(ctx context.Context, a, b string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messaging_blocks
		 WHERE (user_id = ? AND blocked_id = ?) OR (user_id = ? AND blocked_id = ?)`,
		a, b, b, a).Scan(&n)
	return n > 0, err
}

func (r *SQLRepository) BlockCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_blocks WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func ftsPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

// ConversationByMeetingRoom finds the room's chat. Nil, nil when none.
func (r *SQLRepository) ConversationByMeetingRoom(ctx context.Context, room string) (*Conversation, error) {
	if room == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+convCols+` FROM messaging_conversations WHERE meeting_room=?`, room)
	c, err := scanConv(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.loadMembers(ctx, []*Conversation{c}); err != nil {
		return nil, err
	}
	return c, nil
}

// groups: the repository half of groups.go.

func (r *SQLRepository) HideBefore(ctx context.Context, convID, userID string, before time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_hidden (user_id, message_id, created_at)
		 SELECT ?, id, ? FROM messaging_messages WHERE conversation_id=? AND created_at <= ?`,
		userID, now, convID, before.UnixMilli()); err != nil {
		return fmt.Errorf("hide before: %w", err)
	}
	// The counters follow: whatever was just hidden no longer counts.
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_members SET unread = (`+unreadTruth+`), mentions = (`+mentionsTruth+`)
		  WHERE conversation_id=? AND user_id=?`, convID, userID); err != nil {
		return fmt.Errorf("hide before: recount: %w", err)
	}
	return tx.Commit()
}

func (r *SQLRepository) MentionsFor(ctx context.Context, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	holes, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id, user_id FROM messaging_mentions WHERE message_id IN (`+holes+`) ORDER BY rowid`, args...)
	if err != nil {
		return nil, fmt.Errorf("mentions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid, uid string
		if err := rows.Scan(&mid, &uid); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], uid)
	}
	return out, rows.Err()
}

func (r *SQLRepository) SetAvatar(ctx context.Context, convID, blob, mime string) (string, error) {
	var old string
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT avatar_blob FROM messaging_conversations WHERE id=?`, convID).Scan(&old); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_conversations SET avatar_blob=?, avatar_mime=? WHERE id=?`, blob, mime, convID); err != nil {
		return "", err
	}
	return old, tx.Commit()
}

func (r *SQLRepository) Avatar(ctx context.Context, convID string) (string, string, error) {
	var blob, mime string
	err := r.db.QueryRowContext(ctx,
		`SELECT avatar_blob, avatar_mime FROM messaging_conversations WHERE id=?`, convID).Scan(&blob, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return blob, mime, err
}

func (r *SQLRepository) SetPinned(ctx context.Context, convID, messageID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE messaging_conversations SET pinned_message_id=? WHERE id=?`, messageID, convID)
	return err
}

// prefs: the repository half of prefs.go.

func (r *SQLRepository) SetPrefs(ctx context.Context, convID, userID string, favourite, muted, hidden *bool) error {
	// Columns and their arguments are appended together, in one fixed order:
	// a first version built them from a map and sorted the columns
	// afterwards, so a flag could receive its neighbour's value.
	sets := []string{}
	args := []any{}
	for _, f := range []struct {
		col string
		v   *bool
	}{{"favourite", favourite}, {"muted", muted}, {"hidden", hidden}} {
		if f.v != nil {
			sets = append(sets, f.col+"=?")
			args = append(args, boolInt(*f.v))
		}
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, convID, userID)
	_, err := r.db.ExecContext(ctx,
		`UPDATE messaging_members SET `+strings.Join(sets, ", ")+` WHERE conversation_id=? AND user_id=?`, args...)
	return err
}

// MarkUnread moves the cursor to just before the newest real message, so the
// counter (recomputed from the same truth) shows at least that one. A
// conversation with nothing in it stays as it is.
func (r *SQLRepository) MarkUnread(ctx context.Context, convID, userID string) error {
	var newest sql.NullInt64
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM messaging_messages
		  WHERE conversation_id=? AND deleted=0 AND system=0 AND sender_id != ?`, convID, userID).Scan(&newest); err != nil {
		return err
	}
	if !newest.Valid {
		return nil
	}
	cursor := newest.Int64 - 1
	_, err := r.db.ExecContext(ctx,
		`UPDATE messaging_members SET last_read=?, unread = (`+unreadTruthAt+`)
		 WHERE conversation_id=? AND user_id=?`,
		cursor, cursor, convID, userID)
	return err
}

// ForgetConversation is "delete for me": every message so far leaves this
// person's view, the chat leaves the list until somebody writes again, and
// nothing about anybody else changes.
func (r *SQLRepository) ForgetConversation(ctx context.Context, convID, userID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO messaging_hidden (user_id, message_id, created_at)
		 SELECT ?, id, ? FROM messaging_messages WHERE conversation_id=?`, userID, now, convID); err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messaging_members SET hidden=1, unread=0, last_read=?
		  WHERE conversation_id=? AND user_id=?`, now, convID, userID); err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	return tx.Commit()
}

// membershipOps: the repository half of channels.go.

func (r *SQLRepository) AddMembers(ctx context.Context, convID string, ms []Member) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n := 0
	for _, m := range ms {
		role := m.Role
		if role == "" {
			role = RoleMember
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO messaging_members (conversation_id, user_id, email, name, joined_at, last_read, role)
			SELECT ?,?,?,?,?,?,?
			 WHERE NOT EXISTS (SELECT 1 FROM messaging_members WHERE conversation_id=? AND user_id=?)`,
			convID, m.UserID, m.Email, m.Name, m.JoinedAt.UnixMilli(), m.LastRead.UnixMilli()-1, role,
			convID, m.UserID)
		if err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE messaging_conversations SET member_count = member_count + ? WHERE id=?`, n, convID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *SQLRepository) RemoveMember(ctx context.Context, convID, userID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM messaging_members WHERE conversation_id=? AND user_id=?`, convID, userID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if k, _ := res.RowsAffected(); k > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE messaging_conversations SET member_count = max(member_count - 1, 0) WHERE id=?`, convID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *SQLRepository) MemberOf(ctx context.Context, convID, userID string) (*Member, error) {
	var m Member
	var joined, read int64
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, email, name, joined_at, last_read, role FROM messaging_members
		  WHERE conversation_id=? AND user_id=?`, convID, userID).
		Scan(&m.UserID, &m.Email, &m.Name, &joined, &read, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.JoinedAt = time.UnixMilli(joined).UTC()
	m.LastRead = time.UnixMilli(read).UTC()
	return &m, nil
}

// MemberIDs is who to push to: ids only, whatever the size of the channel.
func (r *SQLRepository) MemberIDs(ctx context.Context, convID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id FROM messaging_members WHERE conversation_id=?`, convID)
	if err != nil {
		return nil, fmt.Errorf("member ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *SQLRepository) MembersPage(ctx context.Context, convID string, offset, limit int) ([]Member, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT member_count FROM messaging_conversations WHERE id=?`, convID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id, email, name, joined_at, last_read, role FROM messaging_members
		  WHERE conversation_id=? ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END, name COLLATE NOCASE, rowid
		  LIMIT ? OFFSET ?`, convID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("members page: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var joined, read int64
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &joined, &read, &m.Role); err != nil {
			return nil, 0, err
		}
		m.JoinedAt = time.UnixMilli(joined).UTC()
		m.LastRead = time.UnixMilli(read).UTC()
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// PromoteOldest makes the longest-standing member the owner, when there is
// nobody with the role left.
func (r *SQLRepository) PromoteOldest(ctx context.Context, convID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE messaging_members SET role='owner'
		 WHERE conversation_id=?
		   AND NOT EXISTS (SELECT 1 FROM messaging_members o WHERE o.conversation_id=? AND o.role='owner')
		   AND rowid = (SELECT rowid FROM messaging_members WHERE conversation_id=? ORDER BY joined_at, rowid LIMIT 1)`,
		convID, convID, convID)
	return err
}

func (r *SQLRepository) UpdateConversation(ctx context.Context, convID, title, topic string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE messaging_conversations SET title=?, topic=? WHERE id=?`, title, topic, convID)
	return err
}

// PublicChannels: the browse view. A name prefix walks the browse index in
// order; otherwise a bounded scan of public channels, which are few. The
// viewer's own membership is a correlated existence check.
func (r *SQLRepository) PublicChannels(ctx context.Context, viewerID, q string, limit int) ([]ChannelSummary, error) {
	args := []any{viewerID, string(KindChannel), VisibilityPublic}
	where := ``
	if q != "" {
		lo := strings.ToLower(q)
		where = ` AND (c.title_key >= ? AND c.title_key < ? OR c.title_key LIKE ? ESCAPE '\' OR lower(c.topic) LIKE ? ESCAPE '\')`
		needle := "%" + escapeLike(lo) + "%"
		args = append(args, lo, lo+"\uffff", needle, needle)
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.title, c.topic, c.visibility, c.member_count, c.created_at, c.created_by,
		       EXISTS (SELECT 1 FROM messaging_members m WHERE m.conversation_id = c.id AND m.user_id = ?) AS joined
		  FROM messaging_conversations c
		 WHERE c.kind = ? AND c.visibility = ? AND c.team_id = ''`+where+`
		 ORDER BY c.title_key LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("browse channels: %w", err)
	}
	defer rows.Close()
	var out []ChannelSummary
	for rows.Next() {
		var ch ChannelSummary
		var created int64
		var joined int
		if err := rows.Scan(&ch.ID, &ch.Title, &ch.Topic, &ch.Visibility, &ch.MemberCount, &created, &ch.creatorID, &joined); err != nil {
			return nil, err
		}
		ch.CreatedAt = time.UnixMilli(created).UTC()
		ch.Joined = joined != 0
		out = append(out, ch)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

func (r *SQLRepository) MarkRead(ctx context.Context, convID, userID string, at time.Time) error {
	// Never move the cursor backwards. Two tabs open on the same conversation
	// would otherwise fight, and the loser would resurrect an unread badge
	// the user had already cleared.
	//
	// The counter is recomputed from the cursor rather than zeroed: a message
	// that arrived in the instant between the client reading the thread and
	// this request would otherwise be unread by the cursor and read by the
	// badge, and the two must never disagree.
	//
	// The subquery compares against the NEW cursor, bound explicitly: inside
	// an UPDATE, SQLite evaluates every SET expression against the row's old
	// values, so a subquery written against messaging_members.last_read would
	// count from the cursor being replaced and leave the badge where it was.
	// The random-schedule test caught exactly that on its first run.
	_, err := r.db.ExecContext(ctx,
		`UPDATE messaging_members SET last_read=?,
		        unread = (`+unreadTruthAt+`),
		        mentions = (`+mentionsTruthAt+`)
		 WHERE conversation_id=? AND user_id=? AND last_read < ?`,
		at.UnixMilli(), at.UnixMilli(), at.UnixMilli(), convID, userID, at.UnixMilli())
	return err
}

// mentionsTruth is the unread mentions the counter must equal: the unread
// predicate, restricted to messages that name this member.
const mentionsTruth = `SELECT COUNT(*) FROM messaging_mentions mn
		  JOIN messaging_messages msg ON msg.id = mn.message_id
		 WHERE mn.user_id = messaging_members.user_id
		   AND msg.conversation_id = messaging_members.conversation_id
		   AND msg.created_at > messaging_members.last_read
		   AND msg.sender_id != messaging_members.user_id
		   AND msg.deleted = 0 AND msg.system = 0
		   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h
		                    WHERE h.user_id = messaging_members.user_id AND h.message_id = msg.id)`

var mentionsTruthAt = strings.Replace(mentionsTruth,
	"msg.created_at > messaging_members.last_read", "msg.created_at > ?", 1)

// MentionCount is the truth the mentions counter is held to.
func (r *SQLRepository) MentionCount(ctx context.Context, convID, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT (`+mentionsTruth+`) FROM messaging_members WHERE conversation_id = ? AND user_id = ?`,
		convID, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// unreadTruthAt is unreadTruth with the cursor bound as a parameter, for the
// one statement that is moving it.
var unreadTruthAt = strings.Replace(unreadTruth,
	"msg.created_at > messaging_members.last_read", "msg.created_at > ?", 1)

// unreadTruth is the count the counter must always equal: messages in the
// conversation after this member's cursor, from somebody else, not deleted
// for everyone, not deleted for them, not membership events. Correlated on
// messaging_members.
const unreadTruth = `SELECT COUNT(*) FROM messaging_messages msg
		 WHERE msg.conversation_id = messaging_members.conversation_id
		   AND msg.created_at > messaging_members.last_read
		   AND msg.sender_id != messaging_members.user_id
		   AND msg.deleted = 0 AND msg.system = 0
		   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h
		                    WHERE h.user_id = messaging_members.user_id AND h.message_id = msg.id)`

// UnreadCounts is UnreadCount for every conversation the person is in, in
// one pass. Conversations with nothing unread are absent from the map.
func (r *SQLRepository) UnreadCounts(ctx context.Context, userID string) (map[string]int, error) {
	// A correlated count per membership row rather than a join-and-group:
	// the planner then walks the (conversation_id, created_at) index from
	// last_read forward for each conversation, touching only unread rows. The
	// join form visited every message of every conversation first and
	// measured 2.5x slower on the scale fixture.
	// The maintained counter (see the 0037 migration): one indexed read per
	// membership, no counting. UnreadCount below is the truth it is held to.
	rows, err := r.db.QueryContext(ctx, `
		SELECT conversation_id, unread FROM messaging_members
		 WHERE user_id = ? AND unread > 0`, userID)
	if err != nil {
		return nil, fmt.Errorf("unread counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// LastMessages is LastMessage for a set of conversations in one query: a
// window over the (conversation_id, created_at) index picks the newest row
// of each, with rowid settling ties the way ListMessages does.
func (r *SQLRepository) LastMessages(ctx context.Context, convIDs []string) (map[string]*Message, error) {
	out := map[string]*Message{}
	if len(convIDs) == 0 {
		return out, nil
	}
	holes, args := placeholders(convIDs)
	// The conversation row already knows when its newest message arrived
	// (AddMessage stamps last_message_at in the same transaction), so this is
	// one index seek per conversation rather than a window over every message
	// in all of them. Two messages in the same millisecond both match; the
	// higher rowid -- the later insert -- wins, as everywhere else. A
	// scheduled message that has not been delivered is excluded: it was
	// written at the conversation's creation millisecond once in a test and
	// became the other person's preview before its time.
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixed(msgCols, "m")+`, m.rowid
		  FROM messaging_conversations c
		  JOIN messaging_messages m
		    ON m.conversation_id = c.id AND m.created_at = c.last_message_at
		   AND (m.deliver_at IS NULL OR m.deliver_at <= m.created_at)
		 WHERE c.id IN (`+holes+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("last messages: %w", err)
	}
	defer rows.Close()
	best := map[string]int64{}
	for rows.Next() {
		var m Message
		var created int64
		var edited sql.NullInt64
		var deleted, forwarded, system int
		var replyTo sql.NullString
		var deliver sql.NullInt64
		var rowid int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.SenderName,
			&m.Body, &created, &edited, &deleted, &replyTo, &forwarded, &system,
			&m.Priority, &deliver, &m.Meta, &rowid); err != nil {
			return nil, err
		}
		m.System = system != 0
		if deliver.Valid {
			t := time.UnixMilli(deliver.Int64).UTC()
			m.DeliverAt = &t
		}
		if prev, ok := best[m.ConversationID]; ok && prev > rowid {
			continue
		}
		best[m.ConversationID] = rowid
		m.CreatedAt = time.UnixMilli(created).UTC()
		if edited.Valid {
			t := time.UnixMilli(edited.Int64).UTC()
			m.EditedAt = &t
		}
		m.Deleted = deleted != 0
		m.ReplyTo = replyTo.String
		m.Forwarded = forwarded != 0
		mm := m
		out[m.ConversationID] = &mm
	}
	return out, rows.Err()
}

func (r *SQLRepository) UnreadCount(ctx context.Context, convID, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT (`+unreadTruth+`) FROM messaging_members
		 WHERE conversation_id = ? AND user_id = ?`, convID, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("unread count: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------

type rowScanner interface{ Scan(dest ...any) error }

func scanConv(s rowScanner) (*Conversation, error) {
	var c Conversation
	var kind string
	var created, last int64
	var avatar string
	if err := s.Scan(&c.ID, &kind, &c.Title, &c.CreatedBy, &created, &last,
		&c.Topic, &c.Visibility, &c.MemberCount, &avatar, &c.PinnedMessageID, &c.MeetingRoom, &c.TeamID); err != nil {
		return nil, err
	}
	c.Avatar = avatar != ""
	c.Kind = Kind(kind)
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.LastMessageAt = time.UnixMilli(last).UTC()
	return &c, nil
}

func scanMsg(s rowScanner) (*Message, error) {
	var m Message
	var created int64
	var edited sql.NullInt64
	var deleted int
	var replyTo sql.NullString
	var forwarded, system int
	var deliver sql.NullInt64
	err := s.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.SenderName,
		&m.Body, &created, &edited, &deleted, &replyTo, &forwarded, &system,
		&m.Priority, &deliver, &m.Meta)
	if err != nil {
		return nil, err
	}
	m.Forwarded = forwarded != 0
	m.System = system != 0
	if deliver.Valid {
		t := time.UnixMilli(deliver.Int64).UTC()
		m.DeliverAt = &t
	}
	m.CreatedAt = time.UnixMilli(created).UTC()
	if edited.Valid {
		t := time.UnixMilli(edited.Int64).UTC()
		m.EditedAt = &t
	}
	m.Deleted = deleted != 0
	m.ReplyTo = replyTo.String
	return &m, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func prefixed(cols, p string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

func (r *SQLRepository) RecordCall(ctx context.Context, c *Call) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_calls (id, room, caller_id, callee_id, kind, started_at, answered)
		VALUES (?,?,?,?,?,?,0)`,
		c.ID, c.Room, c.CallerID, c.CalleeID, c.Kind, c.StartedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("record call: %w", err)
	}
	return nil
}

// AnswerCall marks the most recent unanswered call for a room. A decline is
// recorded as unanswered-and-closed, which the thread shows as missed.
func (r *SQLRepository) AnswerCall(ctx context.Context, room string, accepted bool) error {
	v := 0
	if accepted {
		v = 1
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE messaging_calls SET answered = ?
		WHERE id = (SELECT id FROM messaging_calls WHERE room = ? ORDER BY started_at DESC LIMIT 1)`,
		v, room)
	if err != nil {
		return fmt.Errorf("answer call: %w", err)
	}
	return nil
}

// CallsBetween lists calls in either direction between two accounts, newest
// first, with each call's duration read from the room's attendance -- the
// span from the first join to the last leave. Attendance is the scheduling
// module's table; both live in one database, and a cross-table read here is
// cheaper and more honest than a second copy of the same fact.
func (r *SQLRepository) CallsBetween(ctx context.Context, a, b string, limit int) ([]*Call, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.room, c.caller_id, c.callee_id, c.kind, c.started_at, c.answered,
		       COALESCE((SELECT MAX(att.left_at) - MIN(att.joined_at)
		                 FROM scheduling_attendance att
		                 WHERE att.room = c.room AND att.left_at IS NOT NULL), 0)
		FROM messaging_calls c
		WHERE (c.caller_id = ? AND c.callee_id = ?) OR (c.caller_id = ? AND c.callee_id = ?)
		ORDER BY c.started_at DESC
		LIMIT ?`, a, b, b, a, limit)
	if err != nil {
		return nil, fmt.Errorf("calls between: %w", err)
	}
	defer rows.Close()
	var out []*Call
	for rows.Next() {
		var c Call
		var started, durMs int64
		var answered int
		if err := rows.Scan(&c.ID, &c.Room, &c.CallerID, &c.CalleeID, &c.Kind, &started, &answered, &durMs); err != nil {
			return nil, err
		}
		c.StartedAt = time.UnixMilli(started).UTC()
		c.Answered = answered == 1
		if c.Answered && durMs > 0 {
			c.Seconds = durMs / 1000
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
