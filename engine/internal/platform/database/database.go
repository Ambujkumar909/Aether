// Package database owns the single database handle and the schema.
//
// One process, one database. Modules own their own tables, prefixed with the
// module name, and never write to another module's tables -- but they share
// this connection, because a transaction that spans two modules is a normal
// and desirable thing and splitting the handle would make it impossible.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the shared handle.
type DB struct {
	*sql.DB
	path string
}

// Open connects and applies the schema.
//
// SQLite here is a deliberate choice rather than a placeholder. It removes
// the operational burden of a separate database process for single-node
// deployments, it is genuinely fast for this workload, and the repository
// interfaces are written so a Postgres implementation slots in without
// touching a service or a handler.
func Open(ctx context.Context, path string) (*DB, error) {
	// The pragmas matter more than the driver choice.
	//
	//   WAL         readers do not block the writer, which is what lets an
	//               audit query run while a login is being recorded.
	//   busy_timeout SQLite returns SQLITE_BUSY immediately by default, which
	//               surfaces as spurious errors under any concurrency at all.
	//   foreign_keys off by default in SQLite, which quietly turns every
	//               referential constraint into a comment.
	//   synchronous NORMAL is durable under application crash and loses only
	//               on OS crash, which is the right trade for an audit log
	//               that is also on the hot path of every login.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite takes a single writer. More than one open write connection
	// produces lock contention rather than throughput.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	db := &DB{DB: sqlDB, path: path}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Path reports the database file location.
func (db *DB) Path() string { return db.path }

// migration is one forward-only schema step.
type migration struct {
	name string
	stmt string
}

// migrations run in order, exactly once each, recorded in schema_migrations.
//
// Forward only, and never edited after shipping. A migration that has run
// somewhere is history; changing it means two deployments disagree about what
// the schema is while both believe they are up to date.
var migrations = []migration{
	{"0001_identity_users", `
		CREATE TABLE IF NOT EXISTS identity_users (
			id            TEXT PRIMARY KEY,
			email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
			display_name  TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL,
			status        TEXT NOT NULL DEFAULT 'active',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			last_login_at INTEGER,
			failed_logins INTEGER NOT NULL DEFAULT 0,
			locked_until  INTEGER
		)`},
	{"0002_identity_users_status_idx",
		`CREATE INDEX IF NOT EXISTS identity_users_status ON identity_users(status)`},
	{"0003_identity_sessions", `
		CREATE TABLE IF NOT EXISTS identity_sessions (
			id          TEXT PRIMARY KEY,
			token_hash  TEXT NOT NULL UNIQUE,
			user_id     TEXT NOT NULL REFERENCES identity_users(id) ON DELETE CASCADE,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL,
			last_seen   INTEGER NOT NULL,
			ip          TEXT NOT NULL DEFAULT '',
			user_agent  TEXT NOT NULL DEFAULT '',
			revoked_at  INTEGER
		)`},
	{"0004_identity_sessions_user_idx",
		`CREATE INDEX IF NOT EXISTS identity_sessions_user ON identity_sessions(user_id)`},
	{"0005_audit_events", `
		CREATE TABLE IF NOT EXISTS audit_events (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			id          TEXT NOT NULL UNIQUE,
			at          INTEGER NOT NULL,
			actor_id    TEXT NOT NULL DEFAULT '',
			actor_email TEXT NOT NULL DEFAULT '',
			action      TEXT NOT NULL,
			subject     TEXT NOT NULL DEFAULT '',
			outcome     TEXT NOT NULL,
			ip          TEXT NOT NULL DEFAULT '',
			user_agent  TEXT NOT NULL DEFAULT '',
			detail      TEXT NOT NULL DEFAULT '',
			prev_hash   TEXT NOT NULL,
			hash        TEXT NOT NULL
		)`},
	{"0006_audit_events_at_idx",
		`CREATE INDEX IF NOT EXISTS audit_events_at ON audit_events(at DESC)`},
	{"0007_audit_events_actor_idx",
		`CREATE INDEX IF NOT EXISTS audit_events_actor ON audit_events(actor_id)`},
	{"0008_audit_events_action_idx",
		`CREATE INDEX IF NOT EXISTS audit_events_action ON audit_events(action)`},
	{"0009_scheduling_meetings", `
		CREATE TABLE IF NOT EXISTS scheduling_meetings (
			id              TEXT PRIMARY KEY,
			room            TEXT NOT NULL,
			title           TEXT NOT NULL,
			description     TEXT NOT NULL DEFAULT '',
			category        TEXT NOT NULL,
			status          TEXT NOT NULL,
			starts_at       INTEGER NOT NULL,
			ends_at         INTEGER NOT NULL,
			organizer_id    TEXT NOT NULL,
			organizer_email TEXT NOT NULL COLLATE NOCASE,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		)`},
	{"0010_scheduling_meetings_time_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_meetings_time
		 ON scheduling_meetings(starts_at, ends_at)`},
	{"0011_scheduling_meetings_org_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_meetings_org
		 ON scheduling_meetings(organizer_email)`},
	{"0012_scheduling_invitees", `
		CREATE TABLE IF NOT EXISTS scheduling_invitees (
			meeting_id TEXT NOT NULL
				REFERENCES scheduling_meetings(id) ON DELETE CASCADE,
			email      TEXT NOT NULL COLLATE NOCASE,
			user_id    TEXT NOT NULL DEFAULT '',
			response   TEXT NOT NULL DEFAULT 'pending',
			PRIMARY KEY (meeting_id, email)
		)`},
	{"0013_scheduling_invitees_email_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_invitees_email
		 ON scheduling_invitees(email)`},
	{"0014_messaging_conversations", `
		CREATE TABLE IF NOT EXISTS messaging_conversations (
			id              TEXT PRIMARY KEY,
			kind            TEXT NOT NULL,
			title           TEXT NOT NULL DEFAULT '',
			created_by      TEXT NOT NULL,
			created_at      INTEGER NOT NULL,
			last_message_at INTEGER NOT NULL
		)`},
	{"0015_messaging_members", `
		CREATE TABLE IF NOT EXISTS messaging_members (
			conversation_id TEXT NOT NULL
				REFERENCES messaging_conversations(id) ON DELETE CASCADE,
			user_id   TEXT NOT NULL,
			email     TEXT NOT NULL COLLATE NOCASE,
			name      TEXT NOT NULL DEFAULT '',
			joined_at INTEGER NOT NULL,
			last_read INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (conversation_id, user_id)
		)`},
	{"0016_messaging_members_user_idx",
		`CREATE INDEX IF NOT EXISTS messaging_members_user
		 ON messaging_members(user_id)`},
	{"0017_messaging_messages", `
		CREATE TABLE IF NOT EXISTS messaging_messages (
			id              TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL
				REFERENCES messaging_conversations(id) ON DELETE CASCADE,
			sender_id   TEXT NOT NULL,
			sender_name TEXT NOT NULL DEFAULT '',
			body        TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			edited_at   INTEGER,
			deleted     INTEGER NOT NULL DEFAULT 0
		)`},
	{"0018_messaging_messages_conv_idx",
		`CREATE INDEX IF NOT EXISTS messaging_messages_conv
		 ON messaging_messages(conversation_id, created_at DESC)`},
	{"0019_messaging_conversations_activity_idx",
		`CREATE INDEX IF NOT EXISTS messaging_conversations_activity
		 ON messaging_conversations(last_message_at DESC)`},
	{"0020_control_sessions", `
		CREATE TABLE IF NOT EXISTS control_sessions (
			id               TEXT PRIMARY KEY,
			meeting_id       TEXT NOT NULL DEFAULT '',
			controller_id    TEXT NOT NULL,
			controller_email TEXT NOT NULL DEFAULT '',
			controller_name  TEXT NOT NULL DEFAULT '',
			subject_id       TEXT NOT NULL,
			subject_email    TEXT NOT NULL DEFAULT '',
			subject_name     TEXT NOT NULL DEFAULT '',
			scope            TEXT NOT NULL,
			state            TEXT NOT NULL,
			requested_at     INTEGER NOT NULL,
			decided_at       INTEGER,
			expires_at       INTEGER,
			ended_at         INTEGER,
			ended_by         TEXT NOT NULL DEFAULT '',
			reason           TEXT NOT NULL DEFAULT ''
		)`},
	{"0021_control_sessions_subject_idx",
		`CREATE INDEX IF NOT EXISTS control_sessions_subject
		 ON control_sessions(subject_id, state, expires_at)`},
	{"0022_control_sessions_meeting_idx",
		`CREATE INDEX IF NOT EXISTS control_sessions_meeting
		 ON control_sessions(meeting_id, requested_at DESC)`},
	// Who has to wait, decided when the meeting is scheduled rather than when
	// it starts. Defaulting existing rows to 'trusted' rather than 'open' is
	// deliberate: a migration that silently opens every meeting already in the
	// database would be a security regression delivered by an upgrade.
	{"0023_scheduling_meetings_admission",
		`ALTER TABLE scheduling_meetings
		 ADD COLUMN admission TEXT NOT NULL DEFAULT 'trusted'`},
	// Deployment-wide policy. Absent rows are not an error: every setting has
	// a documented default, so an empty table is a working deployment with
	// the safe answer to every question.
	{"0024_org_settings", `
		CREATE TABLE IF NOT EXISTS org_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			updated_by TEXT NOT NULL DEFAULT ''
		)`},
	// One row per account per visit to a room: what actually happened, which
	// the calendar of what was scheduled cannot tell you. Feeds "recent" on
	// the dashboard. left_at is NULL while the person is still in the room.
	{"0025_scheduling_attendance", `
		CREATE TABLE IF NOT EXISTS scheduling_attendance (
			id        TEXT PRIMARY KEY,
			user_id   TEXT NOT NULL,
			room      TEXT NOT NULL,
			name      TEXT NOT NULL DEFAULT '',
			joined_at INTEGER NOT NULL,
			left_at   INTEGER
		)`},
	{"0026_scheduling_attendance_user_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_attendance_user
		 ON scheduling_attendance(user_id, joined_at DESC)`},
	// A call log, one row per ring. Shown in the conversation thread between
	// the two people, never in the dashboard's meeting history.
	{"0027_messaging_calls", `
		CREATE TABLE IF NOT EXISTS messaging_calls (
			id         TEXT PRIMARY KEY,
			room       TEXT NOT NULL,
			caller_id  TEXT NOT NULL,
			callee_id  TEXT NOT NULL,
			kind       TEXT NOT NULL DEFAULT 'video',
			started_at INTEGER NOT NULL,
			answered   INTEGER NOT NULL DEFAULT 0
		)`},
	{"0028_messaging_calls_pair_idx",
		`CREATE INDEX IF NOT EXISTS messaging_calls_pair
		 ON messaging_calls(caller_id, callee_id, started_at DESC)`},

	// Reactions.
	//
	// One row per person per emoji per message, with the pair as the primary
	// key: reacting twice with the same emoji is the same fact, and the
	// database says so rather than the code remembering to check. Teams allows
	// several different emoji from one person on one message, so the key
	// includes the emoji rather than stopping at the person.
	{"0029_messaging_reactions", `
		CREATE TABLE IF NOT EXISTS messaging_reactions (
			message_id TEXT NOT NULL
				REFERENCES messaging_messages(id) ON DELETE CASCADE,
			user_id    TEXT NOT NULL,
			emoji      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (message_id, user_id, emoji)
		)`},
	{"0030_messaging_reactions_msg_idx",
		`CREATE INDEX IF NOT EXISTS messaging_reactions_msg
		 ON messaging_reactions(message_id)`},

	// A quoted reply names the message it answers. Stored as an id rather than
	// a copy of the text: quoting a message that was later edited should show
	// what it says now, not what it said then, which is the behaviour every
	// chat product converged on because the alternative preserves mistakes.
	{"0031_messaging_messages_reply_to",
		`ALTER TABLE messaging_messages ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''`},

	// Saved messages, per person. Somebody else saving a message is not a fact
	// about the message, so it is keyed on the pair and visible only to them.
	{"0032_messaging_saved", `
		CREATE TABLE IF NOT EXISTS messaging_saved (
			user_id    TEXT NOT NULL,
			message_id TEXT NOT NULL
				REFERENCES messaging_messages(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, message_id)
		)`},

	// "Delete for me". One reader's view, not a fact about the message, so it
	// is the same shape as saving one: a (user, message) pair, applied when a
	// page of history is read. "Delete for everyone" stays on the row
	// (messaging_messages.deleted).
	{"0033_messaging_hidden", `
		CREATE TABLE IF NOT EXISTS messaging_hidden (
			user_id    TEXT NOT NULL,
			message_id TEXT NOT NULL
				REFERENCES messaging_messages(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, message_id)
		)`},

	// A forwarded message is a new message that says it was carried here
	// from somewhere else. The flag is all that is kept: the source is not
	// linked, because a link would let the recipient follow it into a
	// conversation they are not in.
	{"0034_messaging_messages_forwarded",
		`ALTER TABLE messaging_messages ADD COLUMN forwarded INTEGER NOT NULL DEFAULT 0`},

	// Files in chats. A row per file per message; the bytes live under a
	// server-generated blob key. Two messages may share one blob (a forward),
	// so the blob is a column, not the primary key. conversation_id is
	// denormalised so the shared-files view and the download check need no
	// join through the message.
	{"0035_messaging_attachments", `
		CREATE TABLE IF NOT EXISTS messaging_attachments (
			id              TEXT PRIMARY KEY,
			message_id      TEXT NOT NULL
				REFERENCES messaging_messages(id) ON DELETE CASCADE,
			conversation_id TEXT NOT NULL,
			uploader_id     TEXT NOT NULL,
			name            TEXT NOT NULL,
			mime            TEXT NOT NULL,
			size            INTEGER NOT NULL,
			blob            TEXT NOT NULL,
			created_at      INTEGER NOT NULL
		)`},
	{"0035_messaging_attachments_idx", `
		CREATE INDEX IF NOT EXISTS messaging_attachments_conv
			ON messaging_attachments(conversation_id, created_at DESC)`},
	{"0035_messaging_attachments_msg_idx", `
		CREATE INDEX IF NOT EXISTS messaging_attachments_msg
			ON messaging_attachments(message_id)`},

	// The people directory at fifty thousand accounts: every keystroke was a
	// LIKE '%q%' over the whole table (28 ms at that size). Three things make
	// it an index lookup instead. A trigram FTS5 table over name and address
	// answers any substring of three or more characters case-insensitively;
	// a generated lower-cased name key and the NOCASE address index answer
	// one- and two-character prefixes as range scans; a generated domain
	// column with an index replaces the substr(instr()) expression the org
	// filter used to compute per row. Triggers keep the FTS table in step with
	// every insert, rename and delete, so nothing else has to remember to.
	{"0036_identity_users_name_key",
		`ALTER TABLE identity_users ADD COLUMN name_key TEXT
			GENERATED ALWAYS AS (lower(display_name)) VIRTUAL`},
	{"0036_identity_users_email_domain",
		`ALTER TABLE identity_users ADD COLUMN email_domain TEXT
			GENERATED ALWAYS AS (lower(substr(email, instr(email, '@') + 1))) VIRTUAL`},
	{"0036_identity_users_email_key",
		`ALTER TABLE identity_users ADD COLUMN email_key TEXT
			GENERATED ALWAYS AS (lower(email)) VIRTUAL`},
	{"0036_identity_users_name_key_idx",
		`CREATE INDEX IF NOT EXISTS identity_users_name_key ON identity_users(name_key)`},
	{"0036_identity_users_email_key_idx",
		`CREATE INDEX IF NOT EXISTS identity_users_email_key ON identity_users(email_key)`},
	{"0036_identity_users_domain_idx",
		`CREATE INDEX IF NOT EXISTS identity_users_domain ON identity_users(status, email_domain)`},
	{"0036_identity_users_fts",
		`CREATE VIRTUAL TABLE IF NOT EXISTS identity_users_fts
			USING fts5(id UNINDEXED, display_name, email, tokenize='trigram')`},
	{"0036_identity_users_fts_backfill",
		`INSERT INTO identity_users_fts(id, display_name, email)
			SELECT id, display_name, email FROM identity_users`},
	{"0036_identity_users_fts_ai", `
		CREATE TRIGGER IF NOT EXISTS identity_users_fts_ai AFTER INSERT ON identity_users BEGIN
			INSERT INTO identity_users_fts(id, display_name, email)
				VALUES (new.id, new.display_name, new.email);
		END`},
	{"0036_identity_users_fts_ad", `
		CREATE TRIGGER IF NOT EXISTS identity_users_fts_ad AFTER DELETE ON identity_users BEGIN
			DELETE FROM identity_users_fts WHERE id = old.id;
		END`},
	{"0036_identity_users_fts_au", `
		CREATE TRIGGER IF NOT EXISTS identity_users_fts_au
			AFTER UPDATE OF display_name, email ON identity_users BEGIN
			DELETE FROM identity_users_fts WHERE id = old.id;
			INSERT INTO identity_users_fts(id, display_name, email)
				VALUES (new.id, new.display_name, new.email);
		END`},

	// Unread as a maintained counter, not a count. The conversation list was
	// counting unread messages per membership on every open (10-14 ms for a
	// person in 200 threads); the counter is bumped on send, recomputed
	// exactly on mark-read, and adjusted on delete-for-everyone and
	// delete-for-me, so it always equals the count it replaces
	// (unread_test.go holds it to that, op by op, under a random schedule).
	{"0037_messaging_members_unread",
		`ALTER TABLE messaging_members ADD COLUMN unread INTEGER NOT NULL DEFAULT 0`},
	{"0037_messaging_members_role",
		`ALTER TABLE messaging_members ADD COLUMN role TEXT NOT NULL DEFAULT 'member'`},
	{"0037_messaging_members_pair_idx",
		`CREATE INDEX IF NOT EXISTS messaging_members_pair ON messaging_members(conversation_id, user_id)`},
	{"0037_messaging_members_unread_backfill", `
		UPDATE messaging_members SET unread = (
			SELECT COUNT(*) FROM messaging_messages m
			 WHERE m.conversation_id = messaging_members.conversation_id
			   AND m.created_at > messaging_members.last_read
			   AND m.sender_id != messaging_members.user_id
			   AND m.deleted = 0
			   AND NOT EXISTS (SELECT 1 FROM messaging_hidden h
			                    WHERE h.user_id = messaging_members.user_id AND h.message_id = m.id))`},
	{"0037_messaging_members_owner_backfill", `
		UPDATE messaging_members SET role = 'owner'
		 WHERE user_id = (SELECT created_by FROM messaging_conversations c WHERE c.id = conversation_id)
		   AND (SELECT kind FROM messaging_conversations c WHERE c.id = conversation_id) != 'direct'`},

	// Channels: a topic, a visibility, a maintained member count (a channel
	// of two thousand does not ship two thousand members with every list),
	// and a lower-cased name key so the browse view is an index lookup and
	// two public channels cannot share a name.
	{"0037_messaging_conversations_topic",
		`ALTER TABLE messaging_conversations ADD COLUMN topic TEXT NOT NULL DEFAULT ''`},
	{"0037_messaging_conversations_visibility",
		`ALTER TABLE messaging_conversations ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private'`},
	{"0037_messaging_conversations_member_count",
		`ALTER TABLE messaging_conversations ADD COLUMN member_count INTEGER NOT NULL DEFAULT 0`},
	{"0037_messaging_conversations_member_count_backfill", `
		UPDATE messaging_conversations SET member_count =
			(SELECT COUNT(*) FROM messaging_members m WHERE m.conversation_id = messaging_conversations.id)`},
	{"0037_messaging_conversations_title_key",
		`ALTER TABLE messaging_conversations ADD COLUMN title_key TEXT
			GENERATED ALWAYS AS (lower(title)) VIRTUAL`},
	{"0037_messaging_channels_name_unique",
		`CREATE UNIQUE INDEX IF NOT EXISTS messaging_channels_name
			ON messaging_conversations(title_key) WHERE kind = 'channel'`},
	{"0037_messaging_channels_browse_idx",
		`CREATE INDEX IF NOT EXISTS messaging_channels_browse
			ON messaging_conversations(kind, visibility, title_key)`},

	// Membership events live in the thread, flagged.
	{"0037_messaging_messages_system",
		`ALTER TABLE messaging_messages ADD COLUMN system INTEGER NOT NULL DEFAULT 0`},

	// What one person has decided about one conversation: pinned to the top,
	// quiet, or out of the list until somebody writes. Facts about the
	// membership, not the conversation, so they live on the membership row.
	{"0038_messaging_members_favourite",
		`ALTER TABLE messaging_members ADD COLUMN favourite INTEGER NOT NULL DEFAULT 0`},
	{"0038_messaging_members_muted",
		`ALTER TABLE messaging_members ADD COLUMN muted INTEGER NOT NULL DEFAULT 0`},
	{"0038_messaging_members_hidden",
		`ALTER TABLE messaging_members ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`},

	// Groups, the rest of the way (groups.go): a picture, one pinned message,
	// mentions (who a message names, and a per-member counter of the ones
	// they have not read, kept in step with the unread counter).
	{"0039_messaging_conversations_avatar",
		`ALTER TABLE messaging_conversations ADD COLUMN avatar_blob TEXT NOT NULL DEFAULT ''`},
	{"0039_messaging_conversations_avatar_mime",
		`ALTER TABLE messaging_conversations ADD COLUMN avatar_mime TEXT NOT NULL DEFAULT ''`},
	{"0039_messaging_conversations_pinned",
		`ALTER TABLE messaging_conversations ADD COLUMN pinned_message_id TEXT NOT NULL DEFAULT ''`},
	{"0039_messaging_members_mentions",
		`ALTER TABLE messaging_members ADD COLUMN mentions INTEGER NOT NULL DEFAULT 0`},
	{"0039_messaging_mentions", `
		CREATE TABLE IF NOT EXISTS messaging_mentions (
			message_id TEXT NOT NULL
				REFERENCES messaging_messages(id) ON DELETE CASCADE,
			user_id    TEXT NOT NULL,
			PRIMARY KEY (message_id, user_id)
		)`},
	{"0039_messaging_mentions_user_idx",
		`CREATE INDEX IF NOT EXISTS messaging_mentions_user ON messaging_mentions(user_id)`},

	// The meeting chat (meetingchat.go): a room of three or more gets a
	// conversation, one per room name, found by the room and kept after.
	// The unique partial index is also the race arbiter when two joins try to
	// create it at once.
	{"0040_messaging_conversations_meeting_room",
		`ALTER TABLE messaging_conversations ADD COLUMN meeting_room TEXT NOT NULL DEFAULT ''`},
	{"0040_messaging_conversations_meeting_room_idx",
		`CREATE UNIQUE INDEX IF NOT EXISTS messaging_conversations_meeting
			ON messaging_conversations(meeting_room) WHERE meeting_room != ''`},

	// Composition (compose.go): a priority label, a delivery time for
	// scheduled messages (NULL for ordinary ones; in the future while
	// pending), and a small opaque JSON payload for cards.
	{"0041_messaging_messages_priority",
		`ALTER TABLE messaging_messages ADD COLUMN priority TEXT NOT NULL DEFAULT ''`},
	{"0041_messaging_messages_deliver_at",
		`ALTER TABLE messaging_messages ADD COLUMN deliver_at INTEGER`},
	{"0041_messaging_messages_meta",
		`ALTER TABLE messaging_messages ADD COLUMN meta TEXT NOT NULL DEFAULT ''`},
	{"0041_messaging_messages_due_idx",
		`CREATE INDEX IF NOT EXISTS messaging_messages_due
			ON messaging_messages(deliver_at) WHERE deliver_at IS NOT NULL`},

	// Search over message text: trigram FTS5, kept in step by triggers.
	// Membership events are not indexed; nobody searches for "joined".
	{"0041_messaging_messages_fts",
		`CREATE VIRTUAL TABLE IF NOT EXISTS messaging_messages_fts
			USING fts5(id UNINDEXED, body, tokenize='trigram')`},
	{"0041_messaging_messages_fts_backfill",
		`INSERT INTO messaging_messages_fts(id, body)
			SELECT id, body FROM messaging_messages WHERE system = 0 AND deleted = 0 AND body != ''`},
	{"0041_messaging_messages_fts_ai", `
		CREATE TRIGGER IF NOT EXISTS messaging_messages_fts_ai AFTER INSERT ON messaging_messages
		WHEN new.system = 0 AND new.body != '' BEGIN
			INSERT INTO messaging_messages_fts(id, body) VALUES (new.id, new.body);
		END`},
	{"0041_messaging_messages_fts_au", `
		CREATE TRIGGER IF NOT EXISTS messaging_messages_fts_au AFTER UPDATE OF body, deleted ON messaging_messages BEGIN
			DELETE FROM messaging_messages_fts WHERE id = old.id;
			INSERT INTO messaging_messages_fts(id, body)
				SELECT new.id, new.body WHERE new.system = 0 AND new.deleted = 0 AND new.body != '';
		END`},
	{"0041_messaging_messages_fts_ad", `
		CREATE TRIGGER IF NOT EXISTS messaging_messages_fts_ad AFTER DELETE ON messaging_messages BEGIN
			DELETE FROM messaging_messages_fts WHERE id = old.id;
		END`},

	// People search what they read, not what they typed: "ship tod" must find
	// "ship **today**". So the index holds the plain text with the marks
	// stripped, which SQL cannot compute -- the repository maintains it in
	// Go (indexMessage), and the triggers go. The table is emptied here and
	// refilled once at start-up (Service.ReindexIfNeeded).
	{"0042_messaging_messages_fts_drop_triggers_ai", `DROP TRIGGER IF EXISTS messaging_messages_fts_ai`},
	{"0042_messaging_messages_fts_drop_triggers_au", `DROP TRIGGER IF EXISTS messaging_messages_fts_au`},
	{"0042_messaging_messages_fts_drop_triggers_ad", `DROP TRIGGER IF EXISTS messaging_messages_fts_ad`},
	{"0042_messaging_messages_fts_clear", `DELETE FROM messaging_messages_fts`},

	// Who has blocked whom. One row per decision; the primary key is the
	// lookup ("has A blocked B") and its prefix is the count per person.
	{"0043_messaging_blocks", `
		CREATE TABLE IF NOT EXISTS messaging_blocks (
			user_id    TEXT    NOT NULL,
			blocked_id TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, blocked_id)
		) WITHOUT ROWID`},
	{"0043_messaging_blocks_reverse", `
		CREATE INDEX IF NOT EXISTS messaging_blocks_blocked
			ON messaging_blocks(blocked_id, user_id)`},

	// What a person says about their own availability, beside the socket's
	// online/offline: available, busy, do-not-disturb, away, with a note and
	// an optional expiry. One row per person; the primary key is the lookup.
	{"0044_messaging_status", `
		CREATE TABLE IF NOT EXISTS messaging_status (
			user_id    TEXT    PRIMARY KEY,
			status     TEXT    NOT NULL,
			note       TEXT    NOT NULL DEFAULT '',
			until      INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		) WITHOUT ROWID`},
	// Poll votes: one row per (message, voter, option). The primary key is
	// the tally query's index; the per-voter prefix is "what did I vote".
	{"0044_messaging_poll_votes", `
		CREATE TABLE IF NOT EXISTS messaging_poll_votes (
			message_id TEXT    NOT NULL,
			user_id    TEXT    NOT NULL,
			option     INTEGER NOT NULL,
			voted_at   INTEGER NOT NULL,
			PRIMARY KEY (message_id, user_id, option)
		) WITHOUT ROWID`},

	// What a message said before each edit. One row per edit, oldest first
	// by edited_at; the current text stays on the message.
	{"0045_messaging_message_edits", `
		CREATE TABLE IF NOT EXISTS messaging_message_edits (
			message_id TEXT    NOT NULL,
			edited_at  INTEGER NOT NULL,
			body       TEXT    NOT NULL,
			PRIMARY KEY (message_id, edited_at)
		) WITHOUT ROWID`},
	// Channel threads: replies hang off a root post. The reply-count query
	// and the "replies of this root" page both walk this index.
	{"0045_messaging_messages_reply_to", `
		CREATE INDEX IF NOT EXISTS messaging_messages_reply_to
			ON messaging_messages(reply_to, created_at) WHERE reply_to != ''`},
	// The channel feed is roots only: an index that serves it directly.
	{"0045_messaging_messages_roots", `
		CREATE INDEX IF NOT EXISTS messaging_messages_roots
			ON messaging_messages(conversation_id, created_at) WHERE reply_to = ''`},
	// When a person was last active: written when their last socket closes
	// and when a device goes idle. One row per account, read by primary key.
	{"0046_messaging_presence", `
		CREATE TABLE IF NOT EXISTS messaging_presence (
			user_id   TEXT    PRIMARY KEY,
			last_seen INTEGER NOT NULL
		) WITHOUT ROWID`},
	// The attendance list of one meeting: every visit to a room since the
	// meeting began, read by room, never by scanning (CLAUDE.md rule 7).
	{"0047_scheduling_attendance_room_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_attendance_room
		 ON scheduling_attendance(room, joined_at)`},
	// "Choose co-organizers": invitees who run the meeting with the organizer.
	{"0048_scheduling_invitees_co_organizer",
		`ALTER TABLE scheduling_invitees ADD COLUMN co_organizer INTEGER NOT NULL DEFAULT 0`},
	// Q&A in a meeting (scheduling/qna.go), by room. Every read is by room or
	// by question, each with its index (rule 7).
	{"0049_meeting_qna_settings", `
		CREATE TABLE IF NOT EXISTS meeting_qna_settings (
			room       TEXT PRIMARY KEY,
			moderation INTEGER NOT NULL DEFAULT 0,
			anonymous  INTEGER NOT NULL DEFAULT 1,
			closed     INTEGER NOT NULL DEFAULT 0
		) WITHOUT ROWID`},
	{"0050_meeting_qna_questions", `
		CREATE TABLE IF NOT EXISTS meeting_qna_questions (
			id          TEXT PRIMARY KEY,
			room        TEXT NOT NULL,
			author_id   TEXT NOT NULL,
			author_name TEXT NOT NULL DEFAULT '',
			anonymous   INTEGER NOT NULL DEFAULT 0,
			body        TEXT NOT NULL,
			created_at  INTEGER NOT NULL,
			status      TEXT NOT NULL,
			answered    INTEGER NOT NULL DEFAULT 0,
			pinned      INTEGER NOT NULL DEFAULT 0
		)`},
	{"0051_meeting_qna_questions_room_idx",
		`CREATE INDEX IF NOT EXISTS meeting_qna_questions_room ON meeting_qna_questions(room, created_at)`},
	{"0052_meeting_qna_replies", `
		CREATE TABLE IF NOT EXISTS meeting_qna_replies (
			id          TEXT PRIMARY KEY,
			question_id TEXT NOT NULL REFERENCES meeting_qna_questions(id) ON DELETE CASCADE,
			author_id   TEXT NOT NULL,
			author_name TEXT NOT NULL DEFAULT '',
			anonymous   INTEGER NOT NULL DEFAULT 0,
			body        TEXT NOT NULL,
			created_at  INTEGER NOT NULL
		)`},
	{"0053_meeting_qna_replies_question_idx",
		`CREATE INDEX IF NOT EXISTS meeting_qna_replies_question ON meeting_qna_replies(question_id, created_at)`},
	{"0054_meeting_qna_likes", `
		CREATE TABLE IF NOT EXISTS meeting_qna_likes (
			question_id TEXT NOT NULL REFERENCES meeting_qna_questions(id) ON DELETE CASCADE,
			user_id     TEXT NOT NULL,
			PRIMARY KEY (question_id, user_id)
		) WITHOUT ROWID`},
	// Meeting notes (scheduling/notes.go): created by the organizer, items in
	// three sections, read by room in creation order.
	{"0055_meeting_notes", `
		CREATE TABLE IF NOT EXISTS meeting_notes (
			room       TEXT PRIMARY KEY,
			created_by TEXT NOT NULL,
			created_at INTEGER NOT NULL
		) WITHOUT ROWID`},
	{"0056_meeting_note_items", `
		CREATE TABLE IF NOT EXISTS meeting_note_items (
			id         TEXT PRIMARY KEY,
			room       TEXT NOT NULL,
			section    TEXT NOT NULL,
			body       TEXT NOT NULL,
			done       INTEGER NOT NULL DEFAULT 0,
			assignee   TEXT NOT NULL DEFAULT '',
			version    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`},
	{"0057_meeting_note_items_room_idx",
		`CREATE INDEX IF NOT EXISTS meeting_note_items_room ON meeting_note_items(room, created_at, id)`},
	// The meeting transcript (scheduling/transcript.go): final lines of
	// speech, numbered per session (the room's epoch), read in order.
	{"0058_meeting_transcript", `
		CREATE TABLE IF NOT EXISTS meeting_transcript (
			room    TEXT NOT NULL,
			session INTEGER NOT NULL,
			n       INTEGER NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			name    TEXT NOT NULL,
			body    TEXT NOT NULL,
			at      INTEGER NOT NULL,
			PRIMARY KEY (room, session, n)
		) WITHOUT ROWID`},
	// "Did this account attend this room": who may read its transcript.
	{"0059_scheduling_attendance_room_user_idx",
		`CREATE INDEX IF NOT EXISTS scheduling_attendance_room_user ON scheduling_attendance(room, user_id)`},
	// Teams (messaging/teams.go): containers above channels. A team owns its
	// channels; a channel with no team is an organisation-wide one, as before.
	{"0060_messaging_teams", `
		CREATE TABLE IF NOT EXISTS messaging_teams (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			name_key     TEXT GENERATED ALWAYS AS (lower(name)) VIRTUAL,
			description  TEXT NOT NULL DEFAULT '',
			visibility   TEXT NOT NULL,
			join_code    TEXT,
			created_by   TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			member_count INTEGER NOT NULL DEFAULT 0
		)`},
	{"0061_messaging_teams_browse_idx",
		`CREATE INDEX IF NOT EXISTS messaging_teams_browse ON messaging_teams(visibility, name_key)`},
	{"0062_messaging_teams_code_idx",
		`CREATE UNIQUE INDEX IF NOT EXISTS messaging_teams_code ON messaging_teams(join_code) WHERE join_code IS NOT NULL`},
	{"0063_messaging_team_members", `
		CREATE TABLE IF NOT EXISTS messaging_team_members (
			team_id   TEXT NOT NULL REFERENCES messaging_teams(id) ON DELETE CASCADE,
			user_id   TEXT NOT NULL,
			email     TEXT NOT NULL COLLATE NOCASE,
			name      TEXT NOT NULL DEFAULT '',
			role      TEXT NOT NULL,
			joined_at INTEGER NOT NULL,
			PRIMARY KEY (team_id, user_id)
		) WITHOUT ROWID`},
	{"0064_messaging_team_members_user_idx",
		`CREATE INDEX IF NOT EXISTS messaging_team_members_user ON messaging_team_members(user_id, team_id)`},
	{"0065_messaging_conversations_team",
		`ALTER TABLE messaging_conversations ADD COLUMN team_id TEXT NOT NULL DEFAULT ''`},
	// Channel names are unique within their team (the organisation-wide ones
	// share team '' and stay unique among themselves).
	{"0066_messaging_channels_name_per_team", `
		DROP INDEX IF EXISTS messaging_channels_name;
		CREATE UNIQUE INDEX IF NOT EXISTS messaging_channels_team_name
			ON messaging_conversations(team_id, title_key) WHERE kind = 'channel';
		CREATE INDEX IF NOT EXISTS messaging_conversations_team ON messaging_conversations(team_id, kind)`},
	// The activity feed (messaging/activity.go): one row per person per
	// event, naming the message or the team; both cascade. The feed walks
	// (user, time); the cascades and the reaction take-back walk the other two.
	{"0067_messaging_activity", `
		CREATE TABLE IF NOT EXISTS messaging_activity (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			kind       TEXT NOT NULL,
			actor_id   TEXT NOT NULL,
			actor_name TEXT NOT NULL DEFAULT '',
			message_id TEXT REFERENCES messaging_messages(id) ON DELETE CASCADE,
			team_id    TEXT REFERENCES messaging_teams(id) ON DELETE CASCADE,
			emoji      TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			read       INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS messaging_activity_feed ON messaging_activity(user_id, created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS messaging_activity_message ON messaging_activity(message_id) WHERE message_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS messaging_activity_team ON messaging_activity(team_id) WHERE team_id IS NOT NULL`},
	// Password reset links (identity/reset.go): the token's hash only, one
	// outstanding per account (a new request replaces it), gone with the
	// account. Looked up by hash; replaced and throttled by account.
	{"0068_identity_password_resets", `
		CREATE TABLE IF NOT EXISTS identity_password_resets (
			token_hash TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL UNIQUE REFERENCES identity_users(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`},
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	for _, m := range migrations {
		var seen int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, m.name).Scan(&seen)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.name, err)
		}
		if seen > 0 {
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(name, applied_at) VALUES (?, ?)`,
			m.name, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}
	return nil
}
