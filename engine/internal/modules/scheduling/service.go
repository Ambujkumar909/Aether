package scheduling

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
)

// MaxDuration bounds a single meeting. Anything longer is a standing room,
// not a meeting, and treating it as one makes every calendar view useless.
const MaxDuration = 24 * time.Hour

// MaxHorizon is how far ahead a meeting may be scheduled. Two years is
// generous for a real plan and short enough that a typo in a year field is
// caught rather than silently accepted.
const MaxHorizon = 2 * 365 * 24 * time.Hour

// Service is the scheduling business logic.
type Service struct {
	// ClampAdmission holds a host's chosen policy to what the deployment
	// permits. Injected rather than imported so scheduling does not depend on
	// the settings module; nil means no ceiling, which is what a deployment
	// without the settings module has.
	//
	// Enforced here, at the point the meeting is written, rather than in the
	// console that offers the choice. A ceiling checked only by the interface
	// is not a ceiling: it is a suggestion that any client which skips the
	// interface ignores.
	ClampAdmission func(ctx context.Context, chosen string) string

	// LiveHost reports whether an account hosts the meeting in progress in a
	// room (organizer or co-organizer, present now) and when it began.
	// Injected from the meeting transport; nil means nobody may export an
	// attendance list.
	LiveHost func(room, userID string) (since time.Time, ok bool)

	// LiveMember reports whether an account is in the meeting in a room now,
	// and whether as its organizer or a co-organizer. Q&A asks it; nil means
	// nobody may use Q&A.
	LiveMember func(room, userID string) (name string, host, ok bool)

	// QnAPush sends a Q&A change to the people in a room's meeting: to all,
	// or, when restricted, to its hosts and one author. Injected from the
	// meeting transport.
	QnAPush func(room string, body []byte, restricted bool, authorID string)

	// NotesPush sends a meeting-notes change to everyone in a room's meeting.
	NotesPush func(room string, body []byte)

	// Occupancy reports how many people are in a room right now. Injected
	// from the meeting transport so this module does not import it; nil in a
	// deployment without one, and then no meeting is ever reported live.
	Occupancy func(room string) int

	repo  Repository
	audit audit.Recorder
	log   *slog.Logger
}

// InvitedToRoom reports whether this account holds an invitation to the
// meeting behind a room -- as organiser or as invitee.
//
// This is the whole point of an invitation. Somebody the organiser named
// walks in; the waiting room exists for everybody else, who has the link but
// was never asked. Unauthenticated by design, like PolicyForRoom: it is
// called on the join path before anybody is a participant, and it answers
// only about the account the transport has already verified.
func (s *Service) InvitedToRoom(ctx context.Context, room, userID, email string) bool {
	if room == "" || (userID == "" && email == "") {
		return false
	}
	ok, err := s.repo.IsInvited(ctx, room, userID, email)
	if err != nil {
		// Fail closed: an unreadable invitee list is not an invitation. The
		// person still gets in the normal way, by being let in.
		s.log.Warn("invite check failed", "room", room, "err", err)
		return false
	}
	return ok
}

// RoomEmptied is called when the last person leaves a meeting room.
//
// A meeting is over when everybody has gone, not when the clock says so. A
// scheduled hour that runs to ninety minutes is one meeting, not one meeting
// followed by a trespass; a scheduled hour everybody leaves after ten minutes
// is finished, and calling it "in progress" for the next fifty is a lie the
// calendar tells and the room can correct.
func (s *Service) RoomEmptied(ctx context.Context, room string) {
	if room == "" {
		return
	}
	if err := s.repo.CompleteRoom(ctx, room, time.Now().UTC()); err != nil {
		s.log.Warn("could not complete meeting", "room", room, "err", err)
	}
}

// markLive stamps the live flag onto meetings on their way out of the module.
func (s *Service) markLive(ms []*Meeting) {
	if s.Occupancy == nil {
		return
	}
	for _, m := range ms {
		if m != nil {
			m.Live = s.Occupancy(m.Room) > 0
		}
	}
}

func NewService(repo Repository, rec audit.Recorder, log *slog.Logger) *Service {
	return &Service{repo: repo, audit: rec, log: log}
}

// Actor is who is acting. Passed in rather than resolved here so this module
// need not import identity.
type Actor struct {
	ID    string
	Email string
	Admin bool
}

