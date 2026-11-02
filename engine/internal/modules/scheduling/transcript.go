package scheduling

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The meeting transcript (Teams: "Start transcription", "Transcript",
// "Download transcript"). Each speaker's own browser turns their own speech
// into text, so every line arrives attributed; the meeting socket hands the
// final lines here (signal/recording.go), and this keeps them, per meeting
// session -- the room's epoch, which is new each time a room fills from empty.
//
// Who may read it: anyone in the meeting now, and anyone who attended it.

const (
	MaxTranscriptLines   = 20000 // a session; eight hours of speech is about 10,000
	MaxTranscriptLineLen = 500   // bytes, as on the wire
)

var ErrTranscriptFull = errors.New("scheduling: the transcript has reached its limit of lines")

// TranscriptLine is one final line of speech.
type TranscriptLine struct {
	N    int       `json:"n"`
	Name string    `json:"name"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// --- storage ----------------------------------------------------------------

// AppendTranscriptLine numbers the line and stores it in one statement, so
// two speakers at once cannot take the same number and the bound holds.
func (r *SQLRepository) AppendTranscriptLine(ctx context.Context, room string, session int64, userID, name, text string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO meeting_transcript (room, session, n, user_id, name, body, at)
		SELECT ?, ?, COALESCE(MAX(n), 0) + 1, ?, ?, ?, ?
		FROM meeting_transcript WHERE room = ? AND session = ?
		HAVING COALESCE(MAX(n), 0) < ?`,
		room, session, userID, name, text, at.UnixMilli(), room, session, MaxTranscriptLines)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// LatestTranscriptSession is the most recent session with a transcript, 0 if none.
func (r *SQLRepository) LatestTranscriptSession(ctx context.Context, room string) (int64, error) {
	var s sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT MAX(session) FROM meeting_transcript WHERE room = ?`, room).Scan(&s)
	return s.Int64, err
}

func (r *SQLRepository) TranscriptLines(ctx context.Context, room string, session int64) ([]TranscriptLine, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT n, name, body, at FROM meeting_transcript
		WHERE room = ? AND session = ? ORDER BY n LIMIT ?`, room, session, MaxTranscriptLines)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TranscriptLine{}
	for rows.Next() {
		var l TranscriptLine
		var at int64
		if err := rows.Scan(&l.N, &l.Name, &l.Text, &at); err != nil {
			return nil, err
		}
		l.At = time.UnixMilli(at).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// Attended reports whether an account was ever in this room.
func (r *SQLRepository) Attended(ctx context.Context, room, userID string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM scheduling_attendance WHERE room = ? AND user_id = ? LIMIT 1`, room, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// --- service ----------------------------------------------------------------

// AppendTranscript stores one final line. Called by the meeting socket,
// which has already checked who is speaking; the bounds are checked again.
func (s *Service) AppendTranscript(ctx context.Context, room string, session time.Time, userID, name, text string, at time.Time) error {
	text = strings.TrimSpace(text)
	if room == "" || len(room) > MaxRoomIDLen || text == "" || len(text) > MaxTranscriptLineLen || !utf8.ValidString(text) {
		return nil
	}
	ok, err := s.repo.AppendTranscriptLine(ctx, room, session.UnixMilli(), userID, name, text, at.UTC())
	if err == nil && !ok {
		return ErrTranscriptFull
	}
	return err
}

// Transcript returns a session's lines (the latest session when session is
// 0) to someone in the meeting now or who attended it.
func (s *Service) Transcript(ctx context.Context, a Actor, room string, session int64) (int64, []TranscriptLine, error) {
	if room == "" || len(room) > MaxRoomIDLen || a.ID == "" {
		return 0, nil, ErrForbidden
	}
	if _, live := s.viewer(room, a.ID); !live {
		ok, err := s.repo.Attended(ctx, room, a.ID)
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			return 0, nil, ErrForbidden
		}
	}
	if session <= 0 {
		var err error
		if session, err = s.repo.LatestTranscriptSession(ctx, room); err != nil || session == 0 {
			return 0, []TranscriptLine{}, err
		}
	}
	lines, err := s.repo.TranscriptLines(ctx, room, session)
	return session, lines, err
}

// TranscriptVTT is the transcript as WebVTT, Teams' download format: one cue
// a line, timed from the start of the session, the speaker as a voice tag.
// Each cue ends where the next begins (a line has no end of its own), the
// last five seconds after it starts.
func TranscriptVTT(session int64, lines []TranscriptLine) []byte {
	var b bytes.Buffer
	b.WriteString("WEBVTT\n")
	start := time.UnixMilli(session)
	for i, l := range lines {
		from := l.At.Sub(start)
		to := from + 5*time.Second
		if i+1 < len(lines) {
			if next := lines[i+1].At.Sub(start); next > from {
				to = next
			}
		}
		fmt.Fprintf(&b, "\n%d\n%s --> %s\n<v %s>%s</v>\n", l.N, vttTime(from), vttTime(to), vttEscape(l.Name), vttEscape(l.Text))
	}
	return b.Bytes()
}

// TranscriptText is the plain-text download: "[hh:mm:ss] Name: text".
func TranscriptText(session int64, lines []TranscriptLine) []byte {
	var b bytes.Buffer
	start := time.UnixMilli(session)
	for _, l := range lines {
		d := l.At.Sub(start)
		if d < 0 {
			d = 0
		}
		fmt.Fprintf(&b, "[%02d:%02d:%02d] %s: %s\n", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, l.Name, l.Text)
	}
	return b.Bytes()
}

func vttTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// A cue's text may not contain "-->" or a blank line, and "<" "&" are markup.
var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", " ", "\n", " ")

func vttEscape(s string) string { return vttEscaper.Replace(s) }
