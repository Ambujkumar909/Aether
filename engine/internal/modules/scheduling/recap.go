package scheduling

import (
	"context"
	"sort"
	"time"
)

// The recap: what a meeting left behind, on one page, for the people who
// were in it or were meant to be. Nothing new is stored; it is the
// attendance, the notes and the transcript the meeting already wrote,
// read together after the fact. Access is "attended, or invited": the
// person who missed the meeting they were invited to is exactly who needs
// the recap, and a stranger with the room name is exactly who must not
// have it.

// RecapPerson is one attendee, their visits summed.
type RecapPerson struct {
	UserID   string    `json:"userId"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joinedAt"`
	LeftAt   time.Time `json:"leftAt,omitzero"`
	Seconds  int64     `json:"seconds"`
	Present  bool      `json:"present,omitempty"` // still in the room
}

// Recap is the page.
type Recap struct {
	Room      string    `json:"room"`
	Title     string    `json:"title"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
	// Seconds from the first arrival to the last departure.
	Seconds   int64            `json:"seconds"`
	Live      bool             `json:"live"`
	People    []RecapPerson    `json:"people"`
	Notes     []*NoteItem      `json:"notes"`
	HasNotes  bool             `json:"hasNotes"`
	Session   int64            `json:"transcriptSession,omitempty"`
	Lines     []TranscriptLine `json:"transcript"`
	Scheduled *Meeting         `json:"scheduled,omitempty"`
}

// RecapWindow bounds how far back the attendance is read. A room name is
// reused by a recurring series; the recap is of its latest gathering, and
// a gathering is taken to be one that started within this window of the
// latest departure.
const RecapWindow = 30 * 24 * time.Hour

// Recap assembles the page for someone who attended the room or was
// invited to the meeting behind it.
func (s *Service) Recap(ctx context.Context, a Actor, room string, now time.Time) (*Recap, error) {
	if room == "" || len(room) > MaxRoomIDLen || a.ID == "" {
		return nil, ErrForbidden
	}
	attended, err := s.repo.Attended(ctx, room, a.ID)
	if err != nil {
		return nil, err
	}
	if !attended {
		invited, err := s.repo.IsInvited(ctx, room, a.ID, a.Email)
		if err != nil {
			return nil, err
		}
		if !invited && !a.Admin {
			return nil, ErrForbidden
		}
	}
	visits, err := s.repo.RoomAttendance(ctx, room, now.Add(-RecapWindow), 5000)
	if err != nil {
		return nil, err
	}
	r := &Recap{Room: room, People: []RecapPerson{}, Notes: []*NoteItem{}, Lines: []TranscriptLine{}}
	if m, err := s.repo.ByRoom(ctx, room); err == nil && m != nil {
		m.firstOccurrence()
		r.Title = m.Title
		r.Scheduled = m
	}
	if r.Title == "" {
		r.Title = room
	}
	// People: one row each, visits summed, the earliest arrival and the
	// latest departure kept. Somebody still present has no departure.
	by := map[string]*RecapPerson{}
	var order []string
	for _, v := range visits {
		key := v.UserID
		if key == "" {
			key = "name:" + v.Name
		}
		p := by[key]
		if p == nil {
			p = &RecapPerson{UserID: v.UserID, Name: v.Name, JoinedAt: v.JoinedAt}
			by[key] = p
			order = append(order, key)
		}
		if v.JoinedAt.Before(p.JoinedAt) {
			p.JoinedAt = v.JoinedAt
		}
		if v.LeftAt.IsZero() {
			p.Present = true
			p.Seconds += int64(now.Sub(v.JoinedAt).Seconds())
		} else {
			if v.LeftAt.After(p.LeftAt) {
				p.LeftAt = v.LeftAt
			}
			p.Seconds += int64(v.LeftAt.Sub(v.JoinedAt).Seconds())
		}
		if r.StartedAt.IsZero() || v.JoinedAt.Before(r.StartedAt) {
			r.StartedAt = v.JoinedAt
		}
		if v.LeftAt.After(r.EndedAt) {
			r.EndedAt = v.LeftAt
		}
	}
	for _, k := range order {
		p := by[k]
		if p.Present {
			p.LeftAt = time.Time{}
			r.Live = true
		}
		r.People = append(r.People, *p)
	}
	sort.SliceStable(r.People, func(i, j int) bool { return r.People[i].JoinedAt.Before(r.People[j].JoinedAt) })
	if r.Live {
		r.EndedAt = time.Time{}
		r.Seconds = int64(now.Sub(r.StartedAt).Seconds())
	} else if !r.StartedAt.IsZero() {
		r.Seconds = int64(r.EndedAt.Sub(r.StartedAt).Seconds())
	}
	if created, err := s.repo.NotesCreated(ctx, room); err == nil && created {
		r.HasNotes = true
		if items, err := s.repo.NoteItems(ctx, room, ""); err == nil && items != nil {
			r.Notes = items
		}
	}
	if session, err := s.repo.LatestTranscriptSession(ctx, room); err == nil && session > 0 {
		if lines, err := s.repo.TranscriptLines(ctx, room, session); err == nil && lines != nil {
			r.Session = session
			r.Lines = lines
		}
	}
	return r, nil
}
