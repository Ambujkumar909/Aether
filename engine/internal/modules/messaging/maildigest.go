package messaging

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Missed-activity email: the push's slower twin.
//
// A person who has chosen a period (notifyprefs.go, Email) and has no
// socket open is sent one email, at most once per period, about what is
// still unread when the period ends. What counts is exactly what would have
// been pushed: a direct message, a mention, a reply to something they
// wrote, an urgent message; never a muted conversation, never a kind they
// have set quiet. The decision is made once, in notifyAbsent, beside the
// push's.
//
// What is stored is only that something waits: one row per person and
// conversation with the time their email may go. The first thing missed
// starts the clock; more of it changes nothing (INSERT OR IGNORE). The
// words are read from the messages when the email is written, so it says
// what is stored at that moment: read in the meantime and no email is
// sent; deleted and it is not quoted.
//
// Bounded everywhere: rows by a person's memberships, a tick by mailBatch
// people, an email by digestShown conversations, and the mail queue by its
// own length. With no mail server (Mail nil) nothing here runs and no row
// is ever written. Idle is one indexed read per tick that finds nothing.

const (
	// mailBatch is the most people one tick writes to.
	// ponytail: one sender, one connection per email; at 200 a minute a
	// deployment where far more than that are due each minute sends late
	// (rows stay due, nothing is lost). Parallel senders if it ever is.
	mailBatch = 200
	// digestRead is the most conversations one email looks at, digestShown
	// the most it lists; the rest are a count.
	digestRead  = 50
	digestShown = 10

	digestSubject = "Unread messages in Aether"
)

type mailDue struct {
	userID, convID string
	dueAt          int64 // ms
}

// DigestLine is one conversation in an email: who wrote last, where, how
// much is unread.
type DigestLine struct {
	Direct   bool
	Title    string
	Unread   int
	Mentions int
	Sender   string
	Words    string
}

// --- repository -------------------------------------------------------------

// AddMailDue notes that something waits. A row already there is left as it
// is: its clock is running.
func (r *SQLRepository) AddMailDue(ctx context.Context, rows []mailDue) error {
	const chunk = 300 // three parameters a row
	for len(rows) > 0 {
		n := min(len(rows), chunk)
		args := make([]any, 0, 3*n)
		for _, x := range rows[:n] {
			args = append(args, x.userID, x.convID, x.dueAt)
		}
		if _, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO messaging_mail_due(user_id, conversation_id, due_at) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?),", n), ","), args...); err != nil {
			return fmt.Errorf("mail due: %w", err)
		}
		rows = rows[n:]
	}
	return nil
}

// MailDueUsers lists people with a row whose time has come, longest
// overdue first. The order is also what makes this a walk of the due_at
// index: asked for distinct people in no order, the planner walked the
// whole table by its key instead (the plan test refused it).
func (r *SQLRepository) MailDueUsers(ctx context.Context, now time.Time, limit int) ([]string, error) {
	ids, err := returningIDs(ctx, r, `SELECT user_id FROM messaging_mail_due WHERE due_at <= ? ORDER BY due_at LIMIT ?`, now.UnixMilli(), limit)
	return appendNew(nil, ids), err
}

// TakeMailDue removes all of one person's rows, due or not, and returns
// their conversations: one email covers everything waiting, and the next
// thing missed starts a new period.
func (r *SQLRepository) TakeMailDue(ctx context.Context, userID string) ([]string, error) {
	return returningIDs(ctx, r, `DELETE FROM messaging_mail_due WHERE user_id = ? RETURNING conversation_id`, userID)
}

// MailDigest reads what is unread for one person in the named
// conversations, newest conversation first: the count the badge shows, and
// the newest unread message from somebody else. A conversation they have
// left, muted or read is absent: the join on that message is the unread
// test, by the same predicate the count is held to. Two messages in one
// millisecond are settled by rowid, the later insert, as everywhere else:
// without it the email quoted the earlier of the two, two runs in five.
func (r *SQLRepository) MailDigest(ctx context.Context, userID string, convIDs []string) ([]DigestLine, error) {
	if len(convIDs) == 0 {
		return nil, nil
	}
	holes, ids := placeholders(convIDs)
	args := append([]any{r.large, userID}, ids...)
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.kind, c.title,
		       CASE WHEN c.member_count > ? THEN (`+unreadCapped+`) ELSE messaging_members.unread END,
		       messaging_members.mentions, last.sender_name, substr(last.body, 1, 400)
		  FROM messaging_members
		  JOIN messaging_conversations c ON c.id = messaging_members.conversation_id
		  JOIN messaging_messages last ON last.id = (
		       SELECT msg.id FROM messaging_messages msg
		        WHERE msg.conversation_id = messaging_members.conversation_id
		          AND msg.created_at > messaging_members.last_read
		          AND msg.sender_id != messaging_members.user_id
		          AND msg.deleted = 0 AND msg.system = 0
		          AND (msg.deliver_at IS NULL OR msg.deliver_at <= msg.created_at)
		          AND NOT EXISTS (SELECT 1 FROM messaging_hidden h
		                           WHERE h.user_id = messaging_members.user_id AND h.message_id = msg.id)
		        ORDER BY msg.created_at DESC, msg.rowid DESC LIMIT 1)
		 WHERE messaging_members.user_id = ? AND messaging_members.muted = 0
		   AND messaging_members.conversation_id IN (`+holes+`)
		 ORDER BY c.last_message_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("mail digest: %w", err)
	}
	defer rows.Close()
	var out []DigestLine
	for rows.Next() {
		var l DigestLine
		var kind string
		if err := rows.Scan(&kind, &l.Title, &l.Unread, &l.Mentions, &l.Sender, &l.Words); err != nil {
			return nil, err
		}
		l.Direct = Kind(kind) == KindDirect
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- service ----------------------------------------------------------------

func (s *Service) mailUnit() time.Duration {
	if s.MailUnit > 0 {
		return s.MailUnit
	}
	return time.Minute
}

// RunMailDigest sends what has come due, once every unit (a minute), until
// ctx ends.
func (s *Service) RunMailDigest(ctx context.Context) {
	t := time.NewTicker(s.mailUnit())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sendDigests(ctx, time.Now().UTC())
		}
	}
}

