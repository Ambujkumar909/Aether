package room

import "github.com/aether-rtc/aether/internal/wire"

// hostRole is the role at or above which somebody is exempt from these
// controls.
const hostRole = wire.RoleHost

// What attendees are allowed to do, decided per meeting by its host.
//
// These are ENFORCED, not merely reflected in the interface, and the
// distinction is the entire point of the file. Hiding the screen-share button
// stops the people who were never going to misuse it and nobody else: a
// participant who opens the console, or runs a modified client, or simply has
// an older build, is unaffected by a hidden button. A host who has turned
// sharing off has made a decision about what can happen in their meeting, and
// an interface-only control turns that decision into a suggestion.
//
// So every permission here is checked at the point the thing actually
// happens -- where a track is forwarded, where a chat frame is dispatched --
// and the client's copy exists only so the buttons can be greyed out rather
// than failing silently when pressed.
//
// Hosts are exempt from all of them. A control that locked the host out of
// their own meeting would be a way to end a meeting by accident.

// Permissions is what a non-host participant may do.
type Permissions struct {
	// Mic and Camera decide whether a participant's audio and video are
	// forwarded to anybody else. Enforced by refusing to forward, rather
	// than by asking the sender to stop, because asking is exactly what a
	// modified client ignores.
	Mic    bool
	Camera bool

	// Share decides whether a participant may present.
	Share bool

	// Chat decides whether a participant may send messages to the meeting.
	Chat bool

	// Reactions decides whether a participant may send live reactions.
	// Teams' "Allow reactions": a separate control from chat, so turning
	// the chat off no longer silences the applause.
	Reactions bool
}

// DefaultPermissions is what a meeting starts as when nothing says otherwise.
//
// Permissive, deliberately: the common meeting is a handful of colleagues who
// all need to talk, and a product that starts every one of those muted teaches
// people to reach for the controls before they have a reason to. The
// deployment can change these, and a host can tighten them the moment they
// need to.
func DefaultPermissions() Permissions {
	return Permissions{Mic: true, Camera: true, Share: true, Chat: true, Reactions: true}
}

// Permissions returns what non-hosts may currently do.
func (r *Room) Permissions() Permissions {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.perms
}

// SetPermissions replaces them. Only a host should reach this.
func (r *Room) SetPermissions(p Permissions) {
	r.mu.Lock()
	r.perms = p
	r.mu.Unlock()
}

// May reports whether a participant is allowed to do something.
//
// Takes the participant id rather than a Permissions value so the host
// exemption lives in one place. Every caller asking "may this person share"
// gets the same answer, including the part about hosts always being able to.
func (r *Room) May(id string, what string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// One presenter at a time applies to everyone, the host included: two
	// shares at once is a stage that cannot show both. See presenter.go.
	if what == "share" && r.presenter != "" && r.presenter != id {
		return false
	}
	// A host is never restricted by their own controls.
	if r.hostID == id {
		return true
	}
	// Nor is a presenter or a co-organizer: the controls are Teams' "for
	// attendees", and a presenter is somebody trusted to speak (roles.go).
	p, ok := r.participants[id]
	if ok && p.Role >= wire.RoleModerator {
		return true
	}
	// "Disable mic" / "Disable camera" on this one attendee.
	if ok && ((what == "mic" && p.MicBlocked) || (what == "camera" && p.CamBlocked)) {
		return false
	}

	switch what {
	case "mic":
		return r.perms.Mic
	case "camera":
		return r.perms.Camera
	case "share":
		return r.perms.Share
	case "chat":
		return r.perms.Chat
	case "reactions":
		return r.perms.Reactions
	}
	// An unrecognised capability is refused rather than allowed. A typo in a
	// caller must not open something up.
	return false
}

// MuteAll marks every non-host as muted and returns who was affected.
//
// A one-shot action rather than a permission: "mute everyone now" and
// "nobody may unmute" are different things a host wants at different moments,
// and collapsing them into one control means the host who wanted quiet for a
// second has to remember to give the microphones back.
//
// Turning Mic off as well is the host's separate choice.
func (r *Room) MuteAll() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var muted []string
	for id, p := range r.participants {
		if id == r.hostID || p.Role >= hostRole {
			continue
		}
		p.State.MicMuted = true
		muted = append(muted, id)
	}
	return muted
}
