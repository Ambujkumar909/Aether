package scheduling

import "github.com/aether-rtc/aether/internal/platform/database"

// PersonalSections lists the rows of this module that are one person's, for
// their own export (database.Section): meetings they organise or are invited
// to (an invitation is kept by address), when they attended, and what they
// asked, answered and said where a transcript was kept. Meeting notes are a
// shared document and are not anybody's.
func PersonalSections(userID, email string) []database.Section {
	q := func(name, query string, arg any) database.Section {
		return database.Section{Name: name, Query: query, Args: []any{arg}}
	}
	return []database.Section{
		q("meetings_organised", `SELECT id, room, title, description, category, status, starts_at, ends_at, recurrence, created_at
			  FROM scheduling_meetings WHERE organizer_email = ?`, email),
		q("meeting_invitations", `SELECT m.id, m.title, m.starts_at, m.ends_at, m.organizer_email, i.response, i.co_organizer
			  FROM scheduling_invitees i JOIN scheduling_meetings m ON m.id = i.meeting_id
			 WHERE i.email = ?`, email),
		q("meeting_attendance", `SELECT room, name, joined_at, left_at
			  FROM scheduling_attendance WHERE user_id = ? ORDER BY joined_at DESC`, userID),
		q("questions_asked", `SELECT id, room, body, anonymous, created_at FROM meeting_qna_questions WHERE author_id = ?`, userID),
		q("question_replies", `SELECT id, question_id, body, anonymous, created_at FROM meeting_qna_replies WHERE author_id = ?`, userID),
		q("transcript_lines", `SELECT room, session, n, body, at FROM meeting_transcript WHERE user_id = ?`, userID),
	}
}
