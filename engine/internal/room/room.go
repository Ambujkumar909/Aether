// Package room is the meeting domain: who is present, what they are
// publishing, and who is speaking.
package room

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/wire"
)

// Participant is one person in a meeting.
type Participant struct {
	ID          string
	DisplayName string
	Role        wire.Role
	JoinedAt    time.Time

	// joinSeq orders arrivals totally. JoinedAt cannot: on Windows two joins
	// in quick succession read the same clock, and a tie broken by map order
	// handed hosting to a random person (TestHostHandoverOnLeave, flaky).
	joinSeq uint64

	// UserID is the signed-in account behind this participant, empty for a
	// guest. It outlives the connection, which ID does not: a reload mints a
	// fresh participant ID and the same UserID.
	UserID string

	// State is the packed presence that rides in a 3 byte delta.
	State wire.State

	// Energy is the smoothed speech energy driving speaker ranking.
	Energy float64

	// Meter is this participant's byte ledger, a child of the room meter.
	Meter *ledger.Meter

	// Send delivers a control frame to this participant. It must not block;
	// the transport owns its own outbound queue, because a slow client must
	// never be able to stall the room.
	Send func(frame []byte)

	// Addr is the host part of the connection's remote address, for the
	// same-device advisory (see SameAddr). Never shown to anyone.
	Addr string

	// MicBlocked and CamBlocked: a host's "Disable mic" / "Disable camera"
	// on this attendee (roles.go SetBlocked).
	MicBlocked, CamBlocked bool
}

// SameAddr counts the other participants in the room connected from this
// address. On a LAN one address is one machine; two clients of one machine
// in one meeting is the classic self-echo (each plays the other's copy of
// your voice into your own microphone, and neither echo canceller has the
// other's output as a reference). Behind a shared NAT the count can name
// colleagues on other machines, so the client words it as a possibility.
func (r *Room) SameAddr(addr, exceptID string) int {
	if addr == "" {
		return 0
	}
	n := 0
	for _, p := range r.Snapshot() {
		if p.ID != exceptID && p.Addr == addr {
			n++
		}
	}
	return n
}

// Room is a meeting.
type Room struct {
	ID      string
	Created time.Time
	Meter   *ledger.Meter

	mu           sync.RWMutex
	participants map[string]*Participant
	speakers     []string

	// The waiting room. See lobby.go: somebody in here is deliberately NOT in
	// participants, so no roster names them and no broadcast reaches them.
	admission Admission
	locked    bool
	waiting   map[string]*Waiter

	// hostID is whoever opened the meeting. Somebody has to be able to admit
	// people, and a room where nobody can is a room nobody can join.
	hostID string

	// hostUserID is the ACCOUNT of whoever most recently hosted, which is
	// what survives a reconnect. This exists because of a locked meeting: the
	// host reloaded their tab, arrived with a new participant ID, and the
	// room -- knowing its host only by the old one -- turned them away from
	// their own meeting with "the host has locked this meeting". A lock has
	// to know who holds the key, and the key is the account, not the socket.
	hostUserID string

	// What non-hosts may do. See permissions.go: enforced where the action
	// happens, not merely reflected in the interface.
	perms Permissions

	// presenter is the one participant whose screen is forwarded. See
	// presenter.go.
	presenter string

	// joinSeq numbers arrivals (Participant.joinSeq).
	joinSeq uint64

	// spotlights is who the host has put on everybody's stage, in order. See
	// spotlight.go.
	spotlights []string

	// blockedAcct remembers "Disable mic / camera" by account, so a rejoin
	// does not lift it. See roles.go SetBlocked.
	blockedAcct map[string][2]bool

	// wb is the whiteboard, nil until someone starts one. See whiteboard.go.
	wb *whiteboard

	// Recording, transcription and captions (recording.go).
	rec *recording

	// breakouts is this meeting's breakout rooms, nil if none. See breakout.go.
	breakouts *Breakouts

	// sessionStart is when the room last went from empty to occupied: the
	// meeting as people experienced it, which is what an attendance list
	// covers. A room lingers after emptying, so its creation time is not it.
	sessionStart time.Time

	// epoch anchors every relative timestamp on the wire, so that no frame
	// ever has to carry an absolute millisecond clock.
	epoch time.Time
}

