package messaging

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Channel threads.
//
// A channel reads as posts, each with its replies folded beneath it, the
// way Teams and Slack channels do and a chat does not. Nothing new is
// stored: a reply is a message whose reply_to names the root, which the
// quote feature already writes. What changes is the reading. The channel
// feed is the roots (ListRoots), each carrying a summary of its replies --
// count, when the last came, who -- and a thread is the replies under one
// root (Replies). A reply to a reply is a reply to the root: threads are
// one level deep, because a second level is a maze.
//
// Pushes are unchanged: a reply arrives as a message with reply_to set, and
// the client adds one to the root's count and, if that thread is open,
// appends it there.

const MaxThreadPage = 200

// Replies lists the messages under one root, oldest first, for a member.
func (s *Service) Replies(ctx context.Context, a Actor, rootID string, limit int) ([]*Message, error) {
	root, err := s.repo.MessageByID(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if err := s.mustBeMember(ctx, root.ConversationID, a.ID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxThreadPage {
		limit = MaxThreadPage
	}
	ms, err := s.repo.ListReplies(ctx, rootID, limit)
	if err != nil {
		return nil, err
	}
	ms = withoutUndelivered(a.ID, s.withoutHidden(ctx, a, ms), time.Now().UTC())
	s.Decorate(ctx, a, ms)
	return ms, nil
}

// History lists what a message said before its edits, for a member.
func (s *Service) History(ctx context.Context, a Actor, messageID string) ([]Edit, error) {
	m, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if err := s.mustBeMember(ctx, m.ConversationID, a.ID); err != nil {
		return nil, err
	}
	if m.Deleted {
		return []Edit{}, nil
	}
	h, err := s.repo.EditHistory(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		h = []Edit{}
	}
	return h, nil
}

// decorateThreads fills reply summaries on the root posts of a channel page.
func (s *Service) decorateThreads(ctx context.Context, msgs []*Message) {
	var roots []string
	for _, m := range msgs {
		if m.ReplyTo == "" && !m.System {
			roots = append(roots, m.ID)
		}
	}
	if len(roots) == 0 {
		return
	}
	sums, err := s.repo.ThreadSummaries(ctx, roots)
	if err != nil {
		s.log.Warn("could not count replies", "err", err)
		return
	}
	for _, m := range msgs {
		if t := sums[m.ID]; t != nil {
			m.Thread = t
		}
	}
}

// rootOf flattens a reply-to-a-reply onto the root, in channels.
func (s *Service) rootOf(ctx context.Context, replyTo string) string {
	if replyTo == "" {
		return ""
	}
	q, err := s.repo.MessageByID(ctx, replyTo)
	if err != nil {
		return replyTo
	}
	if q.ReplyTo != "" {
		return q.ReplyTo
	}
	return replyTo
}

// Announcements: a card with a headline, in channels and groups.
type Announcement struct {
	Headline string `json:"headline"`
	Sub      string `json:"sub,omitempty"`
	Color    string `json:"color,omitempty"`
}

func announcementOf(meta string) *Announcement {
	if meta == "" || !strings.Contains(meta, `"announcement"`) {
		return nil
	}
	var wrap struct {
		A *Announcement `json:"announcement"`
	}
	if json.Unmarshal([]byte(meta), &wrap) != nil || wrap.A == nil {
		return nil
	}
	return wrap.A
}

func validateAnnouncement(a *Announcement) error {
	h, ok := cleanLine(a.Headline, 80)
	if !ok || h == "" {
		return ErrBadAnnouncement
	}
	a.Headline = h
	sub, ok := cleanLine(a.Sub, 200)
	if !ok {
		return ErrBadAnnouncement
	}
	a.Sub = sub
	switch a.Color {
	case "", "brand", "amber", "rose", "sky", "violet":
	default:
		return ErrBadAnnouncement
	}
	return nil
}
