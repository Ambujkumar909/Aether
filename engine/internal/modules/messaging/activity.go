package messaging

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/wire"
)

// The activity feed: what happened that concerns you. Somebody mentioned
// you, replied to something you wrote, reacted to it, or added you to a team.
//
// One row per person per event, written when the event happens and capped
// at MaxActivity a person (the oldest go). A row names the message or team;
// the text, the conversation and its title are read through it at list
// time, so what the feed shows is what is stored: an edit shows the new
// text, a deleted message or team takes its rows with it, and a
// conversation you have left drops out of your feed (the membership check
// is in the one query every read goes through, the push included).

const (
	ActivityMention  = "mention"
	ActivityReply    = "reply"
	ActivityReaction = "reaction"
	ActivityTeam     = "team"

	// MaxActivity is the most rows one person keeps: Teams' feed is a few
	// weeks deep; this is several hundred events.
	MaxActivity = 500
	// MaxActivityPage is the most rows one request lists.
	MaxActivityPage = 100
	// MaxActivityRead is the most ids one "mark read" names.
	MaxActivityRead = 100
)

// Activity is one row of the feed as the client draws it.
type Activity struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ActorID   string    `json:"actorId"`
	ActorName string    `json:"actorName"`
	ConvID    string    `json:"conversationId,omitempty"`
	ConvKind  string    `json:"conversationKind,omitempty"`
	ConvTitle string    `json:"conversationTitle,omitempty"`
	TeamID    string    `json:"teamId,omitempty"`
	TeamName  string    `json:"teamName,omitempty"`
	MessageID string    `json:"messageId,omitempty"`
	RootID    string    `json:"rootId,omitempty"` // the thread a channel reply belongs to
	Emoji     string    `json:"emoji,omitempty"`
	Snippet   string    `json:"snippet,omitempty"`
	At        time.Time `json:"at"`
	Read      bool      `json:"read"`
}

// activityRow is what is written.
type activityRow struct {
	id, userID, kind, actorID, actorName, messageID, teamID, emoji string
	at                                                             time.Time
}

// --- repository -------------------------------------------------------------

// activityVisible: a message row is shown only while you are a member of its
// conversation; a team row always (deleting the team deletes it).
const activityVisible = `(a.message_id IS NULL OR EXISTS (
	SELECT 1 FROM messaging_members mm
	 WHERE mm.conversation_id = m.conversation_id AND mm.user_id = a.user_id))`

const activitySelect = `
	SELECT a.id, a.kind, a.actor_id, a.actor_name,
	       COALESCE(m.conversation_id, ''), COALESCE(c.kind, ''), COALESCE(c.title, ''),
	       COALESCE(t.id, ''), COALESCE(t.name, ''),
	       COALESCE(a.message_id, ''), COALESCE(m.reply_to, ''), a.emoji,
	       COALESCE(substr(m.body, 1, 200), ''), a.created_at, a.read
	  FROM messaging_activity a
	  LEFT JOIN messaging_messages m ON m.id = a.message_id
	  LEFT JOIN messaging_conversations c ON c.id = m.conversation_id
	  LEFT JOIN messaging_teams t ON t.id = COALESCE(a.team_id, NULLIF(c.team_id, ''))`

func scanActivity(rows interface{ Scan(...any) error }) (*Activity, error) {
	var x Activity
	var at int64
	var read int
	if err := rows.Scan(&x.ID, &x.Kind, &x.ActorID, &x.ActorName, &x.ConvID, &x.ConvKind, &x.ConvTitle,
		&x.TeamID, &x.TeamName, &x.MessageID, &x.RootID, &x.Emoji, &x.Snippet, &at, &read); err != nil {
		return nil, err
	}
	x.At = time.UnixMilli(at).UTC()
	x.Read = read == 1
	return &x, nil
}