func New(id string, parent *ledger.Meter) *Room {
	now := time.Now()
	return &Room{
		ID: id, Created: now, epoch: now,
		participants: make(map[string]*Participant),
		waiting:      make(map[string]*Waiter),
		// Trusted by default: colleagues who signed in walk straight in, and
		// anybody who merely has the link has to be let in by a host. Opening
		// the door by default is how a link that leaks becomes an incident.
		admission: AdmissionTrusted,
		perms:     DefaultPermissions(),
		Meter:     parent.Child("room:" + id),
	}
}

var (
	ErrNotFound = errors.New("room: participant not found")
	ErrFull     = errors.New("room: at capacity")
)

// MaxParticipants is a hard limit per room instance. Beyond this a meeting
// should be fanned across multiple SFU nodes with a relay trunk between
// them, because a single forwarding node with N senders and N receivers is
// an O(N^2) problem and pretending otherwise is how meetings fall over at
// exactly the moment they matter.
const MaxParticipants = 500

// Join adds a participant and returns the roster snapshot to send them.
func (r *Room) Join(p *Participant) ([]*Participant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.participants) >= MaxParticipants {
		return nil, ErrFull
	}
	p.JoinedAt = time.Now()
	r.joinSeq++
	p.joinSeq = r.joinSeq
	if b, ok := r.blockedAcct[p.UserID]; ok && p.UserID != "" {
		p.MicBlocked, p.CamBlocked = b[0], b[1]
	}
	if len(r.participants) == 0 {
		r.sessionStart = p.JoinedAt
	}
	p.Meter = r.Meter.Child("participant:" + p.ID)

	// The first person through the door hosts, which is the only answer that
	// works for an ad-hoc meeting: a waiting room with no host is a door
	// nobody can open, and the alternative -- nobody hosts until an organiser
	// arrives -- strands everybody who is on time.
	//
	// A scheduled meeting overrides this by naming its organiser, which is
	// checked before this runs.
	if r.hostID == "" {
		r.hostID = p.ID
		p.Role = wire.RoleHost
		if p.UserID != "" {
			r.hostUserID = p.UserID
		}
	}
	r.participants[p.ID] = p

	out := make([]*Participant, 0, len(r.participants))
	for _, q := range r.participants {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].joinSeq < out[j].joinSeq })
	return out, nil
}

// HostID reports who is hosting, empty if the room is empty.
func (r *Room) HostID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hostID
}

// SetHost names the host explicitly, for a scheduled meeting whose organiser
// is known before anybody arrives.
func (r *Room) SetHost(id string) {
	r.mu.Lock()
	r.hostID = id
	if p, ok := r.participants[id]; ok {
		p.Role = wire.RoleHost
		if p.UserID != "" {
			r.hostUserID = p.UserID
		}
	}
	r.mu.Unlock()
}

// IsHostAccount reports whether this signed-in account is the meeting's host,
// whether or not it is currently connected. This is the question a locked
// door has to answer, and it is deliberately about the account: the
// participant ID changes on every connection, so a lock keyed on it would
// exclude the host the moment their network blinked.
//
// Guests (empty id) are never hosts by account. A guest who happens to be
// hosting an ad-hoc meeting keeps hosting while connected, and if they drop
// out of a locked meeting they are locked out like anybody else -- there is
// nothing durable to recognise them by.
func (r *Room) IsHostAccount(userID string) bool {
	if userID == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hostUserID == userID
}

