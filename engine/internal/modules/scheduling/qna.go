package scheduling

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Q&A in a meeting, as Teams runs it (strings from its package: "Close Q&A",
// "Anonymous", "Like", "Reply", "Pin", "Answered", "Dismissed", "Publish";
// "Organizers can customize Q&A to allow anonymous posts, review questions
// before publishing"). Teams hosts meeting Q&A in a separate service; here it
// is stored with the meeting's room and pushed over the meeting's socket.
//
// Who may do what is decided by the meeting transport (LiveMember): only
// people in the meeting now read or post, and only its organizer and
// co-organizers moderate. A question posted anonymously carries no author
// for anyone, the organizer included -- that is what anonymous means.

const (
	MaxQuestionLen        = 1000
	MaxReplyLen           = 1000
	MaxQuestionsPerRoom   = 1000
	MaxRepliesPerQuestion = 200
)

var (
	ErrQnAClosed   = errors.New("scheduling: Q&A is closed")
	ErrQnABody     = errors.New("scheduling: a question or reply must be 1 to 1000 characters")
	ErrQnAFull     = errors.New("scheduling: this meeting has reached its limit of questions or replies")
	ErrQnAAnon     = errors.New("scheduling: anonymous posts are not allowed in this meeting")
	ErrQnANotFound = errors.New("scheduling: no such question")
	ErrQnAAction   = errors.New("scheduling: unknown moderation action")
)

// Question as a viewer sees it.
type Question struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // "" when anonymous
	Anonymous bool      `json:"anonymous"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"` // published | pending | dismissed
	Answered  bool      `json:"answered"`
	Pinned    bool      `json:"pinned"`
	Likes     int       `json:"likes"`
	Replies   []Reply   `json:"replies"`
	// Per viewer: only in a list read, never in a push (the client keeps its
	// own, so one serialisation serves the whole room).
	Mine  bool `json:"mine,omitempty"`
	Liked bool `json:"liked,omitempty"`

	authorID string
}

type Reply struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Anonymous bool      `json:"anonymous"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// QnASettings are the organizer's choices for a meeting's Q&A.
type QnASettings struct {
	Moderation bool `json:"moderation"` // review questions before they are published
	Anonymous  bool `json:"anonymous"`  // allow anonymous posts
	Closed     bool `json:"closed"`     // "Close Q&A"
}

// Viewer is who is asking, as the meeting transport sees them.
type Viewer struct {
	ID, Name string
	Host     bool
}

// --- storage ----------------------------------------------------------------

func (r *SQLRepository) QnASettings(ctx context.Context, room string) (QnASettings, error) {
	var s QnASettings
	err := r.db.QueryRowContext(ctx, `SELECT moderation, anonymous, closed FROM meeting_qna_settings WHERE room = ?`, room).
		Scan(&s.Moderation, &s.Anonymous, &s.Closed)
	if errors.Is(err, sql.ErrNoRows) {
		return QnASettings{Anonymous: true}, nil // Teams' default allows anonymous posts
	}
	return s, err
}

func (r *SQLRepository) SetQnASettings(ctx context.Context, room string, s QnASettings) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO meeting_qna_settings (room, moderation, anonymous, closed) VALUES (?,?,?,?)
		ON CONFLICT(room) DO UPDATE SET moderation=excluded.moderation, anonymous=excluded.anonymous, closed=excluded.closed`,
		room, s.Moderation, s.Anonymous, s.Closed)
	return err
}

func (r *SQLRepository) countQuestions(ctx context.Context, room string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM meeting_qna_questions WHERE room = ?`, room).Scan(&n)
	return n, err
}

func (r *SQLRepository) InsertQuestion(ctx context.Context, room string, q *Question) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO meeting_qna_questions (id, room, author_id, author_name, anonymous, body, created_at, status, answered, pinned)
		VALUES (?,?,?,?,?,?,?,?,0,0)`,
		q.ID, room, q.authorID, q.Author, q.Anonymous, q.Body, q.CreatedAt.UnixMilli(), q.Status)
	return err
}