func (r *SQLRepository) activityQuery(ctx context.Context, q string, args ...any) ([]*Activity, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	defer rows.Close()
	var out []*Activity
	for rows.Next() {
		x, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AddActivity writes rows and trims each recipient's feed to MaxActivity.
func (r *SQLRepository) AddActivity(ctx context.Context, rows []activityRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	users := map[string]bool{}
	for _, x := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messaging_activity (id, user_id, kind, actor_id, actor_name, message_id, team_id, emoji, created_at)
			VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
			x.id, x.userID, x.kind, x.actorID, x.actorName, x.messageID, x.teamID, x.emoji, x.at.UnixMilli()); err != nil {
			return fmt.Errorf("add activity: %w", err)
		}
		users[x.userID] = true
	}
	for u := range users {
		// The walk past the newest MaxActivity is the user's index, bounded.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM messaging_activity WHERE id IN (
				SELECT id FROM messaging_activity WHERE user_id = ?
				 ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?)`, u, MaxActivity); err != nil {
			return fmt.Errorf("trim activity: %w", err)
		}
	}
	return tx.Commit()
}

// ActivityPage lists one person's feed, newest first, after a (time, id)
// cursor. kind filters to one kind; unread to unread rows.
func (r *SQLRepository) ActivityPage(ctx context.Context, userID string, beforeAt int64, beforeID, kind string, unread bool, limit int) ([]*Activity, error) {
	q := activitySelect + ` WHERE a.user_id = ? AND ` + activityVisible
	args := []any{userID}
	if beforeAt > 0 {
		q += ` AND (a.created_at < ? OR (a.created_at = ? AND a.id < ?))`
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	if kind != "" {
		q += ` AND a.kind = ?`
		args = append(args, kind)
	}
	if unread {
		q += ` AND a.read = 0`
	}
	q += ` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`
	args = append(args, limit)
	return r.activityQuery(ctx, q, args...)
}

// ActivityByIDs reads rows by id, through the same visibility rule.
func (r *SQLRepository) ActivityByIDs(ctx context.Context, ids []string) ([]*Activity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	holes, args := placeholders(ids)
	return r.activityQuery(ctx, activitySelect+` WHERE a.id IN (`+holes+`) AND `+activityVisible+
		` ORDER BY a.created_at, a.id`, args...)
}

// ActivityUnread counts what the badge shows, by the same visibility rule.
func (r *SQLRepository) ActivityUnread(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messaging_activity a
		  LEFT JOIN messaging_messages m ON m.id = a.message_id
		 WHERE a.user_id = ? AND a.read = 0 AND `+activityVisible, userID).Scan(&n)
	return n, err
}

func returningIDs(ctx context.Context, r *SQLRepository, q string, args ...any) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
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

