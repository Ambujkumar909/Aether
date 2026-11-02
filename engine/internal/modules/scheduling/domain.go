// Package scheduling owns meetings: when they are, who is invited, and
// whether they happened.
//
// A meeting here is a plan, not a session. The room it names may be empty,
// occupied, or long finished; this module has an opinion about the calendar
// and none at all about the media plane. That separation is what lets a
// meeting be rescheduled while it is running, and lets a room be joined by
// someone who was never invited to anything.
package scheduling

import (
	"context"
	"errors"
	"time"
)

// Status is where a meeting sits in its life.
type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusCancelled Status = "cancelled"
	StatusCompleted Status = "completed"
)

// Category colours the calendar. A closed set, because a colour picker with
// free-form labels produces a calendar nobody can scan at a glance.
type Category string

const (
	CatBusiness Category = "business"
	CatDesign   Category = "design"
	CatPersonal Category = "personal"
	CatExternal Category = "external"
	CatInternal Category = "internal"
)

func (c Category) Valid() bool {
	switch c {
	case CatBusiness, CatDesign, CatPersonal, CatExternal, CatInternal:
		return true
	}
	return false
}

// Response is an invitee's answer.
type Response string

const (
	RespPending  Response = "pending"
	RespAccepted Response = "accepted"
	RespDeclined Response = "declined"
)

// Invitee is one person asked to a meeting.
type Invitee struct {
	Email    string   `json:"email"`
	UserID   string   `json:"userId,omitempty"`
	Response Response `json:"response"`
	// CoOrganizer: Teams' "Choose co-organizers" -- an invitee who runs the
	// meeting with the organizer and is given that role on arrival.
	CoOrganizer bool `json:"coOrganizer,omitempty"`
}

