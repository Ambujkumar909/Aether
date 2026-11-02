package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Channels and groups.
//
// A group is a conversation with a chosen set of people; a channel is a
// named place people join. Both are conversations underneath -- the same
// messages, pushes, files and reactions -- with three things on top: a
// membership that changes over time, a record of those changes in the thread
// itself (system messages, so "Priya added Rahul" is where it happened and
// arrives at everybody through the same push as any message), and, for
// channels, a name that can be browsed.
//
// Visibility. A public channel can be found and joined by anybody the
// creator could message (the organisation rule, MayMessage, decides that, so
// an outsider never sees a company's channel list). A private channel is
// invisible to non-members and entered by invitation only. Groups are always
// private.
//
// Roles. The creator is the owner. Owners rename, change the topic and
// remove people; any member may add people (a group that only its founder
// could grow would be a mailing list) and anybody may leave. When an owner
// leaves, the longest-standing remaining member inherits the role, so a
// channel is never left with nobody who can tend it.

const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"

	RoleOwner  = "owner"
	RoleMember = "member"

	MaxChannelName  = 60
	MaxChannelTopic = 240
	MaxAddAtOnce    = 50
	// bigConversation is the membership above which the list stops shipping
	// every member with every conversation and sends the count instead.
	bigConversation = 40
)

var (
	ErrChannelExists = errors.New("messaging: a channel with that name already exists")
	ErrBadName       = errors.New("messaging: a channel needs a name")
	ErrNotOwner      = errors.New("messaging: only the owner may do that")
	ErrNotJoinable   = errors.New("messaging: that channel is private")
	ErrDirectFixed   = errors.New("messaging: a direct conversation's members cannot change")
)

// ChannelSummary is one row of the browse view.
type ChannelSummary struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Topic       string    `json:"topic,omitempty"`
	Visibility  string    `json:"visibility"`
	MemberCount int       `json:"memberCount"`
	CreatedAt   time.Time `json:"createdAt"`
	Joined      bool      `json:"joined"`

	creatorID string
}

func cleanTitle(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if rs := []rune(s); len(rs) > max {
		s = string(rs[:max])
	}
	return s
}

// CreateChannel makes a named place and puts its creator in it as owner.
// DeleteChannel removes a channel and everything in it. Owners only. The
// files go with it; the members find it gone on their next list.
func (s *Service) DeleteChannel(ctx context.Context, a Actor, convID string) error {
	c, err := s.repo.ConversationByID(ctx, convID)
	if err != nil {
		return err
	}
	if c.Kind != KindChannel {
		return ErrNotFound
	}
	role, err := s.repo.MemberOf(ctx, convID, a.ID)
	if err != nil || role == nil {
		return ErrForbidden
	}
	if role.Role != RoleOwner {
		return ErrNotOwner
	}
	atts, err := s.repo.DeleteConversation(ctx, convID)
	if err != nil {
		return err
	}
	if s.files != nil {
		for _, at := range atts {
			_ = s.files.Remove(at.Blob)
		}
	}
	return nil
}

func (s *Service) CreateChannel(ctx context.Context, a Actor, name, topic, visibility string) (*Conversation, error) {
	name = strings.TrimPrefix(cleanTitle(name, MaxChannelName), "#")
	if name == "" {
		return nil, ErrBadName
	}
	topic = cleanTitle(topic, MaxChannelTopic)
	if visibility != VisibilityPublic {
		visibility = VisibilityPrivate
	}
	now := time.Now().UTC()
	c := &Conversation{
		ID: newID(), Kind: KindChannel, Title: name, Topic: topic, Visibility: visibility,
		CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members:     []Member{{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now, Role: RoleOwner}},
		MemberCount: 1,
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrChannelExists
		}
		return nil, err
	}
	s.system(ctx, a, c.ID, fmt.Sprintf("created the channel #%s", name))
	return s.decorate(ctx, a, c)
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed: unique")
}