// MarkActivityRead marks the named rows (or all of them) read, and says which
// changed.
func (r *SQLRepository) MarkActivityRead(ctx context.Context, userID string, ids []string, all bool) ([]string, error) {
	if all {
		return returningIDs(ctx, r, `UPDATE messaging_activity SET read = 1 WHERE user_id = ? AND read = 0 RETURNING id`, userID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	holes, args := placeholders(ids)
	return returningIDs(ctx, r, `UPDATE messaging_activity SET read = 1
		 WHERE user_id = ? AND read = 0 AND id IN (`+holes+`) RETURNING id`, append([]any{userID}, args...)...)
}

// MarkActivityReadIn: reading a conversation reads its mentions and replies.
// A reaction is not read by reading the conversation: it is not in it.
func (r *SQLRepository) MarkActivityReadIn(ctx context.Context, userID, convID string) ([]string, error) {
	return returningIDs(ctx, r, `UPDATE messaging_activity SET read = 1
		 WHERE user_id = ? AND read = 0 AND kind IN ('mention', 'reply')
		   AND EXISTS (SELECT 1 FROM messaging_messages m WHERE m.id = messaging_activity.message_id AND m.conversation_id = ?)
		 RETURNING id`, userID, convID)
}

// RemoveReactionActivity takes back the row a reaction made.
func (r *SQLRepository) RemoveReactionActivity(ctx context.Context, messageID, actorID, emoji string) (userID, id string, err error) {
	err = r.db.QueryRowContext(ctx, `
		DELETE FROM messaging_activity
		 WHERE message_id = ? AND kind = 'reaction' AND actor_id = ? AND emoji = ?
		 RETURNING user_id, id`, messageID, actorID, emoji).Scan(&userID, &id)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return "", "", nil
	}
	return userID, id, err
}

// --- service ----------------------------------------------------------------

// activityForMessage records what a newly delivered message means for the
// people it names: a mention for each person mentioned, and a reply for the
// author of the message it answers (unless that author is also mentioned:
// one event, one row).
func (s *Service) activityForMessage(ctx context.Context, m *Message) {
	if m == nil || m.System || m.Deleted {
		return
	}
	var rows []activityRow
	named := map[string]bool{m.SenderID: true}
	for _, uid := range m.Mentions {
		if named[uid] {
			continue
		}
		named[uid] = true
		rows = append(rows, activityRow{id: newID(), userID: uid, kind: ActivityMention, actorID: m.SenderID, actorName: m.SenderName, messageID: m.ID, at: m.CreatedAt})
	}
	if m.ReplyTo != "" {
		if q, err := s.repo.MessageByID(ctx, m.ReplyTo); err == nil && !named[q.SenderID] && !q.System {
			rows = append(rows, activityRow{id: newID(), userID: q.SenderID, kind: ActivityReply, actorID: m.SenderID, actorName: m.SenderName, messageID: m.ID, at: m.CreatedAt})
		}
	}
	s.recordActivity(ctx, rows)
}

// recordActivity writes rows and pushes each to its person.
func (s *Service) recordActivity(ctx context.Context, rows []activityRow) {
	if len(rows) == 0 {
		return
	}
	if err := s.repo.AddActivity(ctx, rows); err != nil {
		s.log.Warn("activity: could not record", "err", err)
		return
	}
	ids := make([]string, len(rows))
	owner := map[string]string{}
	for i, x := range rows {
		ids[i] = x.id
		owner[x.id] = x.userID
	}
	items, err := s.repo.ActivityByIDs(ctx, ids)
	if err != nil {
		return
	}
	for _, it := range items {
		uid := owner[it.ID]
		n, _ := s.repo.ActivityUnread(ctx, uid)
		s.hub.PushActivity(uid, it, n)
	}
}

// Activities is one page of a person's feed and their unread count.
func (s *Service) Activities(ctx context.Context, a Actor, before, kind string, unread bool, limit int) ([]*Activity, int, error) {
	switch kind {
	case "", ActivityMention, ActivityReply, ActivityReaction, ActivityTeam:
	default:
		return nil, 0, ErrBadRequest
	}
	if limit <= 0 || limit > MaxActivityPage {
		limit = 50
	}
	var at int64
	var id string
	if before != "" {
		ts, rest, ok := strings.Cut(before, ":")
		n, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || n <= 0 {
			return nil, 0, ErrBadRequest
		}
		at, id = n, rest
	}
	items, err := s.repo.ActivityPage(ctx, a.ID, at, id, kind, unread, limit)
	if err != nil {
		return nil, 0, err
	}
	n, err := s.repo.ActivityUnread(ctx, a.ID)
	return items, n, err
}

// ReadActivity marks rows read (or all) and tells the person's devices.
func (s *Service) ReadActivity(ctx context.Context, a Actor, ids []string, all bool) (int, error) {
	if len(ids) > MaxActivityRead {
		return 0, ErrBadRequest
	}
	changed, err := s.repo.MarkActivityRead(ctx, a.ID, ids, all)
	if err != nil {
		return 0, err
	}
	n, err := s.repo.ActivityUnread(ctx, a.ID)
	if err != nil {
		return 0, err
	}
	if len(changed) > 0 {
		s.hub.PushActivityRead(a.ID, changed, n)
	}
	return n, nil
}

// readActivityIn follows a conversation being read.
func (s *Service) readActivityIn(ctx context.Context, userID, convID string) {
	changed, err := s.repo.MarkActivityReadIn(ctx, userID, convID)
	if err != nil || len(changed) == 0 {
		return
	}
	n, _ := s.repo.ActivityUnread(ctx, userID)
	s.hub.PushActivityRead(userID, changed, n)
}

// reactionActivity: a reaction on somebody else's message is theirs to see;
// taking it back takes the row back.
func (s *Service) reactionActivity(ctx context.Context, a Actor, m *Message, emoji string, on bool) {
	if m.SenderID == a.ID || m.System {
		return
	}
	// One row per reactor, message and emoji, whatever the toggling.
	uid, id, err := s.repo.RemoveReactionActivity(ctx, m.ID, a.ID, emoji)
	if err != nil {
		return
	}
	if on {
		s.recordActivity(ctx, []activityRow{{id: newID(), userID: m.SenderID, kind: ActivityReaction, actorID: a.ID, actorName: a.Name, messageID: m.ID, emoji: emoji, at: time.Now().UTC()}})
		if id == "" {
			return
		}
	}
	if id != "" {
		n, _ := s.repo.ActivityUnread(ctx, uid)
		s.hub.PushActivityGone(uid, id, n)
	}
}

// --- push -------------------------------------------------------------------

// OpActivity frames, by the 2-bit kind after the header:
//
//	0 item:    id, kind, actor id, actor name, conversation id, kind, title,
//	           team id, team name, message id, root id, emoji (all interned),
//	           time (ms), read, snippet (blob), then the unread count.
//	1 read:    count, that many ids, then the unread count.
//	2 gone:    id, then the unread count.
//
// The item carries everything the feed draws (the push-tail rule): a client
// with the feed open adds it without asking the server for anything.
const (
	actItem = 0
	actRead = 1
	actGone = 2
)

func (h *Hub) PushActivity(userID string, it *Activity, unread int) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpActivity, 0, false)
		w.Bits(actItem, 2)
		w.Bits(0, 6)
		for _, s := range []string{it.ID, it.Kind, it.ActorID, it.ActorName, it.ConvID, it.ConvKind, it.ConvTitle,
			it.TeamID, it.TeamName, it.MessageID, it.RootID, it.Emoji} {
			w.Str(s)
		}
		w.Uvarint(uint64(it.At.UnixMilli()))
		w.Bool(it.Read)
		w.Bits(0, 7)
		w.Blob([]byte(it.Snippet))
		w.Uvarint(uint64(unread))
	})
}