// IsHost reports whether this participant may admit people and change policy.
func (r *Room) IsHost(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.hostID == id {
		return true
	}
	// A co-host, promoted by the host.
	if p, ok := r.participants[id]; ok {
		return p.Role >= wire.RoleHost
	}
	return false
}

// Leave removes a participant.
func (r *Room) Leave(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.participants[id]; !ok {
		return ErrNotFound
	}
	delete(r.participants, id)
	// A presenter who leaves is not presenting any more; otherwise nobody
	// else could share until the room emptied.
	if r.presenter == id {
		r.presenter = ""
	}
	// Nor spotlighted: a spotlight on somebody absent holds a slot of seven
	// and a stage nobody can fill.
	r.unspotlightLocked(id)

	// If the host left, somebody else has to become one, or nobody can admit
	// the next arrival and the meeting silently becomes unjoinable.
	if r.hostID == id {
		r.hostID = ""
		var next *Participant
		for _, q := range r.participants {
			if next == nil || q.joinSeq < next.joinSeq {
				next = q
			}
		}
		if next != nil {
			r.hostID = next.ID
			next.Role = wire.RoleHost
			// The handoff moves the account too, or the old host's account
			// would still hold the key to a meeting somebody else now runs.
			// Left alone if the successor is a guest: better a stale key
			// than none, since a locked meeting with no recognisable host
			// account can never be re-entered by anyone.
			if next.UserID != "" {
				r.hostUserID = next.UserID
			}
		}
	}
	return nil
}

// Get returns a participant.
func (r *Room) Get(id string) (*Participant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.participants[id]
	return p, ok
}

// Snapshot returns the roster.
func (r *Room) Snapshot() []*Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].joinSeq < out[j].joinSeq })
	return out
}

// Size reports the participant count.
func (r *Room) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.participants)
}

// Broadcast sends a frame to everyone except the excluded id.
//
// The frame is encoded once and the same bytes are handed to every
// transport. This matters more than it appears: encoding per recipient in a
// 200 person all-hands turns one 3 byte encode into 200, and the interning
// dictionaries would diverge because each connection has its own table.
// Frames that must be broadcast are therefore encoded without interning, and
// the cost of that decision is visible right here rather than buried.
func (r *Room) Broadcast(frame []byte, exclude string) {
	r.mu.RLock()
	targets := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID != exclude && p.Send != nil {
			targets = append(targets, p)
		}
	}
	r.mu.RUnlock()

	for _, p := range targets {
		p.Send(frame)
		p.Meter.CountSignal(len(frame), false, 1)
	}
}

// ---------------------------------------------------------------------------
// Speaker ranking
// ---------------------------------------------------------------------------

// EnergyDecay is the per-update smoothing applied to speech energy.
//
// Ranking on instantaneous energy produces a UI that flickers between
// whoever coughed most recently, and worse, drives the layer allocator to
// thrash spatial layers on every syllable. Smoothing is not cosmetic here;
// it is what stops the bandwidth allocator from oscillating.
const EnergyDecay = 0.82

// ObserveEnergy folds a new audio level into a participant's smoothed
// energy and returns the new speaker ranking if it changed.
func (r *Room) ObserveEnergy(id string, level float64) ([]wire.Speaker, bool) {
	r.mu.Lock()
	p, ok := r.participants[id]
	if !ok {
		r.mu.Unlock()
		return nil, false
	}
	p.Energy = EnergyDecay*p.Energy + (1-EnergyDecay)*level

	ranked := make([]*Participant, 0, len(r.participants))
	for _, q := range r.participants {
		ranked = append(ranked, q)
	}
	r.mu.Unlock()

	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Energy > ranked[j].Energy })

	// Only the top few matter, both for the UI and for layer arbitration.
	const topN = 4
	if len(ranked) > topN {
		ranked = ranked[:topN]
	}
	out := make([]wire.Speaker, 0, len(ranked))
	ids := make([]string, 0, len(ranked))
	for _, q := range ranked {
		// Below this floor a participant is not speaking, they are in a
		// room with a refrigerator. Including them in the ranking would
		// promote background noise to dominant speaker in a quiet meeting.
		if q.Energy < SpeakingFloor {
			continue
		}
		out = append(out, wire.Speaker{Participant: q.ID, Energy: q.Energy})
		ids = append(ids, q.ID)
	}

	r.mu.Lock()
	changed := !sameOrder(r.speakers, ids)
	r.speakers = ids
	r.mu.Unlock()
	return out, changed
}

