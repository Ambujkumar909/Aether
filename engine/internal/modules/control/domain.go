// Package control is the consent lifecycle for remote control.
//
// internal/rc holds the input codec and the injection safety rules; this
// package holds the question of whether injection is allowed at all. The
// six rules it enforces are stated in full in internal/rc/doc.go, and the
// ones that shape this code most are:
//
//  1. only the person at the machine can grant control, and no role can
//     grant it on their behalf
//  2. a grant expires on its own
//  3. revocation is unilateral and never depends on the controller
//  4. viewing and driving are different grants; escalation is a fresh consent
//  6. every transition is on the tamper-evident audit chain
package control

import (
	"context"
	"errors"
	"time"
)

// Scope is what a session is permitted to do. Rule 4: these are different
// grants, not levels of one grant, and moving between them needs fresh
// consent rather than an upgrade.
type Scope string

const (
	// ScopeView shows the screen and nothing else. No input is injected.
	ScopeView Scope = "view"
	// ScopeControl delivers keyboard and mouse.
	ScopeControl Scope = "control"
)

func (s Scope) valid() bool { return s == ScopeView || s == ScopeControl }

// State is where a session is in its life.
type State string

const (
	StateRequested State = "requested"
	StateGranted   State = "granted"
	StateDenied    State = "denied"
	StateRevoked   State = "revoked"
	StateExpired   State = "expired"
	StateEnded     State = "ended"
)

// Terminal reports whether a state can still change. Terminal sessions are
// kept rather than deleted: the audit record of who controlled whose machine
// and for how long is the point.
func (s State) Terminal() bool {
	switch s {
	case StateDenied, StateRevoked, StateExpired, StateEnded:
		return true
	}
	return false
}

// MaxGrant bounds a single grant regardless of what was asked for.
//
// Rule 2 says consent is bounded; this is the ceiling on how bounded. Thirty
// minutes is long enough for a real support call and short enough that a
// session forgotten about does not become standing access. Renewal is another
// explicit act by the same person, which is the point: the question "is this
// still alright?" gets asked again rather than assumed.
const MaxGrant = 30 * time.Minute

// DefaultGrant is what a request asks for when it does not say.
const DefaultGrant = 10 * time.Minute

// Session is one request to control one person's machine.
type Session struct {
	ID        string `json:"id"`
	MeetingID string `json:"meetingId"`

	// ControllerID is who wants to drive; SubjectID is whose machine it is.
	// Both identities are recorded on every audit event, because "who did
	// this" for remote control has two answers and both matter.
	ControllerID    string `json:"controllerId"`
	ControllerEmail string `json:"controllerEmail"`
	ControllerName  string `json:"controllerName"`
	SubjectID       string `json:"subjectId"`
	SubjectEmail    string `json:"subjectEmail"`
	SubjectName     string `json:"subjectName"`

	Scope Scope `json:"scope"`
	State State `json:"state"`

	RequestedAt time.Time  `json:"requestedAt"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`

	// EndedBy records which side stopped it, which is the difference between
	// "the operator finished" and "the user pulled the plug".
	EndedBy string `json:"endedBy,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Live reports whether this session may currently be used to inject.
func (s *Session) Live(now time.Time) bool {
	return s.State == StateGranted &&
		s.ExpiresAt != nil && now.Before(*s.ExpiresAt)
}

// Remaining reports how long the grant has left.
func (s *Session) Remaining(now time.Time) time.Duration {
	if s.ExpiresAt == nil {
		return 0
	}
	d := s.ExpiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

var (
	ErrNotFound      = errors.New("control: no such session")
	ErrForbidden     = errors.New("control: not permitted")
	ErrBadScope      = errors.New("control: unknown scope")
	ErrBadState      = errors.New("control: session is not in a state that allows this")
	ErrSelfControl   = errors.New("control: a participant cannot request control of themselves")
	ErrAlreadyActive = errors.New("control: this machine already has a live control session")
)

// Repository is the port.
type Repository interface {
	Create(ctx context.Context, s *Session) error
	Update(ctx context.Context, s *Session) error
	ByID(ctx context.Context, id string) (*Session, error)
	// LiveForSubject returns the granted, unexpired session for a machine,
	// which is at most one.
	LiveForSubject(ctx context.Context, subjectID string, now time.Time) (*Session, error)
	// PendingFor lists what a person is being asked to decide.
	PendingFor(ctx context.Context, subjectID string) ([]*Session, error)
	// ForMeeting lists everything in a meeting, for the audit view.
	ForMeeting(ctx context.Context, meetingID string, limit int) ([]*Session, error)
	// ExpireOverdue marks granted sessions past their deadline and returns
	// them, so the caller can audit and notify.
	ExpireOverdue(ctx context.Context, now time.Time) ([]*Session, error)
}

// Reader is the narrow interface other modules depend on.
type Reader interface {
	ByID(ctx context.Context, id string) (*Session, error)
	LiveForSubject(ctx context.Context, subjectID string, now time.Time) (*Session, error)
}
