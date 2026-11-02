package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
)

// Actor is whoever is making a request. Kept as a small struct rather than
// depending on the identity package, so control does not need to know how
// people are authenticated.
type Actor struct {
	ID    string
	Email string
	Name  string
	Admin bool
}

// Notifier delivers session changes to the people involved, over the push
// socket. Injected rather than imported so this package does not depend on
// the transport.
type Notifier interface {
	NotifyControl(userID string, s *Session)
}

type Service struct {
	repo Repository
	rec  audit.Recorder
	log  *slog.Logger
	note Notifier
}

func NewService(repo Repository, rec audit.Recorder, log *slog.Logger) *Service {
	return &Service{repo: repo, rec: rec, log: log}
}

// SetNotifier installs the push path. Optional: the module works without it,
// clients simply have to poll.
func (s *Service) SetNotifier(n Notifier) { s.note = n }

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Request asks a participant for control of their machine.
//
// It creates a request and nothing else. No permission is conferred here by
// anyone, including an administrator: rule 1 says the only thing that can turn
// a request into a grant is the person at the machine deciding so. This
// function is therefore deliberately boring, and that is the security
// property.
func (s *Service) Request(ctx context.Context, actor Actor, meetingID string,
	subject Actor, scope Scope, ip, ua string) (*Session, error) {

	if !scope.valid() {
		return nil, ErrBadScope
	}
	if actor.ID == "" || subject.ID == "" {
		return nil, ErrForbidden
	}
	if actor.ID == subject.ID {
		return nil, ErrSelfControl
	}

	now := time.Now().UTC()

	// One live session per machine. Two operators driving the same cursor is
	// not a feature, and the second request would also make the audit trail
	// ambiguous about who did what.
	if live, err := s.repo.LiveForSubject(ctx, subject.ID, now); err == nil && live != nil {
		return nil, ErrAlreadyActive
	} else if err != nil && err != ErrNotFound {
		return nil, err
	}

	sess := &Session{
		ID:              newID(),
		MeetingID:       meetingID,
		ControllerID:    actor.ID,
		ControllerEmail: actor.Email,
		ControllerName:  actor.Name,
		SubjectID:       subject.ID,
		SubjectEmail:    subject.Email,
		SubjectName:     subject.Name,
		Scope:           scope,
		State:           StateRequested,
		RequestedAt:     now,
	}
	if err := s.repo.Create(ctx, sess); err != nil {
		return nil, err
	}

	s.record(ctx, actor, audit.ActionControlRequested, sess, audit.OutcomeSuccess, ip, ua,
		fmt.Sprintf("scope=%s subject=%s", scope, subject.Email))
	s.notify(sess)
	return sess, nil
}

// Grant is the only path from requested to granted, and only the subject may
// walk it.
//
// The check below is the load-bearing line of the entire feature. An
// administrator is explicitly refused here, and that is not an oversight: the
// safety of remote control rests on a human at the keyboard deciding, in the
// moment, that this specific person may act as them. A role that can grant on
// their behalf turns the product into a remote access tool with an
// authorisation server, which is a different and much more dangerous thing.
func (s *Service) Grant(ctx context.Context, actor Actor, sessionID string,
	d time.Duration, ip, ua string) (*Session, error) {

	sess, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if actor.ID != sess.SubjectID {
		// Denied is audited with the actor who tried, because an admin
		// attempting to grant themselves control of somebody's machine is
		// exactly the event a security review needs to see.
		s.record(ctx, actor, audit.ActionControlGranted, sess, audit.OutcomeDenied, ip, ua,
			"only the subject may grant control")
		return nil, ErrForbidden
	}
	if sess.State != StateRequested {
		return nil, ErrBadState
	}

	if d <= 0 {
		d = DefaultGrant
	}
	if d > MaxGrant {
		d = MaxGrant
	}
	now := time.Now().UTC()
	exp := now.Add(d)

	sess.State = StateGranted
	sess.DecidedAt = &now
	sess.ExpiresAt = &exp
	if err := s.repo.Update(ctx, sess); err != nil {
		return nil, err
	}

	s.record(ctx, actor, audit.ActionControlGranted, sess, audit.OutcomeSuccess, ip, ua,
		fmt.Sprintf("scope=%s controller=%s for=%s", sess.Scope, sess.ControllerEmail, d))
	s.notify(sess)
	return sess, nil
}

