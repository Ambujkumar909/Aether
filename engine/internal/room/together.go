package room

import (
	"errors"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// Together mode (Teams: "Together mode", "Change scene", "Start sharing
// Together Mode for everyone", "Only the organizer can change the scene when
// Together Mode is selected for everyone"). Applied for everyone by an
// organizer or co-organizer; while it is on, each person's client sends the
// outline of the person in its own picture, which is relayed to the others so
// they can seat everyone in one scene (signal/together.go).

const (
	TogetherScenes = 3 // auditorium, café, conference room
	MaskRate       = 12
	MaskBurst      = 12
)

var ErrTogetherDenied = errors.New("room: only the organizer can choose Together mode for everyone")
var ErrTogetherScene = errors.New("room: no such scene")

type TogetherState struct {
	On    bool
	Scene uint8
	By    string // display name of whoever applied it
}

type together struct {
	TogetherState
	buckets map[string]*capBucket
}

// SetTogether turns it on or off for everyone, or changes the scene.
// Reports whether anything changed.
func (r *Room) SetTogether(by string, on bool, scene uint8) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.participants[by]
	if !ok || (r.hostID != by && p.Role < wire.RoleHost) {
		return false, ErrTogetherDenied
	}
	if scene >= TogetherScenes {
		return false, ErrTogetherScene
	}
	if r.tog == nil {
		r.tog = &together{buckets: map[string]*capBucket{}}
	}
	t := r.tog
	if t.On == on && (!on || t.Scene == scene) {
		return false, nil
	}
	t.On, t.Scene, t.By = on, scene, ""
	if on {
		t.By = p.DisplayName
	} else {
		t.Scene = 0
	}
	return true, nil
}

// Together reports the state everyone is told.
func (r *Room) Together() TogetherState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.tog == nil {
		return TogetherState{}
	}
	return r.tog.TogetherState
}

// MaskAllowed: Together mode is on, the sender is here, and within their
// allowance of outlines a second.
func (r *Room) MaskAllowed(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tog == nil || !r.tog.On {
		return false
	}
	if _, ok := r.participants[id]; !ok {
		return false
	}
	b := r.tog.buckets[id]
	if b == nil {
		b = &capBucket{tokens: MaskBurst, at: now}
		r.tog.buckets[id] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * MaskRate
	if b.tokens > MaskBurst {
		b.tokens = MaskBurst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// LeaveTogether forgets a leaver's allowance.
func (r *Room) LeaveTogether(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tog != nil {
		delete(r.tog.buckets, id)
	}
}
