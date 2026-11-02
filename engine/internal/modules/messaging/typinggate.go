package messaging

import (
	"sync"
	"time"
)

// typingGate decides which typing signals are worth relaying. A client
// signals "typing" every few seconds while the text keeps changing and
// "stopped" when it empties, is sent, or goes quiet; the gate relays a
// "typing" at most once per typingRepeat per person per conversation and
// a "stopped" only when the person was typing. So a keystroke-per-frame
// client, or a hostile one, cannot make the audience pay for it, and the
// audience never hears a stop for somebody it never saw start.
//
// Bounded: entries lapse after typingForget and are swept when the table
// grows past typingSweepAt, so a busy day leaves nothing behind.
const (
	typingRepeat  = 2500 * time.Millisecond
	typingForget  = 30 * time.Second
	typingSweepAt = 4096
)

type typingKey struct{ user, conv string }

type typingState struct {
	on bool
	at time.Time
}

type typingGate struct {
	mu   sync.Mutex
	last map[typingKey]typingState
}

func newTypingGate() *typingGate { return &typingGate{last: make(map[typingKey]typingState)} }

// allow reports whether this signal should reach the audience.
func (g *typingGate) allow(userID, convID string, on bool, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.last) > typingSweepAt {
		for k, st := range g.last {
			if now.Sub(st.at) > typingForget {
				delete(g.last, k)
			}
		}
	}
	k := typingKey{userID, convID}
	st, had := g.last[k]
	if had && now.Sub(st.at) > typingForget {
		had = false
	}
	if !on {
		if !had || !st.on {
			return false
		}
		g.last[k] = typingState{on: false, at: now}
		return true
	}
	if had && st.on && now.Sub(st.at) < typingRepeat {
		return false
	}
	g.last[k] = typingState{on: true, at: now}
	return true
}

// typing reports whether the person is currently known to be typing there.
func (g *typingGate) typing(userID, convID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.last[typingKey{userID, convID}]
	return ok && st.on && now.Sub(st.at) <= typingForget
}
