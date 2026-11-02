package room

import (
	"errors"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// Meeting roles, as Teams has them, on the roles the wire already carries:
//
//	Organizer     -- hostID, the one who called the meeting (RoleHost)
//	Co-organizer  -- promoted by a host; may do everything a host does (RoleHost)
//	Presenter     -- may unmute, share and turn on video whatever the
//	                 attendee controls say (RoleModerator)
//	Attendee      -- bound by the meeting's controls (RoleMember)
//
// The controls in permissions.go apply to attendees only, which is what
// Teams' options say in so many words: "Allow mic for attendees", "Allow
// camera for attendees". Somebody made a presenter is trusted to speak.

var (
	ErrNotPermittedRole = errors.New("room: that role cannot be given")
	ErrOrganizer        = errors.New("room: the organizer's role cannot be changed, nor the organizer removed")
)

// SetRole gives id a meeting role. Reports whether it changed. The organizer
// is not changeable -- Teams offers no "Make an attendee" on the organizer --
// and only attendee, presenter and co-organizer can be given.
func (r *Room) SetRole(id string, role wire.Role) (bool, error) {
	if role != wire.RoleMember && role != wire.RoleModerator && role != wire.RoleHost {
		return false, ErrNotPermittedRole
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.participants[id]
	if !ok {
		return false, ErrNotFound
	}
	if id == r.hostID {
		return false, ErrOrganizer
	}
	if p.Role == role {
		return false, nil
	}
	p.Role = role
	p.State.Role = role
	// A presenter is trusted to speak: a disabled mic or camera goes.
	if role >= wire.RoleModerator {
		p.MicBlocked, p.CamBlocked = false, false
		delete(r.blockedAcct, p.UserID)
	}
	return true, nil
}

// Role reports id's role, and whether they are here.
func (r *Room) Role(id string) (wire.Role, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.participants[id]
	if !ok {
		return 0, false
	}
	return p.Role, true
}

// Removable reports whether a host may remove id: present, and not the
// organizer. (Removing yourself is leaving, which is a different button.)
func (r *Room) Removable(by, id string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.participants[id]; !ok {
		return ErrNotFound
	}
	if id == r.hostID || id == by {
		return ErrOrganizer
	}
	return nil
}

// LowerHands lowers every raised hand and returns whose, with their state as
// it now is, so the caller can tell them and everyone else.
func (r *Room) LowerHands() []wire.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []wire.State
	for _, p := range r.participants {
		if p.State.HandRaised {
			p.State.HandRaised = false
			s := p.State
			s.Participant = p.ID
			s.Role = p.Role
			out = append(out, s)
		}
	}
	return out
}

// HostSession reports whether this signed-in account hosts the meeting in
// progress -- as organizer or co-organizer, present now -- and when that
// meeting began. It answers "may this person download the attendance list",
// which Teams gives to organizers and co-organizers.
func (r *Room) HostSession(userID string) (time.Time, bool) {
	if userID == "" {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.participants {
		if p.UserID == userID && (p.ID == r.hostID || p.Role >= wire.RoleHost) {
			return r.sessionStart, true
		}
	}
	return time.Time{}, false
}

// ErrNotAttendee: "Disable mic" and "Disable camera" are for attendees only,
// as Teams offers them; a presenter is somebody trusted to speak.
var ErrNotAttendee = errors.New("room: only an attendee's mic or camera can be disabled")

// SetBlocked disables (or allows again) one attendee's mic and camera:
// "Disable mic" / "Allow mic", "Disable camera" / "Allow camera". Enforced
// in May, so nothing of theirs is forwarded while it holds. Remembered by
// account as well, so leaving and rejoining does not lift it. Reports
// whether anything changed.
func (r *Room) SetBlocked(id string, mic, cam bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.participants[id]
	if !ok {
		return false, ErrNotFound
	}
	if id == r.hostID || p.Role >= wire.RoleModerator {
		return false, ErrNotAttendee
	}
	if p.MicBlocked == mic && p.CamBlocked == cam {
		return false, nil
	}
	p.MicBlocked, p.CamBlocked = mic, cam
	if p.UserID != "" {
		if r.blockedAcct == nil {
			r.blockedAcct = map[string][2]bool{}
		}
		if mic || cam {
			r.blockedAcct[p.UserID] = [2]bool{mic, cam}
		} else {
			delete(r.blockedAcct, p.UserID)
		}
	}
	return true, nil
}

// Blocked is one person whose mic or camera a host has disabled.
type Blocked struct {
	ID       string
	Mic, Cam bool
}

// BlockedList is everybody currently disabled, for someone arriving.
func (r *Room) BlockedList() []Blocked {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Blocked
	for _, p := range r.participants {
		if p.MicBlocked || p.CamBlocked {
			out = append(out, Blocked{ID: p.ID, Mic: p.MicBlocked, Cam: p.CamBlocked})
		}
	}
	return out
}
