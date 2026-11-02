package room

import (
	"errors"
	"slices"
)

// Spotlight: a host puts somebody's video on everybody's stage.
//
// Pinning is one person's choice about their own screen; a spotlight is the
// host's choice about everyone's, which is why it is held here, by the
// server, rather than by each client. Teams' rules, read from its shipped
// package (docs/TEAMS_PARITY.md, "Package evidence"):
//
//   - only organisers (here: hosts and co-hosts) spotlight anyone;
//   - at most seven at once (numberOfAllowedSpotlights = 7), and past that
//     "No more people can be spotlighted";
//   - the person spotlighted may "Exit spotlight" themselves;
//   - a host may "Stop all spotlights".
//
// The list is always sent whole, never as a change, so a client that missed
// a frame is correct again on the next one (CLAUDE.md rule 5).

// MaxSpotlights is Teams' limit, and also what a stage can show legibly.
const MaxSpotlights = 7

var ErrSpotlightFull = errors.New("room: no more people can be spotlighted")

// Spotlight adds id to the spotlight. Reports whether the list changed.
func (r *Room) Spotlight(id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.participants[id]; !ok {
		return false, ErrNotFound
	}
	if slices.Contains(r.spotlights, id) {
		return false, nil
	}
	if len(r.spotlights) >= MaxSpotlights {
		return false, ErrSpotlightFull
	}
	r.spotlights = append(r.spotlights, id)
	return true, nil
}

// Unspotlight removes id. Reports whether it was there.
func (r *Room) Unspotlight(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unspotlightLocked(id)
}

func (r *Room) unspotlightLocked(id string) bool {
	i := slices.Index(r.spotlights, id)
	if i < 0 {
		return false
	}
	r.spotlights = slices.Delete(r.spotlights, i, i+1)
	return true
}

// ClearSpotlights stops every spotlight. Reports whether there were any.
func (r *Room) ClearSpotlights() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	had := len(r.spotlights) > 0
	r.spotlights = nil
	return had
}

// Spotlights returns who is spotlighted, in the order they were added.
func (r *Room) Spotlights() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.spotlights)
}

// IsSpotlit reports whether id is spotlighted.
func (r *Room) IsSpotlit(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Contains(r.spotlights, id)
}

// End finishes the meeting for everybody: "You'll end the meeting for
// everyone." Returns who was in it, so the caller can tell each of them and
// close their connections, and settles every knock as ended.
//
// The meeting-scoped state goes with it. An empty room lingers until the
// reaper takes it (two minutes), and somebody who rejoins the link inside
// that window is starting a new meeting -- Teams asks "Do you want to start a
// new one?" -- not walking into the old one still locked, still muted, with a
// stranger's spotlight on the stage.
func (r *Room) End() []string {
	r.mu.Lock()
	ids := make([]string, 0, len(r.participants))
	for id := range r.participants {
		ids = append(ids, id)
	}
	waiting := make([]*Waiter, 0, len(r.waiting))
	for _, w := range r.waiting {
		waiting = append(waiting, w)
	}
	r.waiting = make(map[string]*Waiter)
	r.locked = false
	r.spotlights = nil
	r.perms = DefaultPermissions()
	r.breakouts = nil
	r.blockedAcct = nil
	r.wb = nil
	r.mu.Unlock()

	for _, w := range waiting {
		w.resolve(WaitEnded)
	}
	return ids
}