// Deny refuses a request. Only the subject decides.
func (s *Service) Deny(ctx context.Context, actor Actor, sessionID, reason, ip, ua string) (*Session, error) {
	sess, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if actor.ID != sess.SubjectID {
		return nil, ErrForbidden
	}
	if sess.State != StateRequested {
		return nil, ErrBadState
	}

	now := time.Now().UTC()
	sess.State = StateDenied
	sess.DecidedAt = &now
	sess.EndedAt = &now
	sess.EndedBy = actor.ID
	sess.Reason = reason
	if err := s.repo.Update(ctx, sess); err != nil {
		return nil, err
	}

	s.record(ctx, actor, audit.ActionControlDenied, sess, audit.OutcomeSuccess, ip, ua, reason)
	s.notify(sess)
	return sess, nil
}

// Revoke ends a live session immediately.
//
// Rule 3: this is unilateral, and it is the one operation whose availability
// matters more than anything else here. The subject may always revoke. The
// controller may stop early. An administrator may kill a session -- note the
// asymmetry with Grant, which admins cannot do: being able to STOP control is
// safe, being able to START it is not, and the two permissions are therefore
// deliberately not symmetrical.
//
// Nothing in this path consults the controller or the network.
func (s *Service) Revoke(ctx context.Context, actor Actor, sessionID, reason, ip, ua string) (*Session, error) {
	sess, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	permitted := actor.ID == sess.SubjectID ||
		actor.ID == sess.ControllerID ||
		actor.Admin
	if !permitted {
		s.record(ctx, actor, audit.ActionControlRevoked, sess, audit.OutcomeDenied, ip, ua,
			"not a party to this session")
		return nil, ErrForbidden
	}
	if sess.State.Terminal() {
		// Revoking something already over is not an error. The user pressing
		// the panic button twice must not see a failure.
		return sess, nil
	}

	now := time.Now().UTC()
	sess.State = StateRevoked
	sess.EndedAt = &now
	sess.EndedBy = actor.ID
	sess.Reason = reason
	if err := s.repo.Update(ctx, sess); err != nil {
		return nil, err
	}

	by := "controller"
	switch {
	case actor.ID == sess.SubjectID:
		by = "subject"
	case actor.Admin:
		by = "administrator"
	}
	s.record(ctx, actor, audit.ActionControlRevoked, sess, audit.OutcomeSuccess, ip, ua,
		fmt.Sprintf("ended by %s: %s", by, reason))
	s.notify(sess)
	return sess, nil
}

// Escalate asks to move from viewing to controlling.
//
// Rule 4: this does not modify the existing session. It ends it and opens a
// fresh request, so the subject sees and answers a new question rather than
// having a session they already agreed to silently acquire the ability to
// type on their machine.
func (s *Service) Escalate(ctx context.Context, actor Actor, sessionID, ip, ua string) (*Session, error) {
	old, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if actor.ID != old.ControllerID {
		return nil, ErrForbidden
	}
	if old.Scope != ScopeView || !old.Live(time.Now().UTC()) {
		return nil, ErrBadState
	}

	now := time.Now().UTC()
	old.State = StateEnded
	old.EndedAt = &now
	old.EndedBy = actor.ID
	old.Reason = "escalated to control"
	if err := s.repo.Update(ctx, old); err != nil {
		return nil, err
	}
	s.record(ctx, actor, audit.ActionControlEscalated, old, audit.OutcomeSuccess, ip, ua,
		"view session closed, fresh consent required for control")
	s.notify(old)

	return s.Request(ctx, actor, old.MeetingID,
		Actor{ID: old.SubjectID, Email: old.SubjectEmail, Name: old.SubjectName},
		ScopeControl, ip, ua)
}