// SpeakingFloor is the smoothed energy below which a participant is treated
// as silent.
const SpeakingFloor = 0.06

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SpeakerRank returns the ranking position of a participant, or -1.
func (r *Room) SpeakerRank(id string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i, s := range r.speakers {
		if s == id {
			return i
		}
	}
	return -1
}

// Epoch is the session time origin.
func (r *Room) Epoch() time.Time { return r.epoch }

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry owns all rooms on this node.
type Registry struct {
	mu    sync.RWMutex
	rooms map[string]*Room
	meter *ledger.Meter
}

func NewRegistry(m *ledger.Meter) *Registry {
	return &Registry{rooms: make(map[string]*Room), meter: m}
}

// GetOrCreate returns the named room, creating it on first use.
func (rg *Registry) GetOrCreate(id string) *Room {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if r, ok := rg.rooms[id]; ok {
		return r
	}
	r := New(id, rg.meter)
	rg.rooms[id] = r
	return r
}

// Get returns an existing room.
func (rg *Registry) Get(id string) (*Room, bool) {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	r, ok := rg.rooms[id]
	return r, ok
}

// Occupancy reports how many people are in a room right now, zero for a room
// that does not exist. This is the difference between a meeting that is over
// and one that is running late: a scheduled end time says when it was meant
// to finish, and this says whether anybody is still in it.
func (rg *Registry) Occupancy(id string) int {
	rg.mu.RLock()
	r, ok := rg.rooms[id]
	rg.mu.RUnlock()
	if !ok {
		return 0
	}
	return r.Size()
}

// Reap removes empty rooms older than minAge, returning how many went.
func (rg *Registry) Reap(minAge time.Duration) int {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	n := 0
	for id, r := range rg.rooms {
		// A meeting whose people are all in breakout rooms is not over: its
		// breakout state is what brings them back.
		if r.Size() == 0 && time.Since(r.Created) > minAge && !r.BreakoutsOpen() {
			delete(rg.rooms, id)
			n++
		}
	}
	return n
}

// All returns every room.
// LiveAccounts is who is in a room right now, by account id, and whether
// they are sharing their screen. Guests (no account) are not listed. One
// pass over the registry; the map is the automatic-status feed.
func (rg *Registry) LiveAccounts() map[string]bool {
	out := map[string]bool{}
	for _, r := range rg.All() {
		for _, p := range r.Snapshot() {
			if p.UserID == "" {
				continue
			}
			// Presenting anywhere wins over merely being in a call elsewhere.
			out[p.UserID] = out[p.UserID] || p.State.Sharing
		}
	}
	return out
}

func (rg *Registry) All() []*Room {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	out := make([]*Room, 0, len(rg.rooms))
	for _, r := range rg.rooms {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// HostSession is Room.HostSession for a room by id; false if no such room.
func (rg *Registry) HostSession(room, userID string) (time.Time, bool) {
	r, ok := rg.Get(room)
	if !ok {
		return time.Time{}, false
	}
	return r.HostSession(userID)
}

// Member reports whether this account is in the room's meeting now, the name
// they are using there, and whether they host it (organizer or co-organizer).
func (rg *Registry) Member(room, userID string) (name string, host, ok bool) {
	r, found := rg.Get(room)
	if !found || userID == "" {
		return "", false, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.participants {
		if p.UserID == userID {
			return p.DisplayName, p.ID == r.hostID || p.Role >= wire.RoleHost, true
		}
	}
	return "", false, false
}
