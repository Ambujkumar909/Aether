package messaging

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// The meeting chat.
//
// Two people in a room are a call, and what they type goes to the direct
// conversation they already have. Three or more are a meeting, and a meeting
// gets a conversation of its own: created the moment the third signed-in
// person joins, everybody present made a member, later arrivals added as
// they come, and kept after the meeting ends -- the notes, the links and the
// "can you send that again" live there, which is the whole reason Teams
// does it this way.
//
// One conversation per room, for the life of the room name: a recurring
// meeting accumulates its history in one place. Guests (no account) are in
// the meeting but not in the conversation; they see the meeting's broadcast
// chat and nothing that outlives it.

// MeetingPerson is what the meeting transport knows about somebody in a
// room: their account and their name. Defined here so the signal package can
// call this module through a function value without importing it.
type MeetingPerson struct {
	UserID string
	Name   string
}

const meetingChatThreshold = 3

// MeetingJoined is called by the meeting transport after every join, with
// everybody currently in the room. Idempotent: calling it again with the
// same people does nothing.
func (s *Service) MeetingJoined(ctx context.Context, room, title string, people []MeetingPerson) {
	room = strings.TrimSpace(room)
	if room == "" {
		return
	}
	// Distinct signed-in accounts. A person on two devices is one person.
	seen := map[string]MeetingPerson{}
	for _, p := range people {
		if p.UserID != "" {
			seen[p.UserID] = p
		}
	}
	if len(seen) < meetingChatThreshold {
		// Still a call, or the third person is a guest. A conversation that
		// exists from an earlier meeting in this room still receives the
		// arrivals, though: it is the room's chat now.
		if c, err := s.repo.ConversationByMeetingRoom(ctx, room); err == nil && c != nil {
			s.admitToMeetingChat(ctx, c, seen)
		}
		return
	}

	c, err := s.repo.ConversationByMeetingRoom(ctx, room)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Warn("meeting chat lookup failed", "room", room, "err", err)
		return
	}
	if c == nil {
		c = s.createMeetingChat(ctx, room, title, seen)
		if c == nil {
			return
		}
	}
	s.admitToMeetingChat(ctx, c, seen)
}

func (s *Service) createMeetingChat(ctx context.Context, room, title string, people map[string]MeetingPerson) *Conversation {
	now := time.Now().UTC()
	var members []Member
	var founder MeetingPerson
	// In name order, so the auto-name and the member list read the same way
	// on every device rather than in map order.
	ordered := make([]MeetingPerson, 0, len(people))
	for _, p := range people {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].UserID < ordered[j].UserID
	})
	for _, p := range ordered {
		email, name, err := s.dir.LookupByID(ctx, p.UserID)
		if err != nil {
			continue
		}
		if name == "" {
			name = p.Name
		}
		if founder.UserID == "" {
			founder = MeetingPerson{UserID: p.UserID, Name: name}
		}
		members = append(members, Member{UserID: p.UserID, Email: email, Name: name, JoinedAt: now, LastRead: now, Role: RoleMember})
	}
	if len(members) < meetingChatThreshold {
		return nil
	}
	// A scheduled meeting's title names the chat; an ad-hoc room, whose
	// "title" is its id, gets the auto-name from its people.
	title = cleanTitle(title, 120)
	if title == room {
		title = ""
	}
	c := &Conversation{
		ID: newID(), Kind: KindGroup, Title: title, MeetingRoom: room,
		CreatedBy: founder.UserID, CreatedAt: now, LastMessageAt: now,
		Members: members,
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		// Two joins racing to create: the unique index lets one win; the
		// other finds it on the next call.
		existing, lookErr := s.repo.ConversationByMeetingRoom(ctx, room)
		if lookErr == nil && existing != nil {
			return existing
		}
		s.log.Warn("could not create the meeting chat", "room", room, "err", err)
		return nil
	}
	s.system(ctx, Actor{ID: founder.UserID, Name: founder.Name}, c.ID, "started a meeting")
	return c
}

// admitToMeetingChat adds whoever is in the room and not yet in the chat.
func (s *Service) admitToMeetingChat(ctx context.Context, c *Conversation, people map[string]MeetingPerson) {
	ids, err := s.repo.MemberIDs(ctx, c.ID)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	now := time.Now().UTC()
	var add []Member
	var names []string
	var actor Actor
	for _, p := range people {
		if have[p.UserID] {
			continue
		}
		email, name, err := s.dir.LookupByID(ctx, p.UserID)
		if err != nil {
			continue
		}
		if name == "" {
			name = p.Name
		}
		add = append(add, Member{UserID: p.UserID, Email: email, Name: name, JoinedAt: now, LastRead: now, Role: RoleMember})
		names = append(names, name)
		actor = Actor{ID: p.UserID, Email: email, Name: name}
	}
	if len(add) == 0 {
		return
	}
	if err := s.repo.AddMembers(ctx, c.ID, add); err != nil {
		s.log.Warn("could not add to the meeting chat", "err", err)
		return
	}
	if len(add) == 1 {
		s.system(ctx, actor, c.ID, "joined the meeting")
	} else {
		s.system(ctx, actor, c.ID, "joined the meeting with "+joinNames(names[:len(names)-1]))
	}
}

// MeetingChat is the conversation for a room, for a member of it.
func (s *Service) MeetingChat(ctx context.Context, a Actor, room string) (*Conversation, error) {
	c, err := s.repo.ConversationByMeetingRoom(ctx, strings.TrimSpace(room))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	if err := s.mustBeMember(ctx, c.ID, a.ID); err != nil {
		return nil, err
	}
	return s.decorate(ctx, a, c)
}
