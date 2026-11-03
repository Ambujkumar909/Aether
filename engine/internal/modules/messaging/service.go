package messaging

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Service is the messaging business logic.
type Service struct {
	// pushMu guards pushing: a message's push in progress (pushMessage).
	pushMu  sync.Mutex
	pushing map[string]*pushRun

	// LiveNow is the rooms' view: account id -> presenting?, for everyone
	// in a room this moment. Injected so this module never imports the
	// room package. Nil means no rooms, and no automatic status.
	LiveNow func() map[string]bool
	// nudge wakes the automatic-status loop the moment somebody joins or
	// leaves a room or starts or stops sharing, so peers see the change at
	// once rather than at the next tick. Buffered one deep: a nudge while
	// one is pending is the same nudge.
	nudge     chan struct{}
	nudgeOnce sync.Once
	// autoPushed observes the loop's pushes in tests; nil in production.
	autoPushed func(to string, st Status)
	// typingGate coalesces typing signals per person per conversation.
	typingGate *typingGate

	files FileStore // nil until UseFiles; uploads are refused without it
	// MayMessage reports whether one address may open a conversation with
	// another. Injected from identity, which owns the organisation policy, so
	// this module does not import it. Nil means no restriction, which is the
	// behaviour of a deployment that has not set one.
	//
	// Enforced here rather than left to the directory, because a rule the
	// search box obeys and the API ignores is a label, not a rule.
	MayMessage func(ctx context.Context, fromEmail, toEmail string) bool

	repo Repository
	dir  Directory
	hub  *Hub
	log  *slog.Logger
}

// reachable is the guard in front of every path that starts a conversation.
func (s *Service) reachable(ctx context.Context, a Actor, otherEmail string) bool {
	if s.MayMessage == nil {
		return true
	}
	return s.MayMessage(ctx, a.Email, otherEmail)
}

func NewService(repo Repository, dir Directory, hub *Hub, log *slog.Logger) *Service {
	return &Service{repo: repo, dir: dir, hub: hub, log: log, typingGate: newTypingGate()}
}

// Actor is who is acting, passed in so this module never imports identity.
type Actor struct {
	ID    string
	Email string
	Name  string
	// Device names the tab or app instance the request came from (the
	// X-Aether-Device header; the same id opens its push socket). A message
	// is delivered to the sender's other devices but never echoed to the
	// one that sent it, which already has it from the response. Empty for
	// a client without the header: then the sender is not pushed at all,
	// the older contract.
	Device string
}

// OpenDirect returns the two-person conversation with someone, creating it
// on first use.
//
// Idempotent by design. "Message this person" is a thing people do many times
// a day, and every one of those must land in the same thread rather than
// spawning a new one that splits the history.
// ContactIDs lists the accounts this person already shares a conversation
// with. It is the definition of "somebody you know" that the people directory
// uses, and it is deliberately the narrowest defensible one: a mutual thread,
// not a shared meeting.
//
// Being in the same 200-person meeting is not a relationship. If it counted,
// one all-hands would make every attendee permanently discoverable to every
// other, which is the exposure this exists to prevent. People in a meeting
// can already see each other's names in the roster and message each other
// from the meeting panel, which addresses accounts directly and never goes
// through the directory at all.
func (s *Service) ContactIDs(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	return s.repo.PeersOf(ctx, userID)
}

// OpenDirectByID is OpenDirect for somebody whose account id you have but
// whose address you do not.
//
// That is exactly the situation inside a meeting: the roster names accounts,
// not mailboxes, because broadcasting everybody's email address to everybody
// in the room -- guests included -- to make a chat panel work would be a
// disclosure the feature does not need. The address is resolved here, on the
// server, from an id the server itself put on the wire.
func (s *Service) OpenDirectByID(ctx context.Context, a Actor, otherID string) (*Conversation, error) {
	otherID = strings.TrimSpace(otherID)
	if otherID == "" {
		return nil, ErrNoMembers
	}
	if otherID == a.ID {
		return nil, ErrSelfDirect
	}
	email, _, err := s.dir.LookupByID(ctx, otherID)
	if err != nil || email == "" {
		return nil, ErrNotFound
	}
	return s.OpenDirect(ctx, a, email)
}