func (h *Hub) PushActivityRead(userID string, ids []string, unread int) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpActivity, 0, false)
		w.Bits(actRead, 2)
		w.Bits(0, 6)
		w.Uvarint(uint64(len(ids)))
		for _, id := range ids {
			w.Str(id)
		}
		w.Uvarint(uint64(unread))
	})
}

func (h *Hub) PushActivityGone(userID, id string, unread int) {
	h.push(userID, func(w *wire.Writer) {
		w.WriteHeader(wire.OpActivity, 0, false)
		w.Bits(actGone, 2)
		w.Bits(0, 6)
		w.Str(id)
		w.Uvarint(uint64(unread))
	})
}

// --- http -------------------------------------------------------------------

func (h *Handler) activityRoutes(mux *http.ServeMux, p func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/messaging/activity", p(h.activity))
	mux.Handle("POST /api/messaging/activity/read", p(h.activityRead))
}

func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, n, err := h.svc.Activities(r.Context(), h.actor(r), q.Get("before"), q.Get("kind"), q.Get("unread") == "1", limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if items == nil {
		items = []*Activity{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "unread": n})
}

func (h *Handler) activityRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
		All bool     `json:"all"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	n, err := h.svc.ReadActivity(r.Context(), h.actor(r), req.IDs, req.All)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"unread": n})
}