// Create schedules a meeting.
func (s *Service) Create(ctx context.Context, a Actor, m *Meeting, ip, ua string) (*Meeting, error) {
	if err := validate(m); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	m.ID = newID()
	m.OrganizerID = a.ID
	m.OrganizerEmail = a.Email
	m.Status = StatusScheduled
	// Held to whatever the deployment permits. A host who asks for a more
	// open meeting than policy allows gets the policy, not an error: the
	// request is not malicious, it is a client that does not know the local
	// rules, and applying them is more useful than refusing the meeting.
	m.Admission = NormaliseAdmission(m.Admission)
	if s.ClampAdmission != nil {
		m.Admission = s.ClampAdmission(ctx, m.Admission)
	}
	m.CreatedAt = now
	m.UpdatedAt = now
	if m.Room == "" {
		m.Room = roomFromTitle(m.Title)
	}
	if err := checkCoOrganizers(m.Invitees); err != nil {
		return nil, err
	}
	m.Invitees = normaliseInvitees(m.Invitees, a.Email)
	if m.Recurrence != nil {
		if err := m.Recurrence.check(m.StartsAt, m.EndsAt, now); err != nil {
			return nil, err
		}
	}

	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	m.firstOccurrence()
	detail := fmt.Sprintf("room %s at %s for %d min", m.Room, m.StartsAt.Format(time.RFC3339), m.DurationMinutes())
	if m.Recurrence != nil {
		detail += fmt.Sprintf(", %s every %d until %s", m.Recurrence.Freq, m.Recurrence.Interval, m.Recurrence.Until.Format("2006-01-02"))
	}
	s.record(ctx, a, "meeting.scheduled", m.Title, ip, ua, detail)
	return m, nil
}

