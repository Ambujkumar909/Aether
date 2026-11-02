package room

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Breakout rooms, as Teams has them (strings and settings from its package):
// a host creates up to `maxBreakoutRoomCount = 50` rooms, "Room names must
// be 35 characters or fewer", assigns people ("Assign participants",
// "Shuffle"), and opens them; "Set a time limit" ("Room will close when time
// expires"), "Automatically move people to rooms", "Let people return to the
// main meeting", "Make an announcement", "Close Rooms". While they are open
// "You can't lock the meeting while breakout rooms are open."
//
// The state lives in the main meeting's Room. Each breakout room is an
// ordinary room of its own (media, chat, attendance all work unchanged)
// whose id the main meeting derives with a fresh random nonce per set of
// rooms, so a room cannot be found by guessing; who may enter it is decided
// by the gateway's tickets, not by its name.

const (
	MaxBreakoutRooms     = 50
	MaxBreakoutNameLen   = 35
	MaxAnnouncementLen   = 500
	BreakoutSep          = "~br~"
	MaxBreakoutTimeLimit = 24 * time.Hour
)

var (
	ErrBreakoutCount     = errors.New("room: breakout room count out of range")
	ErrBreakoutName      = errors.New("room: breakout room names must be 1 to 35 characters")
	ErrBreakoutMember    = errors.New("room: a breakout member is not in the meeting, is a host, or is assigned twice")
	ErrBreakoutsOpen     = errors.New("room: breakout rooms are open")
	ErrBreakoutsNone     = errors.New("room: there are no breakout rooms")
	ErrLockWhileOpen     = errors.New("room: you can't lock the meeting while breakout rooms are open")
	ErrBreakoutTimeLimit = errors.New("room: time limit out of range")
)

// BreakoutRoom is one room: its id, its name, and who is assigned to it
// (participant ids in the main meeting, with the names they had there).
type BreakoutRoom struct {
	ID      string
	Name    string
	Members []BreakoutMember
}

// BreakoutMember is an assignment.
type BreakoutMember struct {
	ID   string // participant id in the main meeting
	Name string
}

// BreakoutConfig is what a host sets.
type BreakoutConfig struct {
	Names       []string
	Members     [][]string // per room, participant ids
	TimeLimit   time.Duration
	AutoMove    bool
	AllowReturn bool
}

// Breakouts is the main meeting's breakout state.
type Breakouts struct {
	Nonce       string
	Rooms       []BreakoutRoom
	Open        bool
	ClosesAt    time.Time // zero: no limit
	TimeLimit   time.Duration
	AutoMove    bool
	AllowReturn bool
	// Managers are the accounts that may enter any room and run them: the
	// organizer and co-organizers when the rooms were opened. Kept here
	// because a manager who walks into a room is no longer in the main
	// meeting to be asked.
	Managers map[string]bool
}

// BreakoutParent returns the main meeting's id for a breakout room id, or ""
// if the id is not a breakout room's.
func BreakoutParent(id string) string {
	i := strings.Index(id, BreakoutSep)
	if i <= 0 {
		return ""
	}
	return id[:i]
}

// SetBreakouts creates or replaces the rooms. Only while closed: moving
// people between open rooms is "Close Rooms", reassign, open again (Teams
// allows it live; ponytail: live reassignment if it is asked for).
func (r *Room) SetBreakouts(cfg BreakoutConfig, nonce string) error {
	if len(cfg.Names) < 1 || len(cfg.Names) > MaxBreakoutRooms || len(cfg.Members) != len(cfg.Names) {
		return ErrBreakoutCount
	}
	if cfg.TimeLimit < 0 || cfg.TimeLimit > MaxBreakoutTimeLimit {
		return ErrBreakoutTimeLimit
	}
	for _, n := range cfg.Names {
		n = strings.TrimSpace(n)
		if n == "" || utf8.RuneCountInString(n) > MaxBreakoutNameLen {
			return ErrBreakoutName
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakouts != nil && r.breakouts.Open {
		return ErrBreakoutsOpen
	}
	seen := map[string]bool{}
	rooms := make([]BreakoutRoom, len(cfg.Names))
	for i, name := range cfg.Names {
		rooms[i] = BreakoutRoom{ID: r.ID + BreakoutSep + nonce + "~" + itoa(i+1), Name: strings.TrimSpace(name)}
		for _, pid := range cfg.Members[i] {
			p, ok := r.participants[pid]
			// "Breakout room managers can't be assigned to breakout rooms":
			// hosts stay to run them.
			if !ok || seen[pid] || pid == r.hostID || p.Role >= hostRole {
				return ErrBreakoutMember
			}
			seen[pid] = true
			rooms[i].Members = append(rooms[i].Members, BreakoutMember{ID: pid, Name: p.DisplayName})
		}
	}
	r.breakouts = &Breakouts{
		Nonce: nonce, Rooms: rooms, TimeLimit: cfg.TimeLimit,
		AutoMove: cfg.AutoMove, AllowReturn: cfg.AllowReturn,
	}
	return nil
}

// OpenBreakouts opens the rooms and returns them, for the caller to move
// each assigned person. managers are the accounts allowed into every room.
func (r *Room) OpenBreakouts(now time.Time, managers map[string]bool) (Breakouts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakouts
	if b == nil {
		return Breakouts{}, ErrBreakoutsNone
	}
	if b.Open {
		return Breakouts{}, ErrBreakoutsOpen
	}
	b.Open = true
	b.Managers = managers
	if b.TimeLimit > 0 {
		b.ClosesAt = now.Add(b.TimeLimit)
	}
	return b.copy(), nil
}

// CloseBreakouts closes the rooms (by a host, or by the timer) and returns
// them, for the caller to bring everyone back. The rooms stay defined, so
// they can be opened again, as Teams keeps them.
func (r *Room) CloseBreakouts() (Breakouts, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakouts
	if b == nil || !b.Open {
		return Breakouts{}, false
	}
	out := b.copy()
	b.Open = false
	b.ClosesAt = time.Time{}
	return out, true
}

// DeleteBreakouts removes closed rooms. "Breakout rooms were deleted".
func (r *Room) DeleteBreakouts() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakouts == nil {
		return ErrBreakoutsNone
	}
	if r.breakouts.Open {
		return ErrBreakoutsOpen
	}
	r.breakouts = nil
	return nil
}

// BreakoutState returns a copy of the state, and whether there is any.
func (r *Room) BreakoutState() (Breakouts, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.breakouts == nil {
		return Breakouts{}, false
	}
	return r.breakouts.copy(), true
}

// BreakoutsOpen reports whether rooms are open: then the main meeting is not
// over when it empties, and it cannot be locked.
func (r *Room) BreakoutsOpen() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.breakouts != nil && r.breakouts.Open
}

// BreakoutRoomByID finds a breakout room of this meeting.
func (r *Room) BreakoutRoomByID(id string) (BreakoutRoom, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.breakouts == nil {
		return BreakoutRoom{}, false
	}
	for _, br := range r.breakouts.Rooms {
		if br.ID == id {
			return br, true
		}
	}
	return BreakoutRoom{}, false
}

func (b *Breakouts) copy() Breakouts {
	out := *b
	out.Rooms = make([]BreakoutRoom, len(b.Rooms))
	for i, br := range b.Rooms {
		out.Rooms[i] = BreakoutRoom{ID: br.ID, Name: br.Name, Members: append([]BreakoutMember(nil), br.Members...)}
	}
	out.Managers = make(map[string]bool, len(b.Managers))
	for k, v := range b.Managers {
		out.Managers[k] = v
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