func (s *Service) OpenDirect(ctx context.Context, a Actor, otherEmail string) (*Conversation, error) {
	otherEmail = strings.ToLower(strings.TrimSpace(otherEmail))
	if otherEmail == "" {
		return nil, ErrNoMembers
	}
	// Your own address opens your own notes. Refusing it was the wrong
	// answer to the right question: a message to yourself is the most-used
	// thread in every product that has one, because it is where a link, a
	// code, or a thought goes on its way between two machines.
	if strings.EqualFold(otherEmail, a.Email) {
		return s.OpenSelf(ctx, a)
	}

	// Checked before the lookup, so a closed deployment cannot be used to ask
	// whether an outside address is registered.
	if !s.reachable(ctx, a, otherEmail) {
		return nil, ErrExternalClosed
	}

	otherID, otherName, err := s.dir.LookupByEmail(ctx, otherEmail)
	if err != nil {
		return nil, ErrNotFound
	}

	if existing, err := s.repo.DirectBetween(ctx, a.ID, otherID); err == nil && existing != nil {
		return s.decorate(ctx, a, existing)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	now := time.Now().UTC()
	c := &Conversation{
		ID: newID(), Kind: KindDirect, Title: "",
		CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members: []Member{
			{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now},
			{UserID: otherID, Email: otherEmail, Name: otherName, JoinedAt: now},
		},
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}

// OpenSelf returns the conversation somebody keeps with themselves, creating
// it on first use.
//
// One member, not two. A self conversation modelled as "you and you" would
// have to survive every query that counts distinct members, and the first one
// that did not would show somebody their own notes as a chat with a stranger.
func (s *Service) OpenSelf(ctx context.Context, a Actor) (*Conversation, error) {
	if a.ID == "" {
		return nil, ErrNoMembers
	}
	if existing, err := s.repo.SelfConversation(ctx, a.ID); err == nil && existing != nil {
		return s.decorate(ctx, a, existing)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	now := time.Now().UTC()
	c := &Conversation{
		ID: newID(), Kind: KindDirect, Title: "",
		CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members: []Member{
			{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now},
		},
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}

// Decorate fills the per-reader parts of a page of messages: reactions, the
// messages any replies quote, and which of them this person saved.
//
// Three queries for a whole page rather than three per message. A conversation
// opens with fifty messages; a query per message per feature is a hundred and
// fifty round trips to draw one screen, which is the difference between a chat
// that opens instantly and one that does not.
func (s *Service) Decorate(ctx context.Context, a Actor, msgs []*Message) {
	if len(msgs) == 0 {
		return
	}
	ids := make([]string, 0, len(msgs))
	var quoted []string
	for _, m := range msgs {
		ids = append(ids, m.ID)
		if m.ReplyTo != "" {
			quoted = append(quoted, m.ReplyTo)
		}
	}

	if groups, err := s.repo.ReactionsFor(ctx, a.ID, ids); err == nil {
		for _, m := range msgs {
			m.Reactions = groups[m.ID]
		}
	} else {
		s.log.Warn("could not read reactions", "err", err)
	}

	if len(quoted) > 0 {
		if quotes, err := s.repo.QuotesFor(ctx, quoted); err == nil {
			for _, m := range msgs {
				if m.ReplyTo != "" {
					m.Quoted = quotes[m.ReplyTo]
				}
			}
		} else {
			s.log.Warn("could not read quoted messages", "err", err)
		}
	}

	if saved, err := s.repo.SavedIDs(ctx, a.ID, ids); err == nil {
		for _, m := range msgs {
			m.Saved = saved[m.ID]
		}
	} else {
		s.log.Warn("could not read saved messages", "err", err)
	}

	s.decoratePolls(ctx, a, msgs)

	s.decorateAttachments(ctx, msgs, ids)
	if mentions, err := s.repo.MentionsFor(ctx, ids); err == nil {
		for _, m := range msgs {
			m.Mentions = mentions[m.ID]
		}
	} else {
		s.log.Warn("could not read mentions", "err", err)
	}
}

// React puts an emoji on a message, or takes it off.
//
// Membership is checked, not assumed: the message id is the only thing the
// client sends, and without this anybody could react to any message in the
// deployment and have it appear to everybody who can see it.
func (s *Service) React(ctx context.Context, a Actor, messageID, emoji string, on bool) error {
	emoji = strings.TrimSpace(emoji)
	if messageID == "" || emoji == "" {
		return ErrEmptyBody
	}
	// Two code points is a flag or a keycap; more is somebody pasting a
	// paragraph into the reaction field.
	if len([]rune(emoji)) > 8 {
		return ErrEmptyBody
	}
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
	if _, err := s.repo.React(ctx, messageID, a.ID, emoji, on); err != nil {
		return err
	}
	s.reactionActivity(ctx, a, m, emoji, on)

	// Everybody in the conversation sees it, including the person who did it:
	// their own client then draws from the same source as everybody else's
	// rather than from an optimistic guess that can drift.
	s.pushMessage(ctx, a, m.ConversationID, messageID)
	return nil
}

// SaveMessage marks a message for later, for one person only.
func (s *Service) SaveMessage(ctx context.Context, a Actor, messageID string, on bool) error {
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
	return s.repo.Save(ctx, a.ID, messageID, on)
}

// Saved is this person's saved list.
func (s *Service) Saved(ctx context.Context, a Actor, limit int) ([]*Message, error) {
	msgs, err := s.repo.SavedMessages(ctx, a.ID, limit)
	if err != nil {
		return nil, err
	}
	s.Decorate(ctx, a, msgs)
	return msgs, nil
}

// pushMessage re-sends one message to the conversation, so a change to it --
// a reaction, an edit -- reaches everybody without a second protocol.
//
// Pushes of one message run one at a time, each reading the state fresh, and
// a change that arrives during one marks the message dirty so the push runs
// again. Without this, 199 people voting at once each read a tally and
// pushed it, and two pushes read moments apart could reach a member in the
// opposite order: that member was left showing an older count, and nothing
// ever corrected it (the 200-person stress run, ARCHITECTURE 6bt). Serialised,
// the last change is always followed by a push of the final state; under a
// storm, most changes ride on a push already running, so 199 votes cost a few
// pushes rather than 199 of them to every member.
func (s *Service) pushMessage(ctx context.Context, a Actor, convID, messageID string) {
	s.pushMu.Lock()
	if s.pushing == nil {
		s.pushing = map[string]*pushRun{}
	}
	if run := s.pushing[messageID]; run != nil {
		run.dirty = true
		s.pushMu.Unlock()
		return
	}
	run := &pushRun{}
	s.pushing[messageID] = run
	s.pushMu.Unlock()
	for {
		s.pushOnce(ctx, a, convID, messageID)
		s.pushMu.Lock()
		if !run.dirty {
			delete(s.pushing, messageID)
			s.pushMu.Unlock()
			return
		}
		run.dirty = false
		s.pushMu.Unlock()
	}
}

// pushRun: a message's push in progress, and whether it changed meanwhile.
type pushRun struct{ dirty bool }

func (s *Service) pushOnce(ctx context.Context, a Actor, convID, messageID string) {
	m, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		return
	}
	s.Decorate(ctx, a, []*Message{m})
	ids, err := s.repo.MemberIDs(ctx, convID)
	if err != nil {
		return
	}
	for _, id := range ids {
		s.hub.PushMessage(id, m)
	}
}

// CreateGroup starts a named conversation.
func (s *Service) CreateGroup(ctx context.Context, a Actor, title string, emails []string) (*Conversation, error) {
	// An empty title is kept empty: the display name is written from the
	// members on every read (displayName), so it follows renames and
	// departures instead of freezing "Priya, Rahul" on the day it was made.
	title = cleanTitle(title, 120)

	now := time.Now().UTC()
	members := []Member{{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now, Role: RoleOwner}}
	seen := map[string]bool{strings.ToLower(a.Email): true}

	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if !s.reachable(ctx, a, e) {
			// Skipped like an address that is not a user: one person the
			// policy excludes should not discard the group.
			s.log.Debug("group invite skipped, outside the organisation", "email", e)
			continue
		}
		id, name, err := s.dir.LookupByEmail(ctx, e)
		if err != nil {
			// An address that is not a user is skipped rather than failing
			// the whole creation: one typo should not discard the other five
			// people who were typed correctly.
			s.log.Debug("group invite skipped, no such user", "email", e)
			continue
		}
		members = append(members, Member{UserID: id, Email: e, Name: name, JoinedAt: now, LastRead: now, Role: RoleMember})
	}
	if len(members) < 2 {
		return nil, ErrNoMembers
	}

	c := &Conversation{
		ID: newID(), Kind: KindGroup, Title: title,
		CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members: members,
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}

// List returns the actor's conversations, most recently active first.
func (s *Service) List(ctx context.Context, a Actor) ([]*Conversation, error) {
	cs, err := s.repo.ListConversations(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	// Two queries for the whole list, not two per conversation. Measured at
	// 200 threads over 360k messages: 14 ms per list the row-at-a-time way,
	// which is 14 ms on every app open for every person. See scale_test.go.
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	unread, err := s.repo.UnreadCounts(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	last, err := s.repo.LastMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	var bare []string
	for _, m := range last {
		if m.Body == "" && !m.Deleted {
			bare = append(bare, m.ID)
		}
	}
	if len(bare) > 0 {
		if atts, err := s.repo.AttachmentsFor(ctx, bare); err == nil {
			for _, m := range last {
				m.Attachments = atts[m.ID]
			}
		}
	}
	s.markBlocked(ctx, a.ID, cs)
	var pinnedIDs []string
	for _, c := range cs {
		s.name(a, c)
		c.Unread = unread[c.ID]
		if m := last[c.ID]; m != nil {
			c.Preview = previewOf(m)
			c.LastMessageAt = m.CreatedAt
		}
		if c.PinnedMessageID != "" {
			pinnedIDs = append(pinnedIDs, c.PinnedMessageID)
		}
	}
	if len(pinnedIDs) > 0 {
		if quotes, err := s.repo.QuotesFor(ctx, pinnedIDs); err == nil {
			for _, c := range cs {
				if c.PinnedMessageID != "" {
					c.Pinned = quotes[c.PinnedMessageID]
				}
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		return cs[i].LastMessageAt.After(cs[j].LastMessageAt)
	})
	return cs, nil
}

// Messages returns a page of history, newest last.
func (s *Service) Messages(ctx context.Context, a Actor, convID string, before time.Time, limit int) ([]*Message, error) {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	if before.IsZero() {
		before = time.Now().UTC().Add(time.Minute)
	}
	// A channel reads as posts with their replies folded beneath them: the
	// feed is the roots. A chat reads as one line of messages.
	var ms []*Message
	var err error
	c, cerr := s.repo.ConversationByID(ctx, convID)
	channel := cerr == nil && c.Kind == KindChannel
	if channel {
		ms, err = s.repo.ListRoots(ctx, convID, before, limit)
	} else {
		ms, err = s.repo.ListMessages(ctx, convID, before, limit)
	}
	if err != nil {
		return nil, err
	}
	// The repository returns newest first because that is what a LIMIT wants;
	// the interface reads oldest first because that is what a person wants.
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}
	ms = withoutUndelivered(a.ID, s.withoutHidden(ctx, a, ms), time.Now().UTC())
	if channel {
		s.decorateThreads(ctx, ms)
	}
	return ms, nil
}

// Send posts a message and pushes it to every other member who is connected.
// Send posts a message. ReplyTo, when set, names the message it answers.
func (s *Service) Send(ctx context.Context, a Actor, convID, body string) (*Message, error) {
	return s.SendReply(ctx, a, convID, body, "")
}

// SendReply is Send with a quoted message.
func (s *Service) SendReply(ctx context.Context, a Actor, convID, body, replyTo string) (*Message, error) {
	m, err := s.send(ctx, a, convID, body, replyTo)
	if err != nil {
		return nil, err
	}
	s.Decorate(ctx, a, []*Message{m})
	return m, nil
}

func (s *Service) send(ctx context.Context, a Actor, convID, body, replyTo string) (*Message, error) {
	return s.post(ctx, a, convID, body, replyTo, false, nil, nil)
}

// MaxForwardTargets bounds one forward. Ten is more than anybody means at
// once and few enough that a stolen session cannot use it as a megaphone.
const MaxForwardTargets = 10

// Forward carries a message into other conversations, as new messages from
// the actor, marked forwarded.
//
// All-or-nothing on membership: every target is checked before anything is
// written, so a list with one conversation you are not in produces nothing
// rather than a partial fan-out. The source must be readable by the actor
// (member of its conversation) and not deleted -- a tombstone has nothing to
// forward, and forwarding must never be a way to read one.
func (s *Service) Forward(ctx context.Context, a Actor, messageID string, to []string) ([]*Message, error) {
	src, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if err := s.mustBeMember(ctx, src.ConversationID, a.ID); err != nil {
		return nil, err
	}
	if src.Deleted {
		return nil, ErrNotFound
	}

	seen := map[string]bool{}
	targets := make([]string, 0, len(to))
	for _, id := range to {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return nil, ErrNoMembers
	}
	if len(targets) > MaxForwardTargets {
		return nil, ErrTooManyTargets
	}
	for _, id := range targets {
		if err := s.mustBeMember(ctx, id, a.ID); err != nil {
			return nil, err
		}
	}

	// The source's files travel with it: fresh rows in each target, sharing
	// the stored bytes, uploaded (as far as the record goes) by the person
	// forwarding. Loaded once, copied per target.
	srcAtts, err := s.repo.AttachmentsFor(ctx, []string{src.ID})
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(targets))
	for _, id := range targets {
		var copies []*Attachment
		for _, at := range srcAtts[src.ID] {
			c := at
			c.ID = newID()
			c.UploaderID = a.ID
			c.CreatedAt = time.Now().UTC()
			copies = append(copies, &c)
		}
		m, err := s.post(ctx, a, id, src.Body, "", true, copies, nil)
		if err != nil {
			return out, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Service) post(ctx context.Context, a Actor, convID, body, replyTo string, forwarded bool, atts []*Attachment, mentions []string) (*Message, error) {
	return s.postWith(ctx, a, convID, body, replyTo, forwarded, atts, mentions, SendOptions{})
}

func (s *Service) postWith(ctx context.Context, a Actor, convID, body, replyTo string, forwarded bool, atts []*Attachment, mentions []string, opts SendOptions) (*Message, error) {
	body = strings.TrimSpace(body)
	if body == "" && len(atts) == 0 {
		return nil, ErrEmptyBody
	}
	if len(body) > MaxBodyLength {
		return nil, ErrTooLong
	}
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	// A blocked direct conversation carries nothing, either way.
	if c, err := s.repo.ConversationByID(ctx, convID); err == nil && s.blockedInDirect(ctx, c, a.ID) {
		return nil, ErrBlocked
	}

	// A reply may only quote a message in the same conversation.
	//
	// Without this the reply target is an arbitrary id from the client, and
	// because the quoted text is loaded and returned on read, quoting a
	// message from a conversation you are not in would print its contents to
	// you. The cheapest possible leak, through a field that looks like
	// presentation.
	if replyTo != "" {
		q, err := s.repo.MessageByID(ctx, replyTo)
		if err != nil || q.ConversationID != convID {
			replyTo = ""
		} else if c, err := s.repo.ConversationByID(ctx, convID); err == nil && c.Kind == KindChannel {
			// Threads are one level deep: a reply to a reply joins the root's
			// thread.
			replyTo = s.rootOf(ctx, replyTo)
		}
	}

	m := &Message{
		ID: newID(), ConversationID: convID,
		SenderID: a.ID, SenderName: a.Name,
		Body: body, CreatedAt: time.Now().UTC(),
		ReplyTo:   replyTo,
		Forwarded: forwarded,
		Mentions:  mentions,
		Priority:  opts.Priority,
		DeliverAt: opts.DeliverAt,
		Meta:      opts.Meta,
	}
	if err := s.repo.AddMessageWithAttachments(ctx, m, atts); err != nil {
		return nil, err
	}
	for _, at := range atts {
		at.MessageID = m.ID
		at.ConversationID = convID
		m.Attachments = append(m.Attachments, *at)
	}
	if m.DeliverAt != nil && m.DeliverAt.After(m.CreatedAt) {
		// Scheduled: stored, shown to its author as pending, delivered by
		// RunScheduler. Nobody else hears of it until then.
		return m, nil
	}
	// The sender has, by definition, read their own message.
	_ = s.repo.MarkRead(ctx, convID, a.ID, m.CreatedAt)

	// The push carries the quote for a reply (see encodeConvMessage), so it
	// has to be loaded before the fanout, the same way the REST read does it.
	if m.ReplyTo != "" {
		s.Decorate(ctx, a, []*Message{m})
	}
	s.fanout(ctx, convID, a.ID, a.Device, m)
	s.activityForMessage(ctx, m)
	s.typingEnded(ctx, a, convID)
	return m, nil
}

// Edit rewrites a message. Only its author may.
func (s *Service) Edit(ctx context.Context, a Actor, msgID, body string) (*Message, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, ErrEmptyBody
	}
	if len(body) > MaxBodyLength {
		return nil, ErrTooLong
	}
	m, err := s.repo.MessageByID(ctx, msgID)
	if err != nil {
		return nil, err
	}
	if m.SenderID != a.ID {
		return nil, ErrForbidden
	}
	now := time.Now().UTC()
	if err := s.repo.EditMessage(ctx, msgID, body, now); err != nil {
		return nil, err
	}
	m.Body = body
	m.EditedAt = &now
	s.fanout(ctx, m.ConversationID, a.ID, a.Device, m)
	return m, nil
}

// Delete removes a message's body but keeps the row.
//
// A hard delete would silently renumber a conversation and let somebody
// erase what they said after somebody else replied to it. The tombstone is
// visible, which is the honest behaviour.
func (s *Service) Delete(ctx context.Context, a Actor, msgID string) error {
	m, err := s.repo.MessageByID(ctx, msgID)
	if err != nil {
		return err
	}
	if m.SenderID != a.ID {
		return ErrForbidden
	}
	if err := s.repo.SoftDeleteMessage(ctx, msgID); err != nil {
		return err
	}
	m.Deleted = true
	m.Body = ""
	s.fanout(ctx, m.ConversationID, a.ID, a.Device, m)
	return nil
}

// MarkRead advances the actor's read cursor.
func (s *Service) MarkRead(ctx context.Context, a Actor, convID string) error {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	at := time.Now().UTC()
	if err := s.repo.MarkRead(ctx, convID, a.ID, at); err != nil {
		return err
	}
	s.readActivityIn(ctx, a.ID, convID)
	// "Seen by" is true rather than eventual: the others learn now, and so
	// do this person's other devices (which is why the actor is included).
	for _, id := range s.smallAudience(ctx, convID) {
		s.hub.PushRead(id, convID, a.ID, at)
	}
	return nil
}

// Typing notifies the other members that somebody is composing.
//
// Never persisted. A typing indicator is worth exactly as much as the moment
// it describes, and writing it to a database would be spending durability on
// something that is stale before the transaction commits.
func (s *Service) Typing(ctx context.Context, a Actor, convID string, on bool) {
	// The gate first, before any query: a client that signals on every
	// keystroke costs the server one map lookup per keystroke and the
	// audience nothing. One "typing" per typingRepeat per person per
	// conversation goes through; a "stopped" only for somebody typing.
	if !s.typingGate.allow(a.ID, convID, on, time.Now()) {
		return
	}
	// Membership next: without it anybody could make "X is typing" appear
	// in any conversation on the server. And ids only, bounded to
	// conversations small enough for the indicator to mean anything.
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return
	}
	if c, err := s.repo.ConversationByID(ctx, convID); err == nil && s.blockedInDirect(ctx, c, a.ID) {
		return
	}
	for _, id := range s.smallAudience(ctx, convID) {
		if id != a.ID {
			s.hub.PushTyping(id, convID, a.Name, a.ID, on)
		}
	}
}

// typingEnded tells the audience a person has stopped typing because they
// sent: the message itself clears the indicator on a client that has it,
// and this covers the client that missed the message (a channel too big
// for the feed to be pushed, a frame lost to a reconnect).
func (s *Service) typingEnded(ctx context.Context, a Actor, convID string) {
	if s.typingGate.typing(a.ID, convID, time.Now()) {
		s.Typing(ctx, a, convID, false)
	}
}

// smallAudience is who to tell about ephemeral things -- typing, read
// cursors -- in a conversation small enough that they mean something. A
// thousand-member channel gets none of that traffic.
func (s *Service) smallAudience(ctx context.Context, convID string) []string {
	ids, err := s.repo.MemberIDs(ctx, convID)
	if err != nil || len(ids) > seenByThreshold {
		return nil
	}
	return ids
}

// Ring asks another account to join a room. It is delivered only if the
// target shares a direct conversation with the caller: a ring from a
// stranger is a nuisance vector, and "people you have talked to" is the
// same rule the contact list already applies.
func (s *Service) Ring(ctx context.Context, a Actor, targetID, room string) {
	if targetID == "" || room == "" || targetID == a.ID {
		return
	}
	if !s.shareDirect(ctx, a.ID, targetID) {
		return
	}
	if blocked, err := s.repo.BlockedEither(ctx, a.ID, targetID); err != nil || blocked {
		return
	}
	s.hub.PushRing(targetID, a.ID, a.Name, room)

	// Log it. The client tags an audio call by suffixing the room with
	// "?audio=1"; the room stored is the clean one so it matches attendance.
	kind := "video"
	cleanRoom := room
	if strings.HasSuffix(room, "?audio=1") {
		kind = "audio"
		cleanRoom = strings.TrimSuffix(room, "?audio=1")
	}
	if err := s.repo.RecordCall(ctx, &Call{
		ID: newID(), Room: cleanRoom, CallerID: a.ID, CalleeID: targetID,
		Kind: kind, StartedAt: time.Now().UTC(),
	}); err != nil {
		s.log.Warn("call log: could not record", "err", err)
	}
}

// CallsFor is the call log for a direct conversation the actor belongs to.
func (s *Service) CallsFor(ctx context.Context, a Actor, convID string) ([]*Call, error) {
	c, err := s.repo.ConversationByID(ctx, convID)
	if err != nil {
		return nil, err
	}
	if c.Kind != KindDirect {
		return []*Call{}, nil
	}
	var me, other bool
	var otherID string
	for _, m := range c.Members {
		if m.UserID == a.ID {
			me = true
		} else {
			other = true
			otherID = m.UserID
		}
	}
	if !me || !other {
		return nil, ErrForbidden
	}
	return s.repo.CallsBetween(ctx, a.ID, otherID, 50)
}

// RingAnswer relays accept/decline back to whoever rang.
func (s *Service) RingAnswer(ctx context.Context, a Actor, callerID, room string, accepted bool) {
	if callerID == "" || callerID == a.ID {
		return
	}
	s.hub.PushRingAnswer(callerID, a.ID, a.Name, room, accepted)
	cleanRoom := strings.TrimSuffix(room, "?audio=1")
	if err := s.repo.AnswerCall(ctx, cleanRoom, accepted); err != nil {
		s.log.Warn("call log: could not mark answer", "err", err)
	}
}

// shareDirect reports whether two accounts have a direct conversation --
// the "people you have talked to" rule the contact list already applies.
func (s *Service) shareDirect(ctx context.Context, a, b string) bool {
	c, err := s.repo.DirectBetween(ctx, a, b)
	return err == nil && c != nil
}

// AnnouncePresence tells everyone who shares a conversation with this user
// that they have arrived or left. Somebody appearing offline is never
// announced as arriving, and their departure carries no last-seen time.
//
// Scoped to actual contacts rather than broadcast: on a node with a thousand
// signed-in accounts, waking all of them because one person opened a laptop
// is precisely the kind of cost this system refuses to pay.
func (s *Service) AnnouncePresence(ctx context.Context, userID string, online bool) {
	hidden := s.hub.Hidden(userID)
	if online && hidden {
		return
	}
	var seen time.Time
	if !online && !hidden {
		// When they were last here: stamped as the socket closed, before
		// the grace ran out, so the peer's "last seen" is not late by it.
		if got, err := s.repo.LastSeenFor(ctx, []string{userID}); err == nil {
			seen = got[userID]
		}
		if seen.IsZero() {
			seen = time.Now().UTC()
		}
	}
	peers, err := s.repo.PeersOf(ctx, userID)
	if err != nil {
		return
	}
	for _, p := range peers {
		s.hub.PushPresence(p, userID, online, seen)
	}
}

// SocketOpened and SocketClosed are the presence side of a push socket's
// life: who is hiding, when they were last here.
func (s *Service) SocketOpened(ctx context.Context, userID string) {
	if st, ok, _ := s.manualStatus(ctx, userID); ok {
		s.hub.SetHidden(userID, st.Status == StatusOffline && (st.Until == nil || st.Until.After(time.Now())))
	}
	_ = s.repo.TouchSeen(ctx, userID, time.Now().UTC())
}

func (s *Service) SocketClosed(ctx context.Context, userID string) {
	_ = s.repo.TouchSeen(ctx, userID, time.Now().UTC())
}

// DeviceIdle records that one of a person's devices went idle or woke; when
// that changes the person's overall idleness the status loop is woken so
// "Away" appears or clears at once. Going idle stamps last-seen: it is the
// last moment they were active.
func (s *Service) DeviceIdle(ctx context.Context, sk *sock, idle bool) {
	if idle {
		_ = s.repo.TouchSeen(ctx, sk.userID, time.Now().UTC())
	}
	if s.hub.SetIdle(sk, idle) {
		s.NudgeAutoStatus()
	}
}

// Online reports whether a user has a live socket and is not appearing
// offline, satisfying the presence interface the identity directory
// consumes.
func (s *Service) Online(userID string) bool { return s.hub.Online(userID) && !s.hub.Hidden(userID) }

// OnlinePeers lists the people who share a conversation with this user and
// have a live socket this moment: what a newly connected client is told so
// that its first paint is right. One query (PeersOf, bounded to 500) and a
// map lookup each.
func (s *Service) OnlinePeers(ctx context.Context, userID string) []string {
	peers, err := s.repo.PeersOf(ctx, userID)
	if err != nil {
		return nil
	}
	out := peers[:0]
	for _, p := range peers {
		if s.Online(p) {
			out = append(out, p)
		}
	}
	return out
}

func (s *Service) fanout(ctx context.Context, convID, senderID, senderDevice string, m *Message) {
	// Ids only: a channel of two thousand must not load two thousand rows of
	// names and addresses to find out who to push to.
	ids, err := s.repo.MemberIDs(ctx, convID)
	if err != nil {
		return
	}
	for _, id := range ids {
		if id == senderID {
			// The sender's other devices -- the phone while the laptop
			// sent, the app while the browser did -- learn of it like
			// anybody else; the sending device does not, it has the
			// response. Without a device id nothing of theirs is pushed.
			if senderDevice != "" {
				s.hub.PushMessageExcept(id, senderDevice, m)
			}
			continue
		}
		s.hub.PushMessage(id, m)
	}
}

func (s *Service) mustBeMember(ctx context.Context, convID, userID string) error {
	ok, err := s.repo.IsMember(ctx, convID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// decorate fills the per-viewer fields: unread count, preview, and the name
// to show. A direct conversation has no stored title -- it is named after
// whoever you are talking to, which depends on who is looking.
// previewOf is the one line the list shows for a conversation's newest
// message. A message that is only files says so rather than showing nothing.
func previewOf(m *Message) string {
	if m.Deleted {
		return "Message deleted"
	}
	if m.System {
		return truncate(m.SenderName+" "+m.Body, 90)
	}
	if b := strings.TrimSpace(m.Body); b != "" {
		return truncate(b, 90)
	}
	switch n := len(m.Attachments); {
	case n == 1:
		return "📎 " + m.Attachments[0].Name
	case n > 1:
		return fmt.Sprintf("📎 %d files", n)
	}
	return ""
}

// decorate fills one conversation the row-at-a-time way; List batches.
func (s *Service) decorate(ctx context.Context, a Actor, c *Conversation) (*Conversation, error) {
	n, err := s.repo.UnreadCount(ctx, c.ID, a.ID)
	if err == nil {
		c.Unread = n
	}
	if last, err := s.repo.LastMessage(ctx, c.ID); err == nil && last != nil {
		if last.Body == "" && !last.Deleted {
			// A files-only message: its files are needed for the preview.
			if atts, err := s.repo.AttachmentsFor(ctx, []string{last.ID}); err == nil {
				last.Attachments = atts[last.ID]
			}
		}
		c.Preview = previewOf(last)
		c.LastMessageAt = last.CreatedAt
	}
	s.name(a, c)
	return c, nil
}

// name gives a conversation the title this person sees.
func (s *Service) name(a Actor, c *Conversation) {
	c.Display = displayName(c, a.ID)
	if c.Kind == KindChannel && c.Display != "" {
		c.Display = "#" + strings.TrimPrefix(c.Display, "#")
	}
	if c.Kind == KindDirect {
		for _, m := range c.Members {
			if m.UserID != a.ID {
				c.Display = m.Name
				if c.Display == "" {
					c.Display = m.Email
				}
				break
			}
		}
		// Nobody else in it: this is the thread you keep with yourself.
		// Named after you, marked, so the list reads the way it does
		// everywhere else that has this.
		if c.Display == "" {
			c.Display = strings.TrimSpace(a.Name)
			if c.Display == "" {
				c.Display = a.Email
			}
			c.Display += " (You)"
			c.SelfChat = true
		}
	}
	if c.Display == "" {
		c.Display = "Conversation"
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
