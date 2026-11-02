package messaging

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// Groups, the rest of the way: what a group of people needs that a pair of
// them does not.
//
//   - A name that writes itself. An untitled group is displayed as the other
//     people in it ("Priya, Rahul and 2 others"), never as "Group chat".
//   - History on joining. Somebody added later sees none of what came before,
//     the last N days of it, or all of it -- the adder's choice, applied as
//     delete-for-me rows on the newcomer's membership so the record itself is
//     untouched.
//   - Mentions. A message names people; they are told (a counter beside the
//     unread one, reset by the same read), and the message says so on the
//     push so the client can light it up.
//   - A picture. Stored like any file, served only to members, image types
//     only, small.
//   - One pinned message per conversation, for the thing everybody keeps
//     scrolling back to.
//   - Read cursors pushed live, so "seen by" is true rather than eventual.
//     Only for conversations small enough for a seen-by row to mean anything
//     (bigConversation); a thousand-member channel gets no such traffic.

const (
	MaxMentions = 20
	// MentionEveryone is the id the client sends for "@everyone"; MaxEveryone
	// is the largest conversation it expands in.
	MentionEveryone = "*"
	MaxEveryone     = 200
	MaxAvatarBytes  = 5 << 20
	HistoryNone     = "none"
	HistoryAll      = "all"
	HistoryDays     = "days"
	seenByThreshold = bigConversation
)

var (
	ErrNotAnImage = errors.New("messaging: a picture must be a PNG, JPEG, GIF or WebP")
	ErrNotInConv  = errors.New("messaging: that message is not in this conversation")
)

// History is what a newcomer gets to see.
type History struct {
	Mode string `json:"mode"` // none | all | days
	Days int    `json:"days,omitempty"`
}

