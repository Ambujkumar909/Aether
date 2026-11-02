package messaging

import "context"

// What one person decides about one conversation.
//
// Favourite pins it to the top of their list. Muted keeps it out of their
// badge total (the client honours the flag; the counter still counts, so
// un-muting shows the truth). Hidden takes it out of the list until somebody
// writes in it again. Forgetting (delete for me) hides it AND hides every
// message so far, for them alone. None of these touches anybody else's view
// or the conversation itself, which is why they live on the membership row.

type Prefs struct {
	Favourite *bool `json:"favourite,omitempty"`
	Muted     *bool `json:"muted,omitempty"`
	Hidden    *bool `json:"hidden,omitempty"`
}

func (s *Service) SetPrefs(ctx context.Context, a Actor, convID string, p Prefs) error {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	return s.repo.SetPrefs(ctx, convID, a.ID, p.Favourite, p.Muted, p.Hidden)
}

func (s *Service) MarkUnread(ctx context.Context, a Actor, convID string) error {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	return s.repo.MarkUnread(ctx, convID, a.ID)
}

func (s *Service) ForgetConversation(ctx context.Context, a Actor, convID string) error {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	return s.repo.ForgetConversation(ctx, convID, a.ID)
}