func (r *SQLRepository) InsertReply(ctx context.Context, qid, authorID string, rp *Reply) error {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM meeting_qna_replies WHERE question_id = ?`, qid).Scan(&n); err != nil {
		return err
	}
	if n >= MaxRepliesPerQuestion {
		return ErrQnAFull
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO meeting_qna_replies (id, question_id, author_id, author_name, anonymous, body, created_at)
		VALUES (?,?,?,?,?,?,?)`, rp.ID, qid, authorID, rp.Author, rp.Anonymous, rp.Body, rp.CreatedAt.UnixMilli())
	return err
}

// ToggleLike likes or unlikes; reports whether it is now liked.
func (r *SQLRepository) ToggleLike(ctx context.Context, qid, user string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM meeting_qna_likes WHERE question_id = ? AND user_id = ?`, qid, user)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO meeting_qna_likes (question_id, user_id) VALUES (?,?)`, qid, user)
	return err == nil, err
}

func (r *SQLRepository) SetQuestionFlags(ctx context.Context, room, qid string, status *string, answered, pinned *bool) error {
	sets, args := []string{}, []any{}
	if status != nil {
		sets, args = append(sets, "status = ?"), append(args, *status)
	}
	if answered != nil {
		sets, args = append(sets, "answered = ?"), append(args, *answered)
	}
	if pinned != nil {
		sets, args = append(sets, "pinned = ?"), append(args, *pinned)
	}
	args = append(args, qid, room)
	res, err := r.db.ExecContext(ctx, `UPDATE meeting_qna_questions SET `+strings.Join(sets, ", ")+` WHERE id = ? AND room = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrQnANotFound
	}
	return nil
}

// Questions reads a room's questions -- or one, when qid is set -- with their
// like counts and replies, in three queries however many there are (rule 7:
// no query per question). Index-backed: questions by (room, created_at),
// replies by (question_id, created_at), likes by their primary key.
func (r *SQLRepository) Questions(ctx context.Context, room, qid, viewer string) ([]*Question, error) {
	q := `SELECT q.id, q.author_id, q.author_name, q.anonymous, q.body, q.created_at, q.status, q.answered, q.pinned,
	             (SELECT count(*) FROM meeting_qna_likes l WHERE l.question_id = q.id),
	             EXISTS (SELECT 1 FROM meeting_qna_likes l WHERE l.question_id = q.id AND l.user_id = ?)
	      FROM meeting_qna_questions q WHERE q.room = ?`
	args := []any{viewer, room}
	if qid != "" {
		q += ` AND q.id = ?`
		args = append(args, qid)
	}
	q += ` ORDER BY q.created_at, q.rowid`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("questions: %w", err)
	}
	defer rows.Close()
	var out []*Question
	byID := map[string]*Question{}
	for rows.Next() {
		var x Question
		var created int64
		if err := rows.Scan(&x.ID, &x.authorID, &x.Author, &x.Anonymous, &x.Body, &created, &x.Status, &x.Answered, &x.Pinned, &x.Likes, &x.Liked); err != nil {
			return nil, err
		}
		x.CreatedAt = time.UnixMilli(created).UTC()
		x.Replies = []Reply{}
		out = append(out, &x)
		byID[x.ID] = &x
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	rq := `SELECT r.id, r.question_id, r.author_name, r.anonymous, r.body, r.created_at
	       FROM meeting_qna_replies r JOIN meeting_qna_questions q ON q.id = r.question_id
	       WHERE q.room = ?`
	rargs := []any{room}
	if qid != "" {
		rq += ` AND q.id = ?`
		rargs = append(rargs, qid)
	}
	rq += ` ORDER BY r.created_at, r.rowid`
	rrows, err := r.db.QueryContext(ctx, rq, rargs...)
	if err != nil {
		return nil, fmt.Errorf("replies: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var rp Reply
		var qid2 string
		var created int64
		if err := rrows.Scan(&rp.ID, &qid2, &rp.Author, &rp.Anonymous, &rp.Body, &created); err != nil {
			return nil, err
		}
		rp.CreatedAt = time.UnixMilli(created).UTC()
		if rp.Anonymous {
			rp.Author = ""
		}
		if x := byID[qid2]; x != nil {
			x.Replies = append(x.Replies, rp)
		}
	}
	return out, rrows.Err()
}

// --- the service ------------------------------------------------------------

// visibleTo: published to everyone; pending to its author and the hosts;
// dismissed to the hosts.
func (q *Question) visibleTo(v Viewer) bool {
	switch q.Status {
	case "published":
		return true
	default:
		// Pending and dismissed: the hosts, and the author, who should know
		// what became of their question rather than watch it vanish.
		return v.Host || q.authorID == v.ID
	}
}

// public strips what no viewer may see: an anonymous author.
func (q *Question) public() *Question {
	c := *q
	if c.Anonymous {
		c.Author = ""
	}
	c.Mine, c.Liked = false, false
	return &c
}

func (s *Service) viewer(room, userID string) (Viewer, bool) {
	if s.LiveMember == nil || userID == "" {
		return Viewer{}, false
	}
	name, host, ok := s.LiveMember(room, userID)
	return Viewer{ID: userID, Name: name, Host: host}, ok
}

// QnA lists what this viewer may see, and the settings.
func (s *Service) QnA(ctx context.Context, a Actor, room string) ([]*Question, QnASettings, error) {
	v, ok := s.viewer(room, a.ID)
	if !ok {
		return nil, QnASettings{}, ErrForbidden
	}
	set, err := s.repo.QnASettings(ctx, room)
	if err != nil {
		return nil, set, err
	}
	all, err := s.repo.Questions(ctx, room, "", v.ID)
	if err != nil {
		return nil, set, err
	}
	out := make([]*Question, 0, len(all))
	for _, q := range all {
		if q.visibleTo(v) {
			p := q.public()
			p.Mine, p.Liked = q.authorID == v.ID, q.Liked
			out = append(out, p)
		}
	}
	return out, set, nil
}

func cleanBody(b string, max int) (string, error) {
	b = strings.TrimSpace(b)
	if b == "" || !utf8.ValidString(b) || utf8.RuneCountInString(b) > max {
		return "", ErrQnABody
	}
	return b, nil
}

// Ask posts a question: published at once, or pending when the organizer
// reviews questions first.
func (s *Service) Ask(ctx context.Context, a Actor, room, body string, anonymous bool) (*Question, error) {
	v, ok := s.viewer(room, a.ID)
	if !ok {
		return nil, ErrForbidden
	}
	body, err := cleanBody(body, MaxQuestionLen)
	if err != nil {
		return nil, err
	}
	set, err := s.repo.QnASettings(ctx, room)
	if err != nil {
		return nil, err
	}
	if set.Closed {
		return nil, ErrQnAClosed
	}
	if anonymous && !set.Anonymous {
		return nil, ErrQnAAnon
	}
	if n, err := s.repo.countQuestions(ctx, room); err != nil {
		return nil, err
	} else if n >= MaxQuestionsPerRoom {
		return nil, ErrQnAFull
	}
	status := "published"
	if set.Moderation && !v.Host {
		status = "pending"
	}
	q := &Question{ID: newID(), Author: v.Name, Anonymous: anonymous, Body: body, CreatedAt: time.Now().UTC(), Status: status, Replies: []Reply{}, authorID: v.ID}
	if err := s.repo.InsertQuestion(ctx, room, q); err != nil {
		return nil, err
	}
	s.pushQuestion(ctx, room, q.ID)
	out := q.public()
	out.Mine = true
	return out, nil
}

// Reply answers in a question's thread; only a question the viewer can see.
func (s *Service) ReplyTo(ctx context.Context, a Actor, room, qid, body string, anonymous bool) error {
	v, ok := s.viewer(room, a.ID)
	if !ok {
		return ErrForbidden
	}
	body, err := cleanBody(body, MaxReplyLen)
	if err != nil {
		return err
	}
	set, err := s.repo.QnASettings(ctx, room)
	if err != nil {
		return err
	}
	if set.Closed {
		return ErrQnAClosed
	}
	if anonymous && !set.Anonymous {
		return ErrQnAAnon
	}
	if q, err := s.one(ctx, room, qid, v.ID); err != nil || !q.visibleTo(v) || q.Status != "published" {
		return ErrQnANotFound
	}
	if err := s.repo.InsertReply(ctx, qid, v.ID, &Reply{ID: newID(), Author: v.Name, Anonymous: anonymous, Body: body, CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	s.pushQuestion(ctx, room, qid)
	return nil
}

// Like toggles the viewer's like on a published question.
func (s *Service) Like(ctx context.Context, a Actor, room, qid string) (bool, error) {
	v, ok := s.viewer(room, a.ID)
	if !ok {
		return false, ErrForbidden
	}
	if q, err := s.one(ctx, room, qid, v.ID); err != nil || q.Status != "published" {
		return false, ErrQnANotFound
	}
	liked, err := s.repo.ToggleLike(ctx, qid, v.ID)
	if err != nil {
		return false, err
	}
	s.pushQuestion(ctx, room, qid)
	return liked, nil
}

// Moderate: "Publish", "Dismiss", "Mark as answered", "Pin", and back.
// Organizer and co-organizers only.
func (s *Service) Moderate(ctx context.Context, a Actor, room, qid, action string) error {
	v, ok := s.viewer(room, a.ID)
	if !ok || !v.Host {
		return ErrForbidden
	}
	t, f := true, false
	var status *string
	var answered, pinned *bool
	switch action {
	case "publish":
		x := "published"
		status = &x
	case "dismiss":
		x := "dismissed"
		status = &x
	case "answered":
		answered = &t
	case "unanswered":
		answered = &f
	case "pin":
		pinned = &t
	case "unpin":
		pinned = &f
	default:
		return ErrQnAAction
	}
	if err := s.repo.SetQuestionFlags(ctx, room, qid, status, answered, pinned); err != nil {
		return err
	}
	s.pushQuestion(ctx, room, qid)
	return nil
}

// SetQnA changes the settings (organizer and co-organizers) and tells the room.
func (s *Service) SetQnA(ctx context.Context, a Actor, room string, set QnASettings) error {
	v, ok := s.viewer(room, a.ID)
	if !ok || !v.Host {
		return ErrForbidden
	}
	if err := s.repo.SetQnASettings(ctx, room, set); err != nil {
		return err
	}
	if s.QnAPush != nil {
		body, _ := json.Marshal(map[string]any{"settings": set})
		s.QnAPush(room, body, false, "")
	}
	return nil
}

func (s *Service) one(ctx context.Context, room, qid, viewer string) (*Question, error) {
	qs, err := s.repo.Questions(ctx, room, qid, viewer)
	if err != nil {
		return nil, err
	}
	if len(qs) != 1 {
		return nil, ErrQnANotFound
	}
	return qs[0], nil
}

// pushQuestion sends the question as it now is to the room: whole, so the
// panel can draw it without asking (the push-tail rule), and once for
// everybody -- per-viewer fields are the client's own. A question not
// published goes only to the hosts and its author.
func (s *Service) pushQuestion(ctx context.Context, room, qid string) {
	if s.QnAPush == nil {
		return
	}
	q, err := s.one(ctx, room, qid, "")
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"question": q.public()})
	s.QnAPush(room, body, q.Status != "published", q.authorID)
}