// Update changes a meeting. Only the organiser or an administrator may.
func (s *Service) Update(ctx context.Context, a Actor, id string, patch *Meeting, ip, ua string) (*Meeting, error) {
	m, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.mayEdit(a, m) {
		return nil, ErrForbidden
	}

	was := m.StartsAt
	m.Title = patch.Title
	m.Description = patch.Description
	m.Category = patch.Category
	m.StartsAt = patch.StartsAt
	m.EndsAt = patch.EndsAt
	// The rule is replaced whole; the occurrences already cancelled stay
	// cancelled unless the caller says otherwise.
	if patch.Recurrence != nil && m.Recurrence != nil && patch.Recurrence.Skipped == nil {
		patch.Recurrence.Skipped = m.Recurrence.Skipped
	}
	m.Recurrence = patch.Recurrence
	if patch.Room != "" {
		m.Room = patch.Room
	}
	if patch.Invitees != nil {
		if err := checkCoOrganizers(patch.Invitees); err != nil {
			return nil, err
		}
		m.Invitees = normaliseInvitees(patch.Invitees, m.OrganizerEmail)
	}
	if err := validate(m); err != nil {
		return nil, err
	}
	if m.Recurrence != nil {
		if err := m.Recurrence.check(m.StartsAt, m.EndsAt, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	m.UpdatedAt = time.Now().UTC()

	if err := s.repo.Update(ctx, m); err != nil {
		return nil, err
	}

	detail := "details changed"
	if !was.Equal(m.StartsAt) {
		// A moved meeting resets everybody who had already answered: an
		// acceptance is for a time, not for a title.
		for i := range m.Invitees {
			m.Invitees[i].Response = RespPending
		}
		_ = s.repo.Update(ctx, m)
		detail = fmt.Sprintf("moved from %s to %s",
			was.Format(time.RFC3339), m.StartsAt.Format(time.RFC3339))
	}
	s.record(ctx, a, "meeting.rescheduled", m.Title, ip, ua, detail)
	return m, nil
}

// Cancel marks a meeting cancelled rather than deleting it, so that the
// people who had it in their calendar can still see what happened to it.
func (s *Service) Cancel(ctx context.Context, a Actor, id, ip, ua string) error {
	return s.CancelOccurrence(ctx, a, id, 0, ip, ua)
}

// CancelOccurrence cancels one occurrence of a series (occurrence is its
// start in Unix milliseconds), or the whole meeting when occurrence is 0.
// The occurrence must be a real one: a made-up time is refused rather than
// recorded, so the skipped list never grows with noise.
func (s *Service) CancelOccurrence(ctx context.Context, a Actor, id string, occurrence int64, ip, ua string) error {
	m, err := s.repo.ByID(ctx, id)
	if err != nil {
		return err
	}
	if !s.mayEdit(a, m) {
		return ErrForbidden
	}
	m.UpdatedAt = time.Now().UTC()
	if occurrence != 0 {
		at := time.UnixMilli(occurrence).UTC()
		if m.Recurrence == nil || !m.Recurrence.isOccurrence(m.StartsAt, at) {
			return ErrNotFound
		}
		for _, sk := range m.Recurrence.Skipped {
			if sk == occurrence {
				return nil // already cancelled; nothing to record twice
			}
		}
		if len(m.Recurrence.Skipped) >= maxSkipped {
			return fmt.Errorf("%w: skipped", ErrBadRecurrence)
		}
		m.Recurrence.Skipped = append(m.Recurrence.Skipped, occurrence)
		if err := s.repo.Update(ctx, m); err != nil {
			return err
		}
		s.record(ctx, a, "meeting.cancelled", m.Title, ip, ua, "occurrence at "+at.Format(time.RFC3339))
		return nil
	}
	m.Status = StatusCancelled
	if err := s.repo.Update(ctx, m); err != nil {
		return err
	}
	s.record(ctx, a, "meeting.cancelled", m.Title, ip, ua, "")
	return nil
}

// Delete removes a meeting entirely. Organiser or administrator only.
func (s *Service) Delete(ctx context.Context, a Actor, id, ip, ua string) error {
	m, err := s.repo.ByID(ctx, id)
	if err != nil {
		return err
	}
	if !s.mayEdit(a, m) {
		return ErrForbidden
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.record(ctx, a, "meeting.deleted", m.Title, ip, ua, "")
	return nil
}

// Respond records an invitee's answer.
func (s *Service) Respond(ctx context.Context, a Actor, id string, r Response, ip, ua string) (*Meeting, error) {
	if r != RespAccepted && r != RespDeclined && r != RespPending {
		return nil, fmt.Errorf("scheduling: unknown response %q", r)
	}
	m, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// You may answer only your own invitation.
	found := false
	for _, inv := range m.Invitees {
		if strings.EqualFold(inv.Email, a.Email) {
			found = true
			break
		}
	}
	if !found {
		return nil, ErrForbidden
	}
	if err := s.repo.SetResponse(ctx, id, a.Email, r); err != nil {
		return nil, err
	}
	s.record(ctx, a, "meeting.responded", m.Title, ip, ua, string(r))
	return s.repo.ByID(ctx, id)
}

// List returns meetings visible to the actor.
func (s *Service) List(ctx context.Context, a Actor, f Filter) ([]*Meeting, error) {
	// A non-administrator sees only what they organise or are invited to.
	// Enforced here rather than trusted from the query string.
	if !a.Admin {
		f.Participant = a.Email
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	ms, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	// A series becomes its occurrences inside the window asked for. With no
	// window, the next year of it: a calendar always asks with one, and
	// a caller that does not still gets something bounded.
	from, to := f.From, f.To
	if to.IsZero() {
		base := from
		if base.IsZero() {
			base = time.Now().UTC()
		}
		to = base.Add(365 * 24 * time.Hour)
	}
	ms = expand(ms, from, to)
	s.markLive(ms)
	return ms, nil
}

// ByID returns one meeting if the actor may see it.
func (s *Service) ByID(ctx context.Context, a Actor, id string) (*Meeting, error) {
	m, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	m.firstOccurrence()
	s.markLive([]*Meeting{m})
	if a.Admin || m.OrganizerID == a.ID {
		return m, nil
	}
	for _, inv := range m.Invitees {
		if strings.EqualFold(inv.Email, a.Email) {
			return m, nil
		}
	}
	return nil, ErrForbidden
}

// Conflicts reports the actor's own overlapping meetings.
func (s *Service) Conflicts(ctx context.Context, a Actor, from, to time.Time, exclude string) ([]*Meeting, error) {
	ms, err := s.repo.Conflicts(ctx, a.Email, from, to, exclude)
	if err != nil {
		return nil, err
	}
	return expand(ms, from, to), nil
}

// Scheduling-assistant bounds: how many people one question may ask about,
// and how wide a window. Enough for a meeting's invitee list and a fortnight.
const (
	MaxFreeBusyPeople = 20
	MaxFreeBusyWindow = 14 * 24 * time.Hour
)

// Busy is a span somebody is in a meeting, and only that: no title, no room,
// no one else who is there. Free/busy is what a colleague may know about a
// colleague's calendar; the rest is not theirs to see.
type Busy struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// FreeBusy answers, for each address, when they are busy inside the window.
// For members only (the handler guards), about exact addresses the caller
// named -- the same reach as the directory's exact-address lookup, and the
// answer is spans without any detail.
func (s *Service) FreeBusy(ctx context.Context, a Actor, emails []string, from, to time.Time) (map[string][]Busy, error) {
	if !to.After(from) || to.Sub(from) > MaxFreeBusyWindow || len(emails) > MaxFreeBusyPeople {
		return nil, ErrBadTimeRange
	}
	out := map[string][]Busy{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, done := out[e]; done {
			continue
		}
		ms, err := s.repo.Busy(ctx, e, from, to)
		if err != nil {
			return nil, err
		}
		spans := []Busy{}
		for _, m := range expand(ms, from, to) {
			// Declined is not busy.
			declined := false
			for _, inv := range m.Invitees {
				if strings.EqualFold(inv.Email, e) && inv.Response == RespDeclined {
					declined = true
				}
			}
			if declined {
				continue
			}
			spans = append(spans, Busy{From: m.StartsAt, To: m.EndsAt})
		}
		out[e] = spans
	}
	return out, nil
}

func (s *Service) mayEdit(a Actor, m *Meeting) bool {
	return a.Admin || m.OrganizerID == a.ID
}

func validate(m *Meeting) error {
	m.Title = strings.TrimSpace(m.Title)
	if m.Title == "" {
		return ErrBadTitle
	}
	if len(m.Title) > 200 {
		m.Title = m.Title[:200]
	}
	if m.Category == "" {
		m.Category = CatInternal
	}
	if !m.Category.Valid() {
		return ErrBadCategory
	}
	if !m.EndsAt.After(m.StartsAt) {
		return ErrBadTimeRange
	}
	if m.EndsAt.Sub(m.StartsAt) > MaxDuration {
		return ErrTooLong
	}
	if time.Until(m.StartsAt) > MaxHorizon {
		return ErrTooFar
	}
	m.StartsAt = m.StartsAt.UTC()
	m.EndsAt = m.EndsAt.UTC()
	return nil
}

// normaliseInvitees lowercases, de-duplicates, and guarantees the organiser
// is on their own invitation list.
func normaliseInvitees(in []Invitee, organiser string) []Invitee {
	seen := map[string]int{}
	var out []Invitee
	add := func(email string, r Response, co bool) {
		e := strings.ToLower(strings.TrimSpace(email))
		if e == "" {
			return
		}
		if i, ok := seen[e]; ok {
			out[i].CoOrganizer = out[i].CoOrganizer || co
			return
		}
		seen[e] = len(out)
		out = append(out, Invitee{Email: e, Response: r, CoOrganizer: co})
	}
	add(organiser, RespAccepted, false)
	for _, inv := range in {
		if inv.Response == respNotInvited {
			continue // checked by checkCoOrganizers before this runs
		}
		add(inv.Email, RespPending, inv.CoOrganizer)
	}
	// The organizer is the organizer, not also a co-organizer.
	if len(out) > 0 {
		out[0].CoOrganizer = false
	}
	return out
}

// respNotInvited marks a co-organizer named who is not on the invitation
// list; it never reaches storage.
const respNotInvited Response = "not-invited"

// checkCoOrganizers: each co-organizer is invited, and there are at most ten.
func checkCoOrganizers(in []Invitee) error {
	n := 0
	for _, inv := range in {
		if inv.Response == respNotInvited {
			return ErrCoOrganizer
		}
		if inv.CoOrganizer {
			n++
		}
	}
	if n > MaxCoOrganizers {
		return ErrCoOrganizer
	}
	return nil
}

// CoOrganizerOf reports whether an account is a co-organizer of the meeting
// scheduled in room. Fails closed.
func (s *Service) CoOrganizerOf(ctx context.Context, room, userID, email string) bool {
	if room == "" || (userID == "" && email == "") {
		return false
	}
	ok, err := s.repo.IsCoOrganizer(ctx, room, userID, email)
	if err != nil {
		s.log.Warn("co-organizer check failed", "room", room, "err", err)
		return false
	}
	return ok
}

// roomFromTitle derives a URL-friendly room name.
func roomFromTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "meeting"
	}
	// A short suffix keeps two meetings with the same title from colliding
	// into one room, which would silently merge two unrelated calls.
	return s + "-" + shortID()
}

func (s *Service) record(ctx context.Context, a Actor, action audit.Action, subject, ip, ua, detail string) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, audit.Event{
		Action: action, Outcome: audit.OutcomeSuccess,
		ActorID: a.ID, ActorEmail: a.Email, Subject: subject,
		IP: ip, UserAgent: ua, Detail: detail,
	})
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func shortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// PolicyForRoom answers, for the meeting transport, how a room should admit
// people and who organised it.
//
// Deliberately unauthenticated and deliberately narrow. It is called on the
// join path before anybody is a participant, so there is no actor to check
// against yet, and it therefore returns nothing that is not already implied
// by holding the room link: the admission policy, which the person is about
// to experience anyway, and the organiser's id, which decides who hosts.
// No title, no description, no invitee list.
// TitleForRoom is the scheduled meeting's title for a room, or "" for an
// ad-hoc room. For the meeting chat's name.
func (s *Service) TitleForRoom(ctx context.Context, room string) string {
	m, err := s.repo.ByRoom(ctx, room)
	if err != nil || m == nil {
		return ""
	}
	return m.Title
}

func (s *Service) PolicyForRoom(ctx context.Context, room string) (string, string, bool) {
	if room == "" {
		return "", "", false
	}
	m, err := s.repo.ByRoom(ctx, room)
	if err != nil || m == nil {
		return "", "", false
	}
	return NormaliseAdmission(m.Admission), m.OrganizerID, true
}
