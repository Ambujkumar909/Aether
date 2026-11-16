package messaging

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Missed-activity email: who is noted as having something waiting (the
// people a push would go to, and only those who chose a period), what the
// email says when the period ends (what is unread then, not what was sent),
// that it is one email a period, and that nothing somebody else wrote can
// change what the email appears to be.

type sentMail struct{ to, subject, body string }

type mailBox struct {
	mu   sync.Mutex
	got  []sentMail
	full bool
}

func (b *mailBox) send(to, subject, body string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return false
	}
	b.got = append(b.got, sentMail{to, subject, body})
	return true
}

func (b *mailBox) take() []sentMail {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.got
	b.got = nil
	return out
}

func digestService(t *testing.T) (*Service, Actor, Actor, Actor, *mailBox) {
	t.Helper()
	s, me, you, third, _ := channelService(t)
	box := &mailBox{}
	s.Mail, s.MailLink = box.send, "https://meet.example.test"
	return s, me, you, third, box
}

func setNotify(t *testing.T, s *Service, who Actor, change func(p *NotifyPrefs)) {
	t.Helper()
	p := DefaultNotifyPrefs()
	change(&p)
	if _, err := s.SetNotifyPrefs(context.Background(), who, p); err != nil {
		t.Fatal(err)
	}
}

