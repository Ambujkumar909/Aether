package room

import "time"

// Allowances. Everything a participant sends that the server relays to the
// whole room is multiplied by the room's size, so each kind is held to a
// per-person rate. The browser's own pace is well inside each one; a client
// that ignores it is cut down here, before the fan-out.
//
// Found by the 50-person stress run (ARCHITECTURE 6br): one client's 500
// reactions became 500 frames to each of the other 49 people.
type Allowance int

const (
	AllowReaction Allowance = iota // the browser: one per 500 ms
	AllowState                     // mute, camera, hand, share: on a person's action
	AllowChat                      // meeting chat: on a person's action
	AllowSpeakers                  // speech energy: the browser reports at most 4 Hz
	AllowBoard                     // whiteboard: live previews at 20 Hz, plus strokes
	numAllowances
)

// Rates a second, and bursts.
var allowanceRates = [numAllowances][2]float64{
	AllowReaction: {2, 4},
	AllowState:    {10, 20},
	AllowChat:     {5, 10},
	AllowSpeakers: {8, 16},
	AllowBoard:    {60, 120},
}

// Burst is the most of a kind that passes at once.
func (a Allowance) Burst() float64 { return allowanceRates[a][1] }

// Allowed: the sender is here and within their allowance of this kind; the
// frame costs one token.
func (r *Room) Allowed(id string, a Allowance, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.participants[id]
	if !ok {
		return false
	}
	rate, burst := allowanceRates[a][0], allowanceRates[a][1]
	b := &p.allowances[a]
	if b.at.IsZero() {
		b.tokens, b.at = burst, now
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
