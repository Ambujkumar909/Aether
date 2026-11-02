package messaging

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Polls.
//
// A poll is a message whose meta carries {"poll": {q, options, multi,
// anon}}; the votes are rows keyed by message, voter and option, and the
// tally rides on the message -- on read and on the push -- so every member
// sees the same numbers move at the same moment. Votes are per voter and
// replace that voter's previous choice; a vote with no options withdraws.
// Only members vote, only on messages that are polls, only on options the
// poll has. An anonymous poll ships counts and no names.

const (
	MinPollOptions = 2
	MaxPollOptions = 10
	MaxPollText    = 200
	MaxOptionText  = 80
)

// Poll is the shape inside meta.
type Poll struct {
	Q       string   `json:"q"`
	Options []string `json:"options"`
	Multi   bool     `json:"multi,omitempty"`
	Anon    bool     `json:"anon,omitempty"`
}

// pollOf reads a poll from a message's meta, or nil.
func pollOf(meta string) *Poll {
	if meta == "" || !strings.Contains(meta, `"poll"`) {
		return nil
	}
	var wrap struct {
		Poll *Poll `json:"poll"`
	}
	if json.Unmarshal([]byte(meta), &wrap) != nil || wrap.Poll == nil {
		return nil
	}
	return wrap.Poll
}

// validatePoll is applied when a message is composed with a poll in its
// meta, so a malformed poll never becomes a message.
func validatePoll(p *Poll) error {
	q, ok := cleanLine(p.Q, MaxPollText)
	if !ok || q == "" {
		return ErrBadPoll
	}
	p.Q = q
	if len(p.Options) < MinPollOptions || len(p.Options) > MaxPollOptions {
		return ErrBadPoll
	}
	for i, o := range p.Options {
		o, ok := cleanLine(o, MaxOptionText)
		if !ok || o == "" {
			return ErrBadPoll
		}
		p.Options[i] = o
	}
	return nil
}

// Vote records a member's choice and pushes the message, tally included,
// to everyone in the conversation.
func (s *Service) Vote(ctx context.Context, a Actor, messageID string, options []int) (*Message, error) {
	m, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	if err := s.mustBeMember(ctx, m.ConversationID, a.ID); err != nil {
		return nil, err
	}
	if m.Deleted {
		return nil, ErrNotFound
	}
	p := pollOf(m.Meta)
	if p == nil {
		return nil, ErrNotAPoll
	}
	seen := map[int]bool{}
	var clean []int
	for _, o := range options {
		if o < 0 || o >= len(p.Options) {
			return nil, ErrBadVote
		}
		if !seen[o] {
			seen[o] = true
			clean = append(clean, o)
		}
	}
	if !p.Multi && len(clean) > 1 {
		return nil, ErrBadVote
	}
	if err := s.repo.SetVotes(ctx, messageID, a.ID, clean, time.Now().UTC()); err != nil {
		return nil, err
	}
	// The push decorates the message (tally included) for everyone; the
	// caller's copy is decorated for them, with their own choice.
	s.pushMessage(ctx, a, m.ConversationID, m.ID)
	s.Decorate(ctx, a, []*Message{m})
	return m, nil
}

// decoratePolls fills tallies for the poll messages in a page: one query.
func (s *Service) decoratePolls(ctx context.Context, a Actor, msgs []*Message) {
	var ids []string
	polls := map[string]*Poll{}
	for _, m := range msgs {
		if p := pollOf(m.Meta); p != nil {
			ids = append(ids, m.ID)
			polls[m.ID] = p
		}
	}
	if len(ids) == 0 {
		return
	}
	tallies, err := s.repo.PollTalliesFor(ctx, a.ID, ids)
	if err != nil {
		s.log.Warn("could not read poll votes", "err", err)
		return
	}
	for _, m := range msgs {
		p := polls[m.ID]
		if p == nil {
			continue
		}
		t := tallies[m.ID]
		if t == nil {
			t = &PollTally{}
		}
		// Every option has a count, voted or not.
		for len(t.Counts) < len(p.Options) {
			t.Counts = append(t.Counts, 0)
			t.Voters = append(t.Voters, nil)
		}
		if p.Anon {
			t.Voters = nil
		}
		if t.Mine == nil {
			t.Mine = []int{}
		}
		m.Poll = t
	}
}