// Meeting is a planned gathering.
type Meeting struct {
	ID          string    `json:"id"`
	Room        string    `json:"room"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Category    Category  `json:"category"`
	Status      Status    `json:"status"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`

	OrganizerID    string `json:"organizerId"`
	OrganizerEmail string `json:"organizerEmail"`

	// Admission decides who walks straight in and who has to be let in, and
	// it is fixed when the meeting is scheduled rather than when it starts.
	//
	// Scheduling time is the only moment the organiser is actually thinking
	// about who is invited. Asking again at the door, when they are late and
	// people are already waiting, produces one answer every time -- whichever
	// is fastest to click.
	Admission string `json:"admission"`

	Invitees []Invitee `json:"invitees"`

	// Live reports whether anybody is in the room at this moment. It is not
	// stored: a meeting's clock says when it was meant to run, and this says
	// whether it actually is. A meeting that overruns stays live past its end
	// time, and one everybody left is over before it.
	Live bool `json:"live"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Admission policies, mirroring internal/room. Duplicated as plain strings
// rather than imported so the scheduling module does not depend on the
// meeting transport, which is the module boundary this codebase keeps.
const (
	AdmissionOpen    = "open"
	AdmissionTrusted = "trusted"
	AdmissionKnock   = "knock"
)

// NormaliseAdmission maps anything unrecognised to the strictest policy.
//
// The direction matters more than the values. A typo, a value from an older
// client, a hand-written API call: every one of those must make the meeting
// stricter. Failing open here would mean a malformed request could publish a
// private meeting to anybody holding the link.
func NormaliseAdmission(a string) string {
	switch a {
	case AdmissionOpen, AdmissionTrusted, AdmissionKnock:
		return a
	default:
		return AdmissionKnock
	}
}

// DurationMinutes is what the interface displays beside a meeting.
func (m *Meeting) DurationMinutes() int {
	d := m.EndsAt.Sub(m.StartsAt)
	if d <= 0 {
		return 0
	}
	return int(d.Minutes())
}

// Filter narrows a meeting query.
type Filter struct {
	// Participant limits results to meetings this user organises or is
	// invited to. Empty means every meeting the caller may see.
	Participant string
	From        time.Time
	To          time.Time
	Status      Status
	Category    Category
	Limit       int
}

var (
	ErrNotFound     = errors.New("scheduling: meeting not found")
	ErrForbidden    = errors.New("scheduling: not permitted")
	ErrBadTimeRange = errors.New("scheduling: end must be after start")
	ErrBadTitle     = errors.New("scheduling: a title is required")
	ErrBadCategory  = errors.New("scheduling: unknown category")
	ErrTooLong      = errors.New("scheduling: a meeting may not exceed 24 hours")
	ErrTooFar       = errors.New("scheduling: cannot schedule more than two years ahead")
	// Co-organizers are chosen from the people invited, at most ten
	// (Microsoft's published limit for meetings).
	ErrCoOrganizer = errors.New("scheduling: co-organizers must be invited, and at most 10")
)

// MaxCoOrganizers is Microsoft's published limit for a meeting.
const MaxCoOrganizers = 10

// Repository is the persistence port.
type Repository interface {
	Create(ctx context.Context, m *Meeting) error
	Update(ctx context.Context, m *Meeting) error
	Delete(ctx context.Context, id string) error
	ByID(ctx context.Context, id string) (*Meeting, error)
	List(ctx context.Context, f Filter) ([]*Meeting, error)
	SetResponse(ctx context.Context, meetingID, email string, r Response) error
	// Conflicts returns meetings for a user that overlap a window, so the
	// interface can warn before double-booking somebody.
	Conflicts(ctx context.Context, userID string, from, to time.Time, exclude string) ([]*Meeting, error)
	// ByRoom finds the meeting behind a room link, for the join path.
	ByRoom(ctx context.Context, room string) (*Meeting, error)

	// IsInvited answers, in one query, whether this account is the organiser
	// of the meeting behind a room or on its invitee list. Narrower than
	// loading the meeting: the join path needs a yes or no, and must not have
	// the invitee list to leak.
	IsInvited(ctx context.Context, room, userID, email string) (bool, error)

	// CompleteRoom marks the meeting behind a room finished. Called when the
	// last person leaves, which is when a meeting is actually over.
	CompleteRoom(ctx context.Context, room string, at time.Time) error

	// Attendance: what actually happened, as opposed to what was scheduled.
	// See attendance.go.
	RecordJoin(ctx context.Context, userID, room, name string, at time.Time) (string, error)
	RecordLeave(ctx context.Context, id string, at time.Time) error
	History(ctx context.Context, userID string, limit int) ([]*Attendance, error)
	RoomAttendance(ctx context.Context, room string, since time.Time, limit int) ([]Visit, error)
	IsCoOrganizer(ctx context.Context, room, userID, email string) (bool, error)

	QnASettings(ctx context.Context, room string) (QnASettings, error)
	SetQnASettings(ctx context.Context, room string, s QnASettings) error
	countQuestions(ctx context.Context, room string) (int, error)
	InsertQuestion(ctx context.Context, room string, q *Question) error
	InsertReply(ctx context.Context, qid, authorID string, rp *Reply) error
	ToggleLike(ctx context.Context, qid, user string) (bool, error)
	SetQuestionFlags(ctx context.Context, room, qid string, status *string, answered, pinned *bool) error
	Questions(ctx context.Context, room, qid, viewer string) ([]*Question, error)

	NotesCreated(ctx context.Context, room string) (bool, error)
	CreateNotes(ctx context.Context, room, by string, at time.Time) error
	NoteItems(ctx context.Context, room, id string) ([]*NoteItem, error)
	AddNoteItem(ctx context.Context, room string, it *NoteItem) error
	EditNoteItem(ctx context.Context, room, id string, e NoteEdit, at time.Time) error
	DeleteNoteItem(ctx context.Context, room, id string, version int) error

	// The meeting transcript (transcript.go).
	AppendTranscriptLine(ctx context.Context, room string, session int64, userID, name, text string, at time.Time) (bool, error)
	LatestTranscriptSession(ctx context.Context, room string) (int64, error)
	TranscriptLines(ctx context.Context, room string, session int64) ([]TranscriptLine, error)
	Attended(ctx context.Context, room, userID string) (bool, error)
}

// Reader is the narrow interface other modules depend on.
//
// Both methods take an Actor. A read interface that let a caller fetch any
// meeting by id without saying who is asking would be an authorisation hole
// wearing the costume of a convenience.
type Reader interface {
	ByID(ctx context.Context, a Actor, id string) (*Meeting, error)
	List(ctx context.Context, a Actor, f Filter) ([]*Meeting, error)
}

// RoomPolicy is what the meeting transport needs, and deliberately no more.
//
// The gateway asks one question -- how should this room admit people, and who
// organised it -- and gets back two strings. Handing it the Reader instead
// would let a transport read the whole schedule, which is a wider door than
// the question needs.
type RoomPolicy interface {
	PolicyForRoom(ctx context.Context, room string) (admission, organizerID string, ok bool)
}
