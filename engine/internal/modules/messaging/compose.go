package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// What a message can carry beyond its text: a priority, a delivery time, and
// a small structured payload (a praise card, a poll -- whatever the client
// can draw). Plus search over everything you can read.
//
// Priority is a label the client shows and, for "urgent", a reason to
// notify harder; it changes nothing on the server beyond being stored and
// pushed. A scheduled message is stored at once, visible to its author as
// "scheduled", invisible to everybody else, and becomes an ordinary message
// -- stamped, counted, pushed -- when its time comes (RunScheduler). Meta is
// opaque JSON, bounded, validated as an object, never interpreted here:
// the server's job is to carry it faithfully, the client's to draw it.

const (
	PriorityNormal    = ""
	PriorityImportant = "important"
	PriorityUrgent    = "urgent"

	MaxMetaBytes    = 2048
	MinScheduleLead = 30 * time.Second
	MaxScheduleLead = 30 * 24 * time.Hour
	schedulerTick   = 2 * time.Second
)

var (
	ErrBadPriority = errors.New("messaging: priority must be important or urgent")
	ErrBadSchedule = errors.New("messaging: a scheduled time must be between 30 seconds and 30 days from now")
	ErrBadMeta     = errors.New("messaging: meta must be a small JSON object")
)

// SendOptions is everything optional about a send.
type SendOptions struct {
	Priority  string
	DeliverAt *time.Time
	Meta      string
}

func (o SendOptions) validate(now time.Time) error {
	switch o.Priority {
	case PriorityNormal, PriorityImportant, PriorityUrgent:
	default:
		return ErrBadPriority
	}
	if o.DeliverAt != nil {
		lead := o.DeliverAt.Sub(now)
		if lead < MinScheduleLead || lead > MaxScheduleLead {
			return ErrBadSchedule
		}
	}
	if o.Meta != "" {
		if len(o.Meta) > MaxMetaBytes || !json.Valid([]byte(o.Meta)) || !strings.HasPrefix(strings.TrimSpace(o.Meta), "{") {
			return ErrBadMeta
		}
		if p := pollOf(o.Meta); p != nil {
			if err := validatePoll(p); err != nil {
				return err
			}
		}
		if an := announcementOf(o.Meta); an != nil {
			if err := validateAnnouncement(an); err != nil {
				return err
			}
		}
	}
	return nil
}

// Compose is the full send: text, reply, files, mentions, and the options.
func (s *Service) Compose(ctx context.Context, a Actor, convID, body, replyTo string, staged []*Attachment, mentions []string, opts SendOptions) (*Message, error) {
	if len(staged) > MaxAttachmentsPerMessage {
		return nil, ErrTooManyFiles
	}
	if err := opts.validate(time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	m, err := s.postWith(ctx, a, convID, body, replyTo, false, staged, s.resolveMentions(ctx, convID, mentions), opts)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// RunScheduler delivers scheduled messages when their time comes. One loop
// per process; stops with the context.
func (s *Service) RunScheduler(ctx context.Context) {
	t := time.NewTicker(schedulerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.deliverDue(ctx)
		}
	}
}

// deliverDue is one pass. Exported through DeliverDueNow for tests, which
// should not have to wait for a ticker.
func (s *Service) deliverDue(ctx context.Context) int {
	now := time.Now().UTC()
	ids, err := s.repo.DueScheduled(ctx, now, 200)
	if err != nil {
		s.log.Warn("scheduled delivery: could not list due messages", "err", err)
		return 0
	}
	n := 0
	for _, id := range ids {
		m, err := s.repo.Deliver(ctx, id, now)
		if err != nil {
			s.log.Warn("scheduled delivery failed", "id", id, "err", err)
			continue
		}
		if m == nil {
			continue
		}
		s.Decorate(ctx, Actor{ID: m.SenderID}, []*Message{m})
		// Everybody, the author's other devices included: the message just
		// became real for them too.
		if ids, err := s.repo.MemberIDs(ctx, m.ConversationID); err == nil {
			for _, uid := range ids {
				s.hub.PushMessage(uid, m)
			}
		}
		n++
	}
	return n
}

// DeliverDueNow runs one delivery pass immediately.
func (s *Service) DeliverDueNow(ctx context.Context) int { return s.deliverDue(ctx) }

// withoutUndelivered drops other people's scheduled messages from a page:
// the author sees theirs, marked; nobody else sees them at all.
func withoutUndelivered(viewerID string, msgs []*Message, now time.Time) []*Message {
	out := msgs[:0]
	for _, m := range msgs {
		if m.DeliverAt != nil && m.DeliverAt.After(now) && m.SenderID != viewerID {
			continue
		}
		out = append(out, m)
	}
	return out
}

// SearchMessages finds messages you can read whose text contains q --
// case-insensitively, anywhere in the word -- newest first. An index lookup
// (trigram FTS5 over the message bodies), bounded before the membership
// join, so a common word over a million messages stays milliseconds.
func (s *Service) SearchMessages(ctx context.Context, a Actor, q string, limit int) ([]*Message, error) {
	return s.SearchIn(ctx, a, q, "", limit)
}

// SearchIn is a search narrowed to one conversation ("" for all). The
// membership join in the query is what keeps a conversation the searcher is
// not in from answering: no separate check, no separate way to get it wrong.
func (s *Service) SearchIn(ctx context.Context, a Actor, q, convID string, limit int) ([]*Message, error) {
	q = cleanText(q)
	if len([]rune(q)) < 3 {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 30
	}
	msgs, err := s.repo.SearchMessages(ctx, a.ID, q, strings.TrimSpace(convID), limit)
	if err != nil {
		return nil, err
	}
	msgs = withoutUndelivered(a.ID, msgs, time.Now().UTC())
	s.Decorate(ctx, a, msgs)
	return msgs, nil
}

func cleanText(q string) string {
	q = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, q)
	q = strings.TrimSpace(q)
	if len(q) > 120 {
		q = q[:120]
	}
	return q
}
