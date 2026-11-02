// Package audit is the tamper-evident record of who did what.
//
// An audit log that can be edited without trace is a log, not an audit. The
// distinction matters precisely in the situation the log exists for: someone
// with access did something they should not have, and then had both motive
// and opportunity to remove the evidence. A plain table gives them a DELETE.
//
// So every event carries the hash of the event before it. Changing, deleting,
// or reordering any entry breaks the chain from that point forward, and the
// break is detectable by anyone with a copy of the head hash -- which the
// server publishes and which can be pinned, printed, or sent somewhere the
// attacker does not control.
//
// This does not make tampering impossible. It makes tampering LOUD, which is
// the achievable goal for a system whose database an administrator can reach.
package audit

import (
	"context"
	"time"
)

// Action is what happened. A closed vocabulary rather than free text, because
// a log you cannot filter reliably is a log nobody reads.
type Action string

const (
	ActionLoginSucceeded  Action = "auth.login.succeeded"
	ActionLoginFailed     Action = "auth.login.failed"
	ActionLogout          Action = "auth.logout"
	ActionSessionRevoked  Action = "auth.session.revoked"
	ActionAccountLocked   Action = "auth.account.locked"
	ActionPasswordChanged Action = "auth.password.changed"

	ActionSignUp        Action = "auth.signup"
	ActionSignUpBlocked Action = "auth.signup.blocked"

	ActionUserCreated     Action = "user.created"
	ActionUserUpdated     Action = "user.updated"
	ActionUserRoleChanged Action = "user.role.changed"
	ActionUserSuspended   Action = "user.suspended"
	ActionUserReactivated Action = "user.reactivated"
	ActionUserDeleted     Action = "user.deleted"

	ActionMeetingJoined   Action = "meeting.joined"
	ActionMeetingLeft     Action = "meeting.left"
	ActionRecordingStart  Action = "meeting.recording.started"
	ActionRecordingStop   Action = "meeting.recording.stopped"
	ActionTranscriptStart Action = "meeting.transcription.started"
	ActionTranscriptStop  Action = "meeting.transcription.stopped"

	ActionMeetingScheduled   Action = "meeting.scheduled"
	ActionMeetingRescheduled Action = "meeting.rescheduled"
	ActionMeetingCancelled   Action = "meeting.cancelled"
	ActionMeetingDeleted     Action = "meeting.deleted"
	ActionMeetingResponded   Action = "meeting.responded"

	// Remote control. Every transition is recorded, because this is the one
	// capability in the product whose abuse must be provable afterwards:
	// somebody acted on another person's machine, under their identity, with
	// their privileges.
	ActionControlRequested Action = "control.requested"
	ActionControlGranted   Action = "control.granted"
	ActionControlDenied    Action = "control.denied"
	ActionControlRevoked   Action = "control.revoked"
	ActionControlExpired   Action = "control.expired"
	ActionControlExtended  Action = "control.extended"
	ActionControlEscalated Action = "control.escalated"

	// Deployment policy. "Who turned remote control back on" is a question
	// somebody eventually asks, and "it has always been like that" is how an
	// incident gets written up badly.
	ActionSettingChanged Action = "settings.changed"

	ActionAuditExported Action = "audit.exported"
)

// Outcome is deliberately coarse. "Denied" and "failed" are different events
// to whoever is reading: one is the system working, the other is the system
// breaking.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeDenied  Outcome = "denied"
	OutcomeFailure Outcome = "failure"
)

// Event is one recorded action.
type Event struct {
	Seq        int64     `json:"seq"`
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	ActorID    string    `json:"actorId"`
	ActorEmail string    `json:"actorEmail"`
	Action     Action    `json:"action"`
	Subject    string    `json:"subject"`
	Outcome    Outcome   `json:"outcome"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"userAgent"`
	Detail     string    `json:"detail"`

	// PrevHash and Hash form the chain. They are exposed so that a verifier
	// outside this process can recompute them.
	PrevHash string `json:"prevHash"`
	Hash     string `json:"hash"`
}

// Filter narrows a query.
type Filter struct {
	ActorID string
	Action  Action
	Outcome Outcome
	Since   time.Time
	Until   time.Time
	Search  string
	Limit   int
	Before  int64 // seq cursor for pagination
}

// Repository is the port. A Postgres implementation would satisfy this
// without any change above it.
type Repository interface {
	// Append writes an event. Implementations must serialise appends, since
	// the chain depends on each event seeing the true previous hash.
	Append(ctx context.Context, e *Event) error
	List(ctx context.Context, f Filter) ([]Event, error)
	Head(ctx context.Context) (seq int64, hash string, err error)
	Count(ctx context.Context, f Filter) (int64, error)
	// Walk visits every event in sequence order, for verification.
	Walk(ctx context.Context, from int64, fn func(Event) error) error
}

// Reader is the narrow interface other modules depend on. Nothing outside
// this package may write an event except through Service.Record, and nothing
// may reach the repository at all.
type Reader interface {
	List(ctx context.Context, f Filter) ([]Event, error)
	Verify(ctx context.Context) (VerifyResult, error)
}

// Recorder is what other modules use to write. Kept separate from Reader so
// a module that only needs to record cannot also read the whole log.
type Recorder interface {
	Record(ctx context.Context, e Event)
}

// VerifyResult is the outcome of walking the chain.
type VerifyResult struct {
	OK        bool      `json:"ok"`
	Checked   int64     `json:"checked"`
	HeadSeq   int64     `json:"headSeq"`
	HeadHash  string    `json:"headHash"`
	BrokenAt  int64     `json:"brokenAt,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}
