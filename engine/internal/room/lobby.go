package room

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// The waiting room.
//
// The whole value of a lobby is that somebody who has not been admitted is
// NOT IN THE MEETING, in every sense the system understands. It is worth being
// explicit about what that means here, because a waiting room that merely hides
// a tile is not a security feature at all -- it is a label, and every real
// breach of one has come from a participant who was in the room while the
// interface said they were waiting.
//
// So a waiter, until admitted, has:
//
//   - no entry in the participant map, so no roster frame names them and no
//     broadcast reaches them
//   - no media peer, so no SDP is exchanged and there is nothing to forward to
//     them even by mistake
//   - no chat, no presence, no speaker reports
//
// The only thing they can do is wait, and leave. That is enforced by where the
// gate sits -- before Join and before the media plane exists -- rather than by
// filtering afterwards, because filtering afterwards means every future frame
// type has to remember to filter too.

// Admission decides who has to wait.
type Admission string

const (
	// AdmissionOpen lets anybody with the link straight in. Convenient, and
	// the correct default only for meetings whose link is not sensitive.
	AdmissionOpen Admission = "open"

	// AdmissionKnock makes everybody wait for a host, including signed-in
	// colleagues. The strictest setting and the right one for anything where
	// the guest list matters.
	AdmissionKnock Admission = "knock"

	// AdmissionTrusted lets signed-in members of the organisation in directly
	// and makes everybody else wait. This is the setting most meetings want:
	// it removes the friction for the people who were invited without opening
	// the door to whoever has the link.
	AdmissionTrusted Admission = "trusted"
)

// Valid reports whether a policy is one this system understands. An unknown
// policy is treated as the strictest rather than the most permissive, which is
// the only safe direction for a parsing mistake to fail in.
func (a Admission) Valid() bool {
	switch a {
	case AdmissionOpen, AdmissionKnock, AdmissionTrusted:
		return true
	}
	return false
}

// Normalise turns anything unrecognised into the strictest policy.
func (a Admission) Normalise() Admission {
	if a.Valid() {
		return a
	}
	return AdmissionKnock
}

// WaitState is where a knock has got to.
type WaitState string

const (
	WaitPending  WaitState = "pending"
	WaitAdmitted WaitState = "admitted"
	WaitDenied   WaitState = "denied"
	WaitExpired  WaitState = "expired"
	WaitEnded    WaitState = "ended" // the host ended the meeting while they waited
)

// LobbyTimeout bounds how long somebody waits before being told nobody
// answered.
//
// A waiter holds a connection, so an unbounded lobby is an unbounded queue,
// which is a resource an unauthenticated party can consume. Five minutes is
// longer than anybody waits for a colleague and short enough that abandoned
// knocks clear themselves.
const LobbyTimeout = 5 * time.Minute

// MaxWaiting bounds the queue itself. Beyond this, knocks are refused
// outright rather than buffered: a lobby that grows without limit is a way to
// exhaust the server from outside the meeting.
const MaxWaiting = 200

var (
	ErrLocked       = errors.New("room: the meeting is locked")
	ErrLobbyFull    = errors.New("room: too many people are already waiting")
	ErrNoSuchWait   = errors.New("room: nobody is waiting under that id")
	ErrNotDecided   = errors.New("room: still waiting")
	ErrNotPermitted = errors.New("room: only a host may admit")
)

// Waiter is one person knocking.
type Waiter struct {
	ID          string
	DisplayName string
	// Authenticated records whether they signed in, which is what
	// AdmissionTrusted turns on.
	Authenticated bool
	UserID        string
	KnockedAt     time.Time

	mu       sync.Mutex
	state    WaitState
	decision chan WaitState
	decided  bool
}

// State reports where this knock has got to.
func (w *Waiter) State() WaitState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// Wait blocks until a decision arrives, the deadline passes, or done closes.
func (w *Waiter) Wait(done <-chan struct{}, timeout time.Duration) WaitState {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s := <-w.decision:
		return s
	case <-t.C:
		w.resolve(WaitExpired)
		return WaitExpired
	case <-done:
		w.resolve(WaitDenied)
		return WaitDenied
	}
}

