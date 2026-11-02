package messaging

import (
	"context"
	"strings"
	"time"
)

// Blocking a person.
//
// A block is one person's decision about another, and it does one thing:
// while it stands, the direct conversation between them carries nothing --
// no messages either way, no calls, no typing. It does not delete anything,
// does not leave any group the two share, and is not announced. The API
// answers both sides with the same refusal, so being blocked is something a
// person can suspect and never confirm, which is the only design that does
// not turn "block" into "notify".
//
// The list is bounded. A thousand blocks is not a use case; it is a script.

const MaxBlocks = 1000

// BlockedPerson is one row of somebody's block list.
type BlockedPerson struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

func (s *Service) Block(ctx context.Context, a Actor, otherID string) error {
	otherID = strings.TrimSpace(otherID)
	if otherID == "" || otherID == a.ID {
		return ErrNotFound
	}
	if _, _, err := s.dir.LookupByID(ctx, otherID); err != nil {
		return ErrNotFound
	}
	n, err := s.repo.BlockCount(ctx, a.ID)
	if err != nil {
		return err
	}
	if n >= MaxBlocks {
		return ErrTooManyBlocks
	}
	return s.repo.Block(ctx, a.ID, otherID, time.Now().UTC())
}

func (s *Service) Unblock(ctx context.Context, a Actor, otherID string) error {
	otherID = strings.TrimSpace(otherID)
	if otherID == "" {
		return ErrNotFound
	}
	return s.repo.Unblock(ctx, a.ID, otherID)
}

// Blocks lists the people this person has blocked, newest first.
func (s *Service) Blocks(ctx context.Context, a Actor) ([]BlockedPerson, error) {
	ids, err := s.repo.BlockedIDs(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedPerson, 0, len(ids))
	for _, id := range ids {
		email, name, err := s.dir.LookupByID(ctx, id)
		if err != nil {
			// An account that has since gone: the block is kept, the row is
			// shown as what is known.
			email = ""
		}
		out = append(out, BlockedPerson{UserID: id, Name: name, Email: email})
	}
	return out, nil
}

// blockedInDirect reports whether the conversation is a direct one in which
// either side has blocked the other. Groups and channels are never blocked.
func (s *Service) blockedInDirect(ctx context.Context, c *Conversation, userID string) bool {
	if c == nil || c.Kind != KindDirect {
		return false
	}
	for _, m := range c.Members {
		if m.UserID != userID {
			blocked, err := s.repo.BlockedEither(ctx, userID, m.UserID)
			return err == nil && blocked
		}
	}
	return false
}

// markBlocked sets Conversation.Blocked on the direct conversations whose
// other person the viewer has blocked -- one query for the whole list.
func (s *Service) markBlocked(ctx context.Context, userID string, cs []*Conversation) {
	ids, err := s.repo.BlockedIDs(ctx, userID)
	if err != nil || len(ids) == 0 {
		return
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for _, c := range cs {
		if c.Kind != KindDirect {
			continue
		}
		for _, m := range c.Members {
			if m.UserID != userID && set[m.UserID] {
				c.Blocked = true
			}
		}
	}
}