// sendDigests writes to everyone whose period has ended, and says how many
// emails it handed over.
func (s *Service) sendDigests(ctx context.Context, now time.Time) int {
	users, err := s.repo.MailDueUsers(ctx, now, mailBatch)
	if err != nil || len(users) == 0 {
		return 0
	}
	prefs, err := s.repo.NotifyPrefsFor(ctx, users)
	if err != nil {
		return 0 // the rows stay due; the next tick asks again
	}
	sent := 0
	for _, id := range users {
		convs, err := s.repo.TakeMailDue(ctx, id)
		if err != nil || len(convs) == 0 {
			continue
		}
		p := prefs[id]
		if emailPeriods[p.Email] == 0 {
			continue // turned off since: what waited is not sent
		}
		email, _, err := s.dir.LookupByID(ctx, id)
		if err != nil {
			continue // the account is gone
		}
		lines, err := s.repo.MailDigest(ctx, id, convs[:min(len(convs), digestRead)])
		if err != nil || len(lines) == 0 {
			continue // read in the meantime: nothing to say
		}
		if !s.Mail(email, digestSubject, digestBody(s.MailLink, lines, p)) {
			// The queue is full. Put the rows back, due now, and stop: the
			// next tick starts with them.
			back := make([]mailDue, len(convs))
			for i, c := range convs {
				back[i] = mailDue{userID: id, convID: c, dueAt: now.UnixMilli()}
			}
			if err := s.repo.AddMailDue(ctx, back); err != nil {
				s.log.Warn("mail digest: could not put back", "err", err)
			}
			break
		}
		sent++
	}
	return sent
}

// oneLine makes text somebody else wrote safe to set inside this email: one
// line, however many it had (so it cannot imitate the lines around it), no
// control characters, no direction overrides (which can make a line read as
// something it is not), and no longer than max characters.
func oneLine(v string, max int) string {
	v = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	if rs := []rune(v); len(rs) > max {
		v = string(rs[:max]) + "…"
	}
	return v
}

var periodWords = map[string]string{"10m": "every 10 minutes", "hourly": "every hour", "8h": "every 8 hours", "daily": "once a day"}

// digestBody is the email. Pure. Names and titles are always given; the
// words of a message only when the person lets notices carry them.
func digestBody(link string, lines []DigestLine, p NotifyPrefs) string {
	var b strings.Builder
	b.WriteString("While you were away from Aether:\n")
	for i, l := range lines {
		if i == digestShown {
			fmt.Fprintf(&b, "\nand %d more.\n", len(lines)-digestShown)
			break
		}
		who := oneLine(l.Sender, 80)
		if !l.Direct {
			if title := oneLine(l.Title, 80); title != "" {
				who += " in " + title
			} else {
				who += " in a group chat"
			}
		}
		n := fmt.Sprint(l.Unread)
		if l.Unread >= UnreadCap {
			n = fmt.Sprintf("%d+", UnreadCap-1)
		}
		fmt.Fprintf(&b, "\n%s, %s unread", who, n)
		if l.Mentions > 0 {
			b.WriteString(", you were mentioned")
		}
		b.WriteString("\n")
		if words := oneLine(l.Words, 140); p.Preview && words != "" {
			fmt.Fprintf(&b, "    \"%s\"\n", words)
		}
	}
	fmt.Fprintf(&b, "\nOpen Aether: %s\n\n", link)
	fmt.Fprintf(&b, "You get this %s at most, and only while something is unread.\n", periodWords[p.Email])
	b.WriteString("To change that or turn it off: Account, then \"What you are told about\".\n")
	return b.String()
}
