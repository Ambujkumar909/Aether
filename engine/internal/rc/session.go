package rc

import (
	"sync"
	"time"
)

// Injector performs the platform-specific work. Everything above it is
// portable and testable; everything below it is one syscall per method.
type Injector interface {
	// MouseMove takes normalised coordinates in [0, CoordMax].
	MouseMove(x, y uint16) error
	MouseButton(b Button, down bool) error
	MouseWheel(dx, dy int16) error
	Key(vk uint16, down bool) error
	// Text types a literal string, layout-independently.
	Text(s string) error
}

// Grant is permission to inject, as this machine understands it.
//
// The controlled machine keeps its own copy rather than trusting the server's
// word for it, which is rule 5 of the consent model: the server brokers and
// audits, it does not authorise. A server that is compromised or simply buggy
// can then fail to stop a session, but cannot start one.
type Grant struct {
	// SessionID identifies the control session this grant belongs to. An
	// event carrying a different session is refused rather than ignored,
	// because a stale session injecting into a new one is how a revoked
	// operator keeps typing.
	SessionID string

	// ControllerID is who was granted control, recorded so the agent can
	// report accurately what it acted on.
	ControllerID string

	// Expires is when this grant stops being valid, with no further action
	// from anyone. Rule 2: consent is bounded in time. Renewal is a fresh
	// explicit act, not an automatic extension.
	Expires time.Time
}

// Session applies input on behalf of a grant, and is responsible for the one
// invariant that matters more than any other here: the machine is never left
// in a state the person sitting at it cannot get out of.
type Session struct {
	inj Injector

	mu      sync.Mutex
	grant   *Grant
	buttons [buttonCount]bool
	keys    map[uint16]bool
	ended   bool

	// paused is set when the local user takes over. Rule 3 says revocation
	// must be instant and uninterceptable; this is the softer, more common
	// case of the same instinct -- the person at the keyboard should always
	// win a fight over the cursor without having to end anything.
	paused bool

	Applied  int64
	Refused  int64
	Released int64
}

// NewSession prepares an injection session with no grant. It refuses
// everything until Grant is called, which is the safe default: a session that
// injects before consent has been recorded is the whole failure mode.
func NewSession(inj Injector) *Session {
	return &Session{inj: inj, keys: make(map[uint16]bool)}
}

// Grant installs or replaces permission to inject.
func (s *Session) Grant(g Grant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Replacing a grant while keys are held would strand them under the old
	// session, so hand them back first.
	if s.grant != nil && s.grant.SessionID != g.SessionID {
		s.releaseAllLocked()
	}
	gg := g
	s.grant = &gg
	s.ended = false
}

// Revoke ends injection immediately and releases everything held.
//
// This is the operation rule 3 is about, so it is written to be incapable of
// failing in any interesting way: it takes no arguments that could be wrong,
// consults nothing remote, and cannot be refused. A release that errors is
// retried across every other held input rather than aborting the loop,
// because a partial release is worse than none -- it leaves the user holding
// down a key they never pressed with no obvious way to notice.
func (s *Session) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grant = nil
	s.ended = true
	s.releaseAllLocked()
}

// SetPaused stops or resumes injection without ending the session. Used when
// the local user moves their own mouse or types.
func (s *Session) SetPaused(p bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p && !s.paused {
		// Let go of everything before standing aside, or the local user
		// inherits whatever the operator was holding.
		s.releaseAllLocked()
	}
	s.paused = p
}

// Paused reports whether injection is currently suspended.
func (s *Session) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Active reports whether this session may currently inject.
func (s *Session) Active(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked(now)
}

func (s *Session) activeLocked(now time.Time) bool {
	return !s.ended && !s.paused && s.grant != nil && now.Before(s.grant.Expires)
}