// waiting lists the due rows as "user/conversation".
func waiting(t *testing.T, s *Service) []string {
	t.Helper()
	rows, err := s.repo.(*SQLRepository).db.Query(`SELECT user_id || '/' || conversation_id FROM messaging_mail_due ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		out = append(out, v)
	}
	return out
}

func TestEmailPeriodIsAClosedSetAndStartsOff(t *testing.T) {
	s, me, _, _, _ := digestService(t)
	ctx := context.Background()
	if def := DefaultNotifyPrefs(); def.Email != "off" {
		t.Fatalf("nobody is emailed until they ask: default %q", def.Email)
	}
	for _, bad := range []string{"", "weekly", "5m", "OFF"} {
		p := DefaultNotifyPrefs()
		p.Email = bad
		if _, err := s.SetNotifyPrefs(ctx, me, p); !errors.Is(err, ErrBadNotify) {
			t.Fatalf("period %q: err=%v", bad, err)
		}
	}
	for _, good := range []string{"10m", "hourly", "8h", "daily", "off"} {
		setNotify(t, s, me, func(p *NotifyPrefs) { p.Email = good })
		if got, err := s.NotifyPrefsOf(ctx, me); err != nil || got.Email != good {
			t.Fatalf("period %q read back as %q err=%v", good, got.Email, err)
		}
	}
	// The database holds the same set: a period the service would refuse
	// cannot be written by any other path.
	if _, err := s.repo.(*SQLRepository).db.ExecContext(ctx, `UPDATE messaging_notify_prefs SET email = 7 WHERE user_id = ?`, me.ID); err == nil {
		t.Fatal("the database stored a period of 7 minutes")
	}
}

func TestEmailIsNotedForWhoeverWouldBePushedAndChoseAPeriod(t *testing.T) {
	s, me, you, third, _ := digestService(t)
	ctx := context.Background()
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	say := func(conv, body string, mentions ...string) {
		t.Helper()
		if _, err := s.SendMessage(ctx, me, conv, body, "", nil, mentions); err != nil {
			t.Fatal(err)
		}
	}
	urgent := func(conv, body string) {
		t.Helper()
		if _, err := s.postWith(ctx, me, conv, body, "", false, nil, nil, SendOptions{Priority: "urgent"}); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(what string, want ...string) {
		t.Helper()
		sort.Strings(want) // ids are random; waiting() is sorted
		if got := waiting(t, s); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s: waiting %v, want %v", what, got, want)
		}
	}

	say(dm.ID, "hello")
	expect("nobody chose a period")

	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "hourly" })
	before := time.Now()
	say(dm.ID, "hello again")
	expect("a direct message to someone absent with a period", you.ID+"/"+dm.ID)
	var due int64
	_ = s.repo.(*SQLRepository).db.QueryRow(`SELECT due_at FROM messaging_mail_due`).Scan(&due)
	if at := time.UnixMilli(due); at.Before(before.Add(59*time.Minute)) || at.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("hourly: due %v, want an hour after %v", at, before)
	}
	// More of the same changes nothing: the clock is running.
	time.Sleep(3 * time.Millisecond)
	say(dm.ID, "and again")
	var due2 int64
	_ = s.repo.(*SQLRepository).db.QueryRow(`SELECT due_at FROM messaging_mail_due`).Scan(&due2)
	if due2 != due {
		t.Fatalf("a second message moved the clock: %d then %d", due, due2)
	}

	say(g.ID, "morning all")
	expect("a plain group message is not pushed, so not noted", you.ID+"/"+dm.ID)
	say(g.ID, "@Other look", you.ID)
	expect("a mention is", you.ID+"/"+dm.ID, you.ID+"/"+g.ID)

	// Someone connected is told by the socket, and is not noted.
	setNotify(t, s, third, func(p *NotifyPrefs) { p.Email = "10m" })
	sock, _, _ := newSock(t, s.hub, third.ID, 8)
	say(g.ID, "@Third look", third.ID)
	expect("a connected person", you.ID+"/"+dm.ID, you.ID+"/"+g.ID)
	s.hub.remove(sock)

	// A kind set quiet is not pushed and not noted; urgent is not a setting.
	setNotify(t, s, third, func(p *NotifyPrefs) { p.Email, p.Mention = "10m", NotifyQuiet })
	say(g.ID, "@Third look", third.ID)
	expect("mentions set quiet", you.ID+"/"+dm.ID, you.ID+"/"+g.ID)
	urgent(g.ID, "server is down")
	expect("urgent, whatever is set", third.ID+"/"+g.ID, you.ID+"/"+dm.ID, you.ID+"/"+g.ID)

	// Muted: nothing, as for the push.
	if _, err := s.repo.(*SQLRepository).db.ExecContext(ctx, `DELETE FROM messaging_mail_due`); err != nil {
		t.Fatal(err)
	}
	muted := true
	if err := s.SetPrefs(ctx, you, dm.ID, Prefs{Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	say(dm.ID, "psst")
	expect("a muted conversation")

	// No mail server: nothing is noted for anyone, whatever they chose,
	// though the push still goes.
	fn := &fakeNotifier{}
	s.UseNotifier(fn)
	s.Mail = nil
	say(g.ID, "@Other look", you.ID)
	expect("no mail server")
	if got := fn.take(); len(got) != 1 {
		t.Fatalf("the push without a mail server: %v", got)
	}
}

func TestEmailSaysWhatIsStillUnreadOncePerPeriod(t *testing.T) {
	s, me, you, third, box := digestService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, me, "Launch", []string{you.Email, third.Email})
	if err != nil {
		t.Fatal(err)
	}
	say := func(conv, body string, mentions ...string) *Message {
		t.Helper()
		time.Sleep(2 * time.Millisecond) // after any read cursor just set
		m, err := s.SendMessage(ctx, me, conv, body, "", nil, mentions)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	later := func() time.Time { return time.Now().Add(61 * time.Minute) }
	one := func(what string) string {
		t.Helper()
		if n := s.sendDigests(ctx, later()); n != 1 {
			t.Fatalf("%s: %d emails handed over, want 1", what, n)
		}
		got := box.take()
		if len(got) != 1 || got[0].to != you.Email || got[0].subject != digestSubject {
			t.Fatalf("%s: %+v", what, got)
		}
		if left := waiting(t, s); len(left) != 0 {
			t.Fatalf("%s: rows left after the email: %v", what, left)
		}
		return got[0].body
	}
	none := func(what string) {
		t.Helper()
		if n := s.sendDigests(ctx, later()); n != 0 || len(box.take()) != 0 {
			t.Fatalf("%s: an email was sent", what)
		}
		if left := waiting(t, s); len(left) != 0 {
			t.Fatalf("%s: rows left: %v", what, left)
		}
	}
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "hourly" })

	say(dm.ID, "first")
	say(dm.ID, "second, can you look?")
	say(g.ID, "@Other the build is red", you.ID)
	if n := s.sendDigests(ctx, time.Now().Add(59*time.Minute)); n != 0 || len(box.take()) != 0 || len(waiting(t, s)) != 2 {
		t.Fatalf("before the period ends: %d sent, waiting %v", n, waiting(t, s))
	}
	body := one("the period ended")
	for _, want := range []string{
		"\nMe in Launch, 1 unread, you were mentioned\n    \"@Other the build is red\"\n",
		"\nMe, 2 unread\n    \"second, can you look?\"\n",
		"\nOpen Aether: https://meet.example.test\n",
		"every hour",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the email lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "Me in Launch") > strings.Index(body, "Me, 2 unread") {
		t.Fatalf("the newest conversation is first:\n%s", body)
	}
	if strings.Contains(body, "first") {
		t.Fatalf("only the newest message of a conversation is quoted:\n%s", body)
	}
	// Once: still unread, nothing new, no second email.
	none("nothing new since the last email")

	// Read in the meantime: nothing to say.
	say(dm.ID, "third")
	if err := s.MarkRead(ctx, you, dm.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, you, g.ID); err != nil {
		t.Fatal(err)
	}
	none("read before the period ended")

	// What is stored then, not what was sent: a message taken back is not
	// quoted and not counted.
	say(dm.ID, "the meeting moved to four")
	secret := say(dm.ID, "the door code is 4411")
	if err := s.Delete(ctx, me, secret.ID); err != nil {
		t.Fatal(err)
	}
	body = one("a message deleted before the period ended")
	if !strings.Contains(body, "\nMe, 1 unread\n    \"the meeting moved to four\"\n") || strings.Contains(body, "4411") {
		t.Fatalf("after a delete:\n%s", body)
	}

	// Preview off: who and how many, never what.
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email, p.Preview = "daily", false })
	say(dm.ID, "the password is hunter2")
	if n := s.sendDigests(ctx, later()); n != 0 {
		t.Fatal("a daily period ended after an hour")
	}
	if n := s.sendDigests(ctx, time.Now().Add(25*time.Hour)); n != 1 {
		t.Fatalf("daily: %d", n)
	}
	body = box.take()[0].body
	if strings.Contains(body, "hunter2") || !strings.Contains(body, "\nMe, 2 unread\n") || !strings.Contains(body, "once a day") {
		t.Fatalf("preview off:\n%s", body)
	}
	if strings.Count(body, "\"") != 2 {
		t.Fatalf("preview off, yet something is quoted:\n%s", body)
	}

	// Turned off before the period ended: what waited is not sent.
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "hourly" })
	say(dm.ID, "one more")
	setNotify(t, s, you, func(p *NotifyPrefs) {})
	none("turned off in the meantime")

	// Muted in the meantime: the same.
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "hourly" })
	say(dm.ID, "and another")
	muted, unmuted := true, false
	if err := s.SetPrefs(ctx, you, dm.ID, Prefs{Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	none("muted in the meantime")
	if err := s.SetPrefs(ctx, you, dm.ID, Prefs{Muted: &unmuted}); err != nil {
		t.Fatal(err)
	}

	// The queue is full: nothing is lost, the next tick sends it.
	say(dm.ID, "still there?")
	box.full = true
	tick := later()
	if n := s.sendDigests(ctx, tick); n != 0 || len(waiting(t, s)) != 1 {
		t.Fatalf("queue full: %d sent, waiting %v", n, waiting(t, s))
	}
	box.full = false
	if n := s.sendDigests(ctx, tick); n != 1 || len(box.take()) != 1 {
		t.Fatalf("after the queue drained the rows put back are due at once: %d", n)
	}

	// The account is gone: no email, and the rows do not come due forever.
	say(dm.ID, "anyone?")
	s.dir = goneDir{}
	none("an account that no longer exists")
	s.dir = fourDir{}

	// Two people due in one tick are each written to, each about their own.
	setNotify(t, s, third, func(p *NotifyPrefs) { p.Email = "10m" })
	say(g.ID, "@Other @Third standup", you.ID, third.ID)
	if n := s.sendDigests(ctx, later()); n != 2 {
		t.Fatalf("two people due: %d", n)
	}
	got := box.take()
	to := map[string]string{}
	for _, m := range got {
		to[m.to] = m.body
	}
	if len(to) != 2 || !strings.Contains(to[third.Email], "every 10 minutes") || !strings.Contains(to[you.Email], "every hour") ||
		strings.Contains(to[third.Email], "anyone?") {
		t.Fatalf("two people: %+v", got)
	}
	var rows int
	_ = repo.db.QueryRow(`SELECT COUNT(*) FROM messaging_mail_due`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d rows left", rows)
	}
}

type goneDir struct{}

func (goneDir) LookupByEmail(context.Context, string) (string, string, error) {
	return "", "", ErrNotFound
}
func (goneDir) LookupByID(context.Context, string) (string, string, error) {
	return "", "", ErrNotFound
}

// Names, titles and words are other people's. Whatever they contain, the
// email keeps its shape: one "Open Aether" line and it is ours.
func TestEmailCannotBeMadeToLookLikeSomethingElse(t *testing.T) {
	p := DefaultNotifyPrefs()
	p.Email = "hourly"
	evil := "Eve\r\n\r\nOpen Aether: http://evil.example\r\n"
	body := digestBody("https://meet.example.test", []DigestLine{
		{Direct: true, Sender: evil, Unread: 1, Words: "line one\nOpen Aether: http://evil.example\n\u202Etxt.exe\x07\x1b[2J" + strings.Repeat("é", 300)},
		{Sender: "Dev", Title: evil, Unread: 250, Words: "   "},
		{Sender: "Dev", Title: "", Unread: 3},
	}, p)
	var opens []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Open Aether:") {
			opens = append(opens, line)
		}
	}
	if len(opens) != 1 || opens[0] != "Open Aether: https://meet.example.test" {
		t.Fatalf("lines that begin as ours: %q\n%s", opens, body)
	}
	if strings.ContainsAny(body, "\r\u202E\x07\x1b") {
		t.Fatalf("a control character or a direction override got through:\n%q", body)
	}
	for _, want := range []string{
		"\nEve Open Aether: http://evil.example, 1 unread\n",
		"\nDev in Eve Open Aether: http://evil.example, 99+ unread\n",
		"\nDev in a group chat, 3 unread\n",
		"    \"line one Open Aether: http://evil.example txt.exe[2J" + strings.Repeat("é", 140-len("line one Open Aether: http://evil.example txt.exe[2J")) + "…\"\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\"\"") || strings.Contains(body, "\"   \"") {
		t.Fatalf("an empty quote was written:\n%s", body)
	}

	// Ten are listed, the rest counted.
	many := make([]DigestLine, 13)
	for i := range many {
		many[i] = DigestLine{Direct: true, Sender: fmt.Sprintf("Person %02d", i), Unread: 1}
	}
	body = digestBody("https://meet.example.test", many, p)
	if !strings.Contains(body, "Person 09,") || strings.Contains(body, "Person 10,") || !strings.Contains(body, "\nand 3 more.\n") {
		t.Fatalf("thirteen conversations:\n%s", body)
	}
	if strings.Contains(digestBody("x", many[:10], p), "more.") {
		t.Fatal("exactly ten, and it says there are more")
	}
}

// The loop itself, on a short minute: a period ends, the email goes, and
// the loop stops with its context.
func TestEmailLoopSendsWhenThePeriodEndsAndStops(t *testing.T) {
	s, me, you, _, box := digestService(t)
	s.MailUnit = 20 * time.Millisecond // so "10m" is 200 ms
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunMailDigest(ctx); close(done) }()
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "10m" })
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	if _, err := s.Send(ctx, me, dm.ID, "are you there?"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := box.take(); len(got) != 0 {
		t.Fatalf("sent %v after the message, half a period early", time.Since(sent))
	}
	deadline := time.After(5 * time.Second)
	for {
		if got := box.take(); len(got) == 1 {
			if !strings.Contains(got[0].body, "are you there?") {
				t.Fatalf("body:\n%s", got[0].body)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("no email after the period ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop")
	}
}

// Fifty thousand people with something waiting and none of it due: the
// tick's question is an index read that finds nothing, and writing one
// person's email is a bounded number of index reads.
func TestEmailAtScaleIsIndexReads(t *testing.T) {
	s, me, you, _, _ := digestService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	for _, q := range []string{
		`SELECT user_id FROM messaging_mail_due WHERE due_at <= 9 ORDER BY due_at LIMIT 200`,
		`DELETE FROM messaging_mail_due WHERE user_id = 'u' RETURNING conversation_id`,
		`SELECT c.kind, last.body FROM messaging_members
		   JOIN messaging_conversations c ON c.id = messaging_members.conversation_id
		   JOIN messaging_messages last ON last.id = (
		        SELECT msg.id FROM messaging_messages msg
		         WHERE msg.conversation_id = messaging_members.conversation_id
		           AND msg.created_at > messaging_members.last_read
		           AND msg.sender_id != messaging_members.user_id AND msg.deleted = 0 AND msg.system = 0
		         ORDER BY msg.created_at DESC, msg.rowid DESC LIMIT 1)
		  WHERE messaging_members.user_id = 'u' AND messaging_members.muted = 0
		    AND messaging_members.conversation_id IN ('a', 'b')`,
	} {
		rows, err := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			plan += d + "; "
		}
		rows.Close()
		if strings.Contains(plan, "SCAN") {
			t.Fatalf("not index-backed: %s\n-> %s", q, plan)
		}
	}
	if testing.Short() || raceEnabled {
		t.Skip("timings only without -short and without -race")
	}
	const people = 50_000
	now := time.Now().UTC()
	rows := make([]mailDue, people)
	for i := range rows {
		rows[i] = mailDue{userID: fmt.Sprintf("u%05d", i), convID: "c", dueAt: now.Add(time.Duration(i+1) * time.Second).UnixMilli()}
	}
	t0 := time.Now()
	if err := repo.AddMailDue(ctx, rows); err != nil {
		t.Fatal(err)
	}
	seed := time.Since(t0)
	const rounds = 200
	t0 = time.Now()
	for range rounds {
		if got, err := repo.MailDueUsers(ctx, now, mailBatch); err != nil || len(got) != 0 {
			t.Fatalf("nothing is due: %v %v", got, err)
		}
	}
	idle := time.Since(t0) / rounds
	t0 = time.Now()
	var due int
	for range rounds {
		got, err := repo.MailDueUsers(ctx, now.Add(10*time.Minute), mailBatch)
		if err != nil {
			t.Fatal(err)
		}
		due = len(got)
	}
	busy := time.Since(t0) / rounds

	// One real person among them, with something unread, written to.
	setNotify(t, s, you, func(p *NotifyPrefs) { p.Email = "10m" })
	dm, err := s.OpenDirect(ctx, me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if _, err := s.Send(ctx, me, dm.ID, fmt.Sprintf("message %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	t0 = time.Now()
	for range rounds {
		lines, err := repo.MailDigest(ctx, you.ID, []string{dm.ID})
		if err != nil || len(lines) != 1 || lines[0].Unread != 40 || lines[0].Words != "message 39" {
			t.Fatalf("digest: %+v %v", lines, err)
		}
	}
	read := time.Since(t0) / rounds
	t.Logf("%d waiting: seeded in %v; a tick with nothing due %v; with %d due (of a %d batch) %v; one person's email read %v",
		people, seed, idle, due, mailBatch, busy, read)
	if due != mailBatch {
		t.Fatalf("a tick takes a batch: %d", due)
	}
	if idle > time.Millisecond || busy > 5*time.Millisecond || read > 5*time.Millisecond {
		t.Fatalf("too slow for index reads: idle %v, busy %v, read %v", idle, busy, read)
	}
}