// Extend renews a grant. Rule 2: renewal is another explicit act by the
// subject, never an automatic rollover.
func (s *Service) Extend(ctx context.Context, actor Actor, sessionID string,
	d time.Duration, ip, ua string) (*Session, error) {

	sess, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if actor.ID != sess.SubjectID {
		return nil, ErrForbidden
	}
	now := time.Now().UTC()
	if !sess.Live(now) {
		// An expired session is not extended back to life. That would let a
		// grant the user has already watched run out be resurrected without
		// them being asked again.
		return nil, ErrBadState
	}
	if d <= 0 || d > MaxGrant {
		d = DefaultGrant
	}
	exp := now.Add(d)
	sess.ExpiresAt = &exp
	if err := s.repo.Update(ctx, sess); err != nil {
		return nil, err
	}
	s.record(ctx, actor, audit.ActionControlExtended, sess, audit.OutcomeSuccess, ip, ua,
		fmt.Sprintf("extended by %s", d))
	s.notify(sess)
	return sess, nil
}

// ExpireDue marks every overdue grant expired and returns them.
//
// Expiry is recorded rather than merely happening. A session that ended
// because nobody stopped it is a different fact from one the user revoked,
// and an audit log that cannot tell them apart cannot answer the question
// that matters afterwards.
func (s *Service) ExpireDue(ctx context.Context, now time.Time) ([]*Session, error) {
	gone, err := s.repo.ExpireOverdue(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, sess := range gone {
		s.rec.Record(ctx, audit.Event{
			ActorID: sess.SubjectID, ActorEmail: sess.SubjectEmail,
			Action:  audit.ActionControlExpired,
			Subject: sess.ID,
			Outcome: audit.OutcomeSuccess,
			Detail: fmt.Sprintf("scope=%s controller=%s grant lapsed",
				sess.Scope, sess.ControllerEmail),
		})
		s.notify(sess)
	}
	return gone, nil
}

// ByID implements Reader.
func (s *Service) ByID(ctx context.Context, id string) (*Session, error) {
	return s.repo.ByID(ctx, id)
}

// LiveForSubject implements Reader.
func (s *Service) LiveForSubject(ctx context.Context, subjectID string, now time.Time) (*Session, error) {
	return s.repo.LiveForSubject(ctx, subjectID, now)
}

// PendingFor lists what this person is being asked to decide.
func (s *Service) PendingFor(ctx context.Context, subjectID string) ([]*Session, error) {
	return s.repo.PendingFor(ctx, subjectID)
}

// ForMeeting lists sessions in a meeting.
func (s *Service) ForMeeting(ctx context.Context, meetingID string, limit int) ([]*Session, error) {
	return s.repo.ForMeeting(ctx, meetingID, limit)
}

// MayInject answers the question the media path actually asks: is this
// controller allowed to send input to this machine right now?
//
// The server answers it for its own routing, but the agent answers it again
// locally before injecting anything, because rule 5 says the controlled
// machine is the authority. Two independent checks means a server that is
// wrong can fail to stop a session but cannot start one.
func (s *Service) MayInject(ctx context.Context, sessionID, controllerID, subjectID string,
	now time.Time) bool {

	sess, err := s.repo.ByID(ctx, sessionID)
	if err != nil {
		return false
	}
	return sess.Scope == ScopeControl &&
		sess.Live(now) &&
		sess.ControllerID == controllerID &&
		sess.SubjectID == subjectID
}

func (s *Service) record(ctx context.Context, actor Actor, a audit.Action,
	sess *Session, out audit.Outcome, ip, ua, detail string) {

	s.rec.Record(ctx, audit.Event{
		ActorID: actor.ID, ActorEmail: actor.Email,
		Action:    a,
		Subject:   sess.ID,
		Outcome:   out,
		IP:        ip,
		UserAgent: ua,
		Detail:    detail,
	})
}

// notify pushes to both parties. Both, always: a session change that only one
// side can see is how a user ends up believing control has stopped when it
// has not.
func (s *Service) notify(sess *Session) {
	if s.note == nil {
		return
	}
	s.note.NotifyControl(sess.SubjectID, sess)
	s.note.NotifyControl(sess.ControllerID, sess)
}
