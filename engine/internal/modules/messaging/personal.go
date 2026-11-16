package messaging

import "github.com/aether-rtc/aether/internal/platform/database"

// PersonalSections lists the rows of this module that are one person's, for
// their own export (database.Section): what they wrote and what is stored
// about them. Other people's messages are not theirs to take away, even in a
// conversation they are in; a message of their own that they deleted is
// gone and is not listed.
//
// Every query reaches its rows through an index on the person (the plan
// test holds each to that): an export must not read a table of everybody's
// rows to find one person's.
func PersonalSections(userID string) []database.Section {
	q := func(name, query string) database.Section {
		return database.Section{Name: name, Query: query, Args: []any{userID}}
	}
	return []database.Section{
		q("conversations", `SELECT c.id, c.kind, c.title, m.role, m.joined_at, m.last_read, m.favourite, m.muted
			  FROM messaging_members m JOIN messaging_conversations c ON c.id = m.conversation_id
			 WHERE m.user_id = ?`),
		q("messages", `SELECT id, conversation_id, body, created_at, edited_at, reply_to, priority, deliver_at
			  FROM messaging_messages
			 WHERE sender_id = ? AND deleted = 0 AND system = 0 ORDER BY created_at`),
		q("message_earlier_versions", `SELECT e.message_id, e.edited_at, e.body
			  FROM messaging_messages m JOIN messaging_message_edits e ON e.message_id = m.id
			 WHERE m.sender_id = ? AND m.deleted = 0`),
		q("files", `SELECT id, message_id, conversation_id, name, mime, size, created_at
			  FROM messaging_attachments WHERE uploader_id = ?`),
		q("reactions", `SELECT message_id, emoji, created_at FROM messaging_reactions WHERE user_id = ?`),
		q("poll_votes", `SELECT message_id, option, voted_at FROM messaging_poll_votes WHERE user_id = ?`),
		q("saved_messages", `SELECT message_id, created_at FROM messaging_saved WHERE user_id = ?`),
		q("mentions_of_you", `SELECT message_id FROM messaging_mentions WHERE user_id = ?`),
		q("blocked_people", `SELECT blocked_id, created_at FROM messaging_blocks WHERE user_id = ?`),
		q("status", `SELECT status, note, until, updated_at FROM messaging_status WHERE user_id = ?`),
		q("last_seen", `SELECT last_seen FROM messaging_presence WHERE user_id = ?`),
		q("out_of_office", `SELECT message, starts_at, ends_at, updated_at FROM messaging_ooo WHERE user_id = ?`),
		q("notification_settings", `SELECT direct, grp, mention, reply, reaction, preview, email, updated_at
			  FROM messaging_notify_prefs WHERE user_id = ?`),
		q("teams", `SELECT t.id, t.name, tm.role, tm.joined_at
			  FROM messaging_team_members tm JOIN messaging_teams t ON t.id = tm.team_id
			 WHERE tm.user_id = ?`),
		q("activity", `SELECT kind, actor_name, message_id, team_id, emoji, created_at, read
			  FROM messaging_activity WHERE user_id = ? ORDER BY created_at DESC, id DESC`),
		q("calls_made", `SELECT room, callee_id, kind, started_at, answered FROM messaging_calls WHERE caller_id = ?`),
		q("calls_received", `SELECT room, caller_id, kind, started_at, answered FROM messaging_calls WHERE callee_id = ?`),
	}
}