// resolve settles a waiter exactly once.
//
// Once, because two hosts can click Admit and Deny at the same instant, and a
// second send on the channel would block forever holding whichever goroutine
// made it. The first decision wins and the rest are no-ops.
func (w *Waiter) resolve(s WaitState) bool {
	w.mu.Lock()
	if w.decided {
		w.mu.Unlock()
		return false
	}
	w.decided = true
	w.state = s
	w.mu.Unlock()

	select {
	case w.decision <- s:
	default:
		// Nobody is listening any more: the waiter gave up and disconnected.
		// Their state is still recorded so the host sees an accurate list.
	}
	return true
}

// Policy returns the room's admission policy.
func (r *Room) Policy() Admission {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.admission == "" {
		return AdmissionTrusted
	}
	return r.admission
}

// SetPolicy changes who has to wait. Only a host should reach this.
func (r *Room) SetPolicy(a Admission) {
	r.mu.Lock()
	r.admission = a.Normalise()
	r.mu.Unlock()
}

// Locked reports whether the meeting refuses all new arrivals.
func (r *Room) Locked() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.locked
}

// SetLocked closes or reopens the meeting to newcomers.
func (r *Room) SetLocked(v bool) {
	r.mu.Lock()
	r.locked = v
	r.mu.Unlock()
}

// MustWait decides whether this arrival has to knock.
//
// Deliberately a pure function of the policy and the arrival, with no
// side effects, so it can be exhaustively tested against every combination.
func (r *Room) MustWait(authenticated bool) bool {
	switch r.Policy() {
	case AdmissionOpen:
		return false
	case AdmissionTrusted:
		return !authenticated
	default:
		// AdmissionKnock, and anything unrecognised.
		return true
	}
}

// Knock registers somebody waiting for admission.
func (r *Room) Knock(w *Waiter) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.locked {
		return ErrLocked
	}
	if len(r.waiting) >= MaxWaiting {
		return ErrLobbyFull
	}
	if r.waiting == nil {
		r.waiting = make(map[string]*Waiter)
	}
	w.state = WaitPending
	w.decision = make(chan WaitState, 1)
	w.KnockedAt = time.Now()
	r.waiting[w.ID] = w
	return nil
}

// Waiting lists everybody currently knocking, oldest first.
func (r *Room) Waiting() []*Waiter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Waiter, 0, len(r.waiting))
	for _, w := range r.waiting {
		if w.State() == WaitPending {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KnockedAt.Before(out[j].KnockedAt) })
	return out
}

// Decide admits or denies one waiter. Reports whether it changed anything, so
// a duplicate click is distinguishable from a decision that landed.
func (r *Room) Decide(id string, admit bool) (bool, error) {
	r.mu.Lock()
	w, ok := r.waiting[id]
	if ok {
		delete(r.waiting, id)
	}
	r.mu.Unlock()

	if !ok {
		return false, ErrNoSuchWait
	}
	s := WaitDenied
	if admit {
		s = WaitAdmitted
	}
	return w.resolve(s), nil
}

// DecideAll admits or denies everybody currently waiting, and reports how many
// it actually settled.
func (r *Room) DecideAll(admit bool) int {
	r.mu.Lock()
	all := make([]*Waiter, 0, len(r.waiting))
	for _, w := range r.waiting {
		all = append(all, w)
	}
	r.waiting = make(map[string]*Waiter)
	r.mu.Unlock()

	s := WaitDenied
	if admit {
		s = WaitAdmitted
	}
	n := 0
	for _, w := range all {
		if w.resolve(s) {
			n++
		}
	}
	return n
}

// Withdraw removes a waiter who gave up, so the host stops being shown
// somebody who is no longer there.
func (r *Room) Withdraw(id string) {
	r.mu.Lock()
	w, ok := r.waiting[id]
	if ok {
		delete(r.waiting, id)
	}
	r.mu.Unlock()
	if ok {
		w.resolve(WaitDenied)
	}
}

// SweepLobby settles anybody who has waited past the deadline, and returns
// them so the caller can tell the hosts.
func (r *Room) SweepLobby(now time.Time, timeout time.Duration) []*Waiter {
	r.mu.Lock()
	var stale []*Waiter
	for id, w := range r.waiting {
		if now.Sub(w.KnockedAt) >= timeout {
			stale = append(stale, w)
			delete(r.waiting, id)
		}
	}
	r.mu.Unlock()

	for _, w := range stale {
		w.resolve(WaitExpired)
	}
	return stale
}

// WaitingCount reports the queue depth, for telemetry.
func (r *Room) WaitingCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, w := range r.waiting {
		if w.State() == WaitPending {
			n++
		}
	}
	return n
}