// displayName writes the name of a group that has none: the other members,
// by first name, up to three, then a count.
func displayName(c *Conversation, viewerID string) string {
	if c.Kind != KindGroup || strings.TrimSpace(c.Title) != "" {
		return c.Title
	}
	// Full names while there are few enough to read; first names once it is
	// a crowd and a count carries the rest.
	var names, firsts []string
	for _, m := range c.Members {
		if m.UserID == viewerID {
			continue
		}
		n := strings.TrimSpace(m.Name)
		if n == "" {
			n = m.Email
		}
		names = append(names, n)
		if i := strings.IndexAny(n, " @"); i > 0 {
			n = n[:i]
		}
		firsts = append(firsts, n)
	}
	others := c.MemberCount - 1
	if len(c.Members) == 0 {
		// A big group ships no members: the count has to do.
		if others <= 0 {
			return "Group"
		}
		return "Group · " + itoa(others) + " people"
	}
	switch {
	case len(names) == 0:
		return "Group"
	case len(names) <= 3 && others <= 3:
		return joinNames(names)
	default:
		shown := firsts
		if len(shown) > 2 {
			shown = shown[:2]
		}
		return strings.Join(shown, ", ") + " and " + itoa(others-len(shown)) + " others"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// applyHistory hides what a newcomer is not meant to see.
func (s *Service) applyHistory(ctx context.Context, convID string, newcomers []Member, h History) {
	var before time.Time
	switch h.Mode {
	case HistoryAll:
		return
	case HistoryDays:
		days := h.Days
		if days <= 0 {
			days = 7
		}
		if days > 3650 {
			days = 3650
		}
		before = time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	default:
		before = time.Now().UTC()
	}
	for _, m := range newcomers {
		if err := s.repo.HideBefore(ctx, convID, m.UserID, before); err != nil {
			s.log.Warn("could not apply history choice", "err", err)
		}
	}
}

// resolveMentions keeps only members, deduplicated and bounded. A mention of
// somebody outside the conversation is dropped rather than refused: the
// client offered the name from the member list, so this is a race or a
// stale list, not a request to reject.
func (s *Service) resolveMentions(ctx context.Context, convID string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		// "@everyone" / "@channel": the client sends "*". Expanded here to the
		// members, so every one of them is counted as mentioned -- and only in
		// a conversation small enough that "everyone" is a set of people
		// rather than a broadcast list.
		if id == MentionEveryone {
			if c, err := s.repo.ConversationByID(ctx, convID); err == nil && c.Kind != KindDirect {
				if members, err := s.repo.MemberIDs(ctx, convID); err == nil && len(members) <= MaxEveryone {
					for _, m := range members {
						if !seen[m] {
							seen[m] = true
							out = append(out, m)
						}
					}
				}
			}
			seen[id] = true
			continue
		}
		seen[id] = true
		if ok, _ := s.repo.IsMember(ctx, convID, id); ok {
			out = append(out, id)
		}
		if len(out) >= MaxEveryone {
			break
		}
	}
	return out
}

// SendMessage is Send with everything a message can carry: a reply, files,
// mentions.
func (s *Service) SendMessage(ctx context.Context, a Actor, convID, body, replyTo string, staged []*Attachment, mentions []string) (*Message, error) {
	if len(staged) > MaxAttachmentsPerMessage {
		return nil, ErrTooManyFiles
	}
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	return s.post(ctx, a, convID, body, replyTo, false, staged, s.resolveMentions(ctx, convID, mentions))
}

// SetAvatar gives a group or channel a picture. Members may; image types
// only, sniffed; bounded; the old picture is removed.
func (s *Service) SetAvatar(ctx context.Context, a Actor, convID, name, declared string, r io.Reader) error {
	if s.files == nil {
		return ErrNoFiles
	}
	c, err := s.repo.ConversationByID(ctx, convID)
	if err != nil {
		return err
	}
	if c.Kind == KindDirect {
		return ErrDirectFixed
	}
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	head = head[:n]
	mime := DetectMIME(head, declared)
	if !isPictureType(mime) {
		return ErrNotAnImage
	}
	key := newID() + newID()
	limited := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), r), MaxAvatarBytes+1)
	size, err := s.files.Put(key, limited)
	if err != nil {
		_ = s.files.Remove(key)
		return err
	}
	if size > MaxAvatarBytes {
		_ = s.files.Remove(key)
		return ErrTooLarge
	}
	old, err := s.repo.SetAvatar(ctx, convID, key, mime)
	if err != nil {
		_ = s.files.Remove(key)
		return err
	}
	if old != "" {
		_ = s.files.Remove(old)
	}
	s.system(ctx, a, convID, "changed the picture")
	return nil
}

func isPictureType(m string) bool {
	switch m {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// OpenAvatar serves the picture to a member.
func (s *Service) OpenAvatar(ctx context.Context, a Actor, convID string) (mime string, f io.ReadSeekCloser, mod time.Time, err error) {
	if s.files == nil {
		return "", nil, time.Time{}, ErrNotFound
	}
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return "", nil, time.Time{}, err
	}
	key, mime, err := s.repo.Avatar(ctx, convID)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	if key == "" {
		return "", nil, time.Time{}, ErrNotFound
	}
	f, mod, err = s.files.Open(key)
	return mime, f, mod, err
}

// Pin keeps one message at the top of the conversation; an empty id unpins.
// Any member may: a pin is a shared bookmark, not a moderation act.
func (s *Service) Pin(ctx context.Context, a Actor, convID, messageID string) error {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return err
	}
	if messageID != "" {
		m, err := s.repo.MessageByID(ctx, messageID)
		if err != nil {
			return err
		}
		if m.ConversationID != convID || m.Deleted || m.System {
			return ErrNotInConv
		}
	}
	if err := s.repo.SetPinned(ctx, convID, messageID); err != nil {
		return err
	}
	if messageID == "" {
		s.system(ctx, a, convID, "unpinned a message")
	} else {
		s.system(ctx, a, convID, "pinned a message")
	}
	return nil
}