// Apply injects one event on behalf of sessionID.
//
// Every event is checked against the grant, not just the first. A grant that
// expires mid-drag must stop the drag, and it stops it by releasing what is
// held rather than by silently ceasing to move.
func (s *Session) Apply(sessionID string, e Event, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.grant == nil || s.grant.SessionID != sessionID {
		s.Refused++
		return ErrNotPermitted
	}
	if !now.Before(s.grant.Expires) {
		// Expiry is not a quiet stop. Anything held under the expired grant
		// is handed back before the session goes inert.
		s.releaseAllLocked()
		s.grant = nil
		s.ended = true
		s.Refused++
		return ErrNotPermitted
	}
	if s.ended || s.paused {
		s.Refused++
		return ErrNotPermitted
	}

	if err := s.applyLocked(e); err != nil {
		return err
	}
	s.Applied++
	return nil
}

func (s *Session) applyLocked(e Event) error {
	switch e.Kind {
	case KindMouseMove:
		return s.inj.MouseMove(e.X, e.Y)

	case KindMouseButton:
		if e.Button >= buttonCount {
			return ErrBadButton
		}
		// The button carries its own position, so move first and click
		// where the operator was actually pointing. Relying on an earlier
		// move having arrived is how a click lands somewhere else.
		if err := s.inj.MouseMove(e.X, e.Y); err != nil {
			return err
		}
		if err := s.inj.MouseButton(e.Button, e.Down); err != nil {
			return err
		}
		s.buttons[e.Button] = e.Down
		return nil

	case KindMouseWheel:
		if err := s.inj.MouseMove(e.X, e.Y); err != nil {
			return err
		}
		return s.inj.MouseWheel(e.WheelX, e.WheelY)

	case KindKey:
		if err := s.inj.Key(e.VK, e.Down); err != nil {
			return err
		}
		if e.Down {
			s.keys[e.VK] = true
		} else {
			delete(s.keys, e.VK)
		}
		return nil

	case KindText:
		return s.inj.Text(e.Text)

	case KindRelease:
		s.releaseAllLocked()
		return nil
	}
	return ErrUnknownKind
}

// releaseAllLocked hands back every held button and key.
//
// This is the single most important function in the package, and the reason
// is that its failure is invisible and unbounded. If a session ends while the
// operator is mid-drag or holding Alt -- because the network dropped, the
// grant expired, or the user revoked it -- the button or modifier stays
// physically down as far as the operating system is concerned. A stuck Alt
// makes the keyboard useless; a stuck mouse button turns every subsequent
// movement into a drag. The person at the machine has no way to know why, and
// the usual instinct, pressing and releasing the key themselves, is exactly
// what fixes it and exactly what they will not think to do.
//
// So every path that stops injection comes through here, errors are pushed
// past rather than returned, and it is safe to call repeatedly.
func (s *Session) releaseAllLocked() {
	for b := Button(0); b < buttonCount; b++ {
		if !s.buttons[b] {
			continue
		}
		// Errors are deliberately swallowed: a failure releasing one button
		// must not prevent the attempt on the next. Something stuck down is
		// the worst outcome available, so every release is attempted.
		_ = s.inj.MouseButton(b, false)
		s.buttons[b] = false
		s.Released++
	}
	for vk := range s.keys {
		_ = s.inj.Key(vk, false)
		delete(s.keys, vk)
		s.Released++
	}
}

// Held reports what is currently down, for telemetry and tests.
func (s *Session) Held() (buttons int, keys int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for b := Button(0); b < buttonCount; b++ {
		if s.buttons[b] {
			buttons++
		}
	}
	return buttons, len(s.keys)
}

// ApplyBatch applies a decoded burst, stopping at the first refusal.
//
// Stopping rather than skipping is deliberate. If a grant expires halfway
// through a burst, the remaining events are not "mostly fine to apply": they
// are input from someone who is no longer permitted to act on this machine.
func (s *Session) ApplyBatch(sessionID string, events []Event, now time.Time) (int, error) {
	for i, e := range events {
		if err := s.Apply(sessionID, e, now); err != nil {
			return i, err
		}
	}
	return len(events), nil
}