// BrowseChannels lists the public channels this person could join, plus
// whether they already have. Bounded, and narrowed by a typed prefix or
// substring of the name.
func (s *Service) BrowseChannels(ctx context.Context, a Actor, q string, limit int) ([]ChannelSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.repo.PublicChannels(ctx, a.ID, strings.TrimSpace(q), limit*2)
	if err != nil {
		return nil, err
	}
	// The organisation rule, applied to the creator: a channel is a place in
	// its creator's organisation, and somebody who could not message the
	// creator does not get to browse it.
	out := rows[:0]
	for _, ch := range rows {
		if !ch.Joined && !s.mayReachUser(ctx, a, ch.creatorID) {
			continue
		}
		out = append(out, ch)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Service) mayReachUser(ctx context.Context, a Actor, userID string) bool {
	if s.MayMessage == nil || userID == "" || userID == a.ID {
		return true
	}
	email, _, err := s.dir.LookupByID(ctx, userID)
	if err != nil {
		return false
	}
	return s.MayMessage(ctx, a.Email, email)
}

// JoinChannel puts the actor into a public channel. Idempotent.
func (s *Service) JoinChannel(ctx context.Context, a Actor, id string) (*Conversation, error) {
	c, err := s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Kind != KindChannel {
		return nil, ErrNotFound
	}
	if ok, err := s.repo.IsMember(ctx, id, a.ID); err != nil {
		return nil, err
	} else if ok {
		return s.decorate(ctx, a, c)
	}
	if c.Visibility != VisibilityPublic {
		return nil, ErrNotJoinable
	}
	if !s.mayReachUser(ctx, a, c.CreatedBy) {
		return nil, ErrExternalClosed
	}
	now := time.Now().UTC()
	if err := s.repo.AddMembers(ctx, id, []Member{{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now, Role: RoleMember}}); err != nil {
		return nil, err
	}
	s.system(ctx, a, id, "joined")
	c, err = s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}

// AddMembers invites people into a group or channel. Any member may. People
// the organisation rule excludes, or who have no account, are skipped rather
// than failing the rest -- and named in the response so the interface can say
// so.
func (s *Service) AddMembers(ctx context.Context, a Actor, id string, emails []string, history ...History) (*Conversation, []string, error) {
	h := History{Mode: HistoryAll}
	if len(history) > 0 {
		h = history[0]
	}
	c, err := s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if c.Kind == KindDirect {
		return nil, nil, ErrDirectFixed
	}
	if err := s.mustBeMember(ctx, id, a.ID); err != nil {
		return nil, nil, err
	}
	if len(emails) > MaxAddAtOnce {
		emails = emails[:MaxAddAtOnce]
	}
	now := time.Now().UTC()
	var add []Member
	var skipped []string
	seen := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if !s.reachable(ctx, a, e) {
			skipped = append(skipped, e)
			continue
		}
		uid, name, err := s.dir.LookupByEmail(ctx, e)
		if err != nil {
			skipped = append(skipped, e)
			continue
		}
		if ok, _ := s.repo.IsMember(ctx, id, uid); ok {
			continue
		}
		add = append(add, Member{UserID: uid, Email: e, Name: name, JoinedAt: now, LastRead: now, Role: RoleMember})
	}
	if len(add) > 0 {
		if err := s.repo.AddMembers(ctx, id, add); err != nil {
			return nil, nil, err
		}
		s.applyHistory(ctx, id, add, h)
		names := make([]string, 0, len(add))
		for _, m := range add {
			n := m.Name
			if n == "" {
				n = m.Email
			}
			names = append(names, n)
		}
		s.system(ctx, a, id, "added "+joinNames(names))
	}
	c, err = s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	c, _ = s.decorate(ctx, a, c)
	return c, skipped, nil
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// RemoveMember takes somebody out. Owners may remove anyone; anyone may
// remove themselves (that is Leave).
func (s *Service) RemoveMember(ctx context.Context, a Actor, id, userID string) error {
	c, err := s.repo.ConversationByID(ctx, id)
	if err != nil {
		return err
	}
	if c.Kind == KindDirect {
		return ErrDirectFixed
	}
	me, err := s.repo.MemberOf(ctx, id, a.ID)
	if err != nil {
		return err
	}
	if me == nil {
		return ErrForbidden
	}
	if userID != a.ID && me.Role != RoleOwner {
		return ErrNotOwner
	}
	target, err := s.repo.MemberOf(ctx, id, userID)
	if err != nil {
		return err
	}
	if target == nil {
		return nil // already gone; nothing to do and nothing to announce
	}
	if err := s.repo.RemoveMember(ctx, id, userID); err != nil {
		return err
	}
	// An owner who leaves hands the role to whoever has been there longest.
	if target.Role == RoleOwner {
		_ = s.repo.PromoteOldest(ctx, id)
	}
	if userID == a.ID {
		s.system(ctx, a, id, "left")
	} else {
		n := target.Name
		if n == "" {
			n = target.Email
		}
		s.system(ctx, a, id, "removed "+n)
	}
	return nil
}

// Leave is RemoveMember on yourself.
func (s *Service) Leave(ctx context.Context, a Actor, id string) error {
	return s.RemoveMember(ctx, a, id, a.ID)
}

// Rename changes a group's or channel's title and topic. Owners for a
// channel; any member for a group, which has no owner in the same sense.
func (s *Service) Rename(ctx context.Context, a Actor, id, title, topic string) (*Conversation, error) {
	c, err := s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Kind == KindDirect {
		return nil, ErrDirectFixed
	}
	me, err := s.repo.MemberOf(ctx, id, a.ID)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, ErrForbidden
	}
	if c.Kind == KindChannel && me.Role != RoleOwner {
		return nil, ErrNotOwner
	}
	title = strings.TrimPrefix(cleanTitle(title, MaxChannelName), "#")
	topic = cleanTitle(topic, MaxChannelTopic)
	if title == "" {
		title = c.Title
	}
	if err := s.repo.UpdateConversation(ctx, id, title, topic); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrChannelExists
		}
		return nil, err
	}
	switch {
	case title != c.Title && topic != c.Topic:
		s.system(ctx, a, id, fmt.Sprintf("renamed this to %q and set the topic", title))
	case title != c.Title:
		s.system(ctx, a, id, fmt.Sprintf("renamed this to %q", title))
	case topic != c.Topic:
		s.system(ctx, a, id, "changed the topic to "+topic)
	}
	c, err = s.repo.ConversationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}

// Members pages through a conversation's membership, for the people panel.
func (s *Service) Members(ctx context.Context, a Actor, id string, offset, limit int) ([]Member, int, error) {
	if err := s.mustBeMember(ctx, id, a.ID); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return s.repo.MembersPage(ctx, id, offset, limit)
}

// system posts a membership event into the thread. It rides the same path
// as any message -- stored, pushed, previewed -- with a flag so the client
// draws it as an event rather than as something the actor said. It does not
// count as unread: nobody needs a badge to learn that somebody joined.
func (s *Service) system(ctx context.Context, a Actor, convID, text string) {
	m := &Message{
		ID: newID(), ConversationID: convID,
		SenderID: a.ID, SenderName: a.Name,
		Body: text, CreatedAt: time.Now().UTC(), System: true,
	}
	if err := s.repo.AddMessageWithAttachments(ctx, m, nil); err != nil {
		s.log.Warn("could not record a membership event", "err", err)
		return
	}
	s.fanout(ctx, convID, "", "", m)
}
