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
	AllowMedia                     // socket media, in bytes: far above any real encoder
	numAllowances
)

// Rates a second, and bursts.
var allowanceRates = [numAllowances][2]float64{
	AllowReaction: {2, 4},
	AllowState:    {10, 20},
	AllowChat:     {5, 10},
	AllowSpeakers: {8, 16},
	AllowBoard:    {60, 120},
	// 25 Mbit/s sustained and a 6 MB burst: no camera or screen encoder
	// comes near it (a still camera at constant quality is 49-81 kbit/s, a
	// moving one a few hundred; a 4K screen's key frame a few hundred KB).
	// Not a quality ceiling (rule 3): a bound on one client multiplying
	// junk by the room's size.
	AllowMedia: {25e6 / 8, 6e6},
}

// Burst is the most of a kind that passes at once.
func (a Allowance) Burst() float64 { return allowanceRates[a][1] }

// Allowed: the sender is here and within their allowance of this kind; the
// frame costs one token.
func (r *Room) Allowed(id string, a Allowance, now time.Time) bool {
	return r.AllowedN(id, a, 1, now)
}

// AllowedN: the same, for something that costs n tokens (bytes, for media).
func (r *Room) AllowedN(id string, a Allowance, n float64, now time.Time) bool {
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
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}
