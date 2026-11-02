package messaging

import (
	"context"
	"strings"
	"time"
)

// Availability, beside presence.
//
// The socket says whether somebody is connected; this is what they say
// about themselves -- busy, do not disturb, away -- with a note ("in a
// meeting until 3") and an optional expiry, after which they are simply
// available again. It goes to the same people presence goes to: everyone
// who shares a conversation with them, and nobody else. A stranger asking
// for a status gets nothing back, not "unknown": the answer would itself
// confirm the account.

const (
	StatusAvailable = "available"
	StatusBusy      = "busy"
	StatusDND       = "dnd"
	StatusAway      = "away"
	// Appear offline: connected, working, reading -- and shown to everyone
	// as offline, with no arrival, no departure and no last-seen time.
	StatusOffline = "offline"

	// Automatic only: never accepted from SetStatus, never offered to pick.
	StatusInCall     = "incall"
	StatusPresenting = "presenting"

	MaxStatusNote  = 120
	MaxStatusUntil = 7 * 24 * time.Hour
	MaxStatusQuery = 200
)

func validStatus(s string) bool {
	switch s {
	case StatusAvailable, StatusBusy, StatusDND, StatusAway, StatusOffline:
		return true
	}
	return false
}

// SetStatus records what a person says about themselves and tells their
// peers. Available with no note clears the row's meaning; it is still a
// row, so the change is pushed.
func (s *Service) SetStatus(ctx context.Context, a Actor, status, note string, until *time.Time) (Status, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	if !validStatus(status) {
		return Status{}, ErrBadStatus
	}
	note, ok := cleanLine(note, MaxStatusNote)
	if !ok {
		return Status{}, ErrBadStatus
	}
	now := time.Now().UTC()
	if until != nil {
		if until.Before(now) || until.Sub(now) > MaxStatusUntil {
			return Status{}, ErrBadStatus
		}
		u := until.UTC()
		until = &u
	}
	st := Status{UserID: a.ID, Status: status, Note: note, Until: until, UpdatedAt: now}
	if err := s.repo.SetStatus(ctx, st); err != nil {
		return Status{}, err
	}
	// Appearing offline is a presence fact as much as a status: peers are
	// told the person left (no last-seen time), and told they are back
	// when it is lifted. The hub's record makes Online() answer without a
	// query.
	wasHidden := s.hub.Hidden(a.ID)
	nowHidden := status == StatusOffline
	s.hub.SetHidden(a.ID, nowHidden)
	peers, perr := s.repo.PeersOf(ctx, a.ID)
	if perr == nil && wasHidden != nowHidden && s.hub.Online(a.ID) {
		for _, p := range peers {
			s.hub.PushPresence(p, a.ID, !nowHidden, time.Time{})
		}
	}
	// What peers see is the effective status: a manual pick while in a room
	// still reads in a call, as it does on the read path.
	eff, _ := effective(a.ID, st, true, s.autoNow(), now)
	s.presenceOf(ctx, a.ID, &eff)
	// The same audience as presence, for the same reason: bounded to the
	// people who can already see this person.
	if perr == nil {
		for _, p := range peers {
			s.hub.PushStatus(p, eff)
		}
	}
	s.hub.PushStatus(a.ID, eff) // their own other devices
	return eff, nil
}

// effective folds what the system knows into what a person said. Top first:
//
//   - appear offline, when picked: hides everything below, the rooms too;
//   - presenting, then in a call, when they are in a room right now: facts
//     about the person's screen and microphone, not preferences, so they
//     outrank anything picked;
//   - away, when every device of theirs has gone idle, unless they picked
//     something stronger than available (busy, do not disturb, away);
//   - whatever was picked (an expired pick reads available), or nothing.
//
// The calendar sets nothing: a scheduled meeting nobody has joined is not
// a call.
func effective(id string, manual Status, hasManual bool, auto map[string]string, now time.Time) (Status, bool) {
	if hasManual && manual.Until != nil && manual.Until.Before(now) {
		manual.Status, manual.Note, manual.Until = StatusAvailable, "", nil
	}
	if hasManual && manual.Status == StatusOffline {
		return manual, true
	}
	switch auto[id] {
	case StatusPresenting:
		return Status{UserID: id, Status: StatusPresenting, Note: "Presenting", UpdatedAt: now, Auto: true}, true
	case StatusInCall:
		return Status{UserID: id, Status: StatusInCall, Note: "In a call", UpdatedAt: now, Auto: true}, true
	case StatusAway:
		if !hasManual || manual.Status == StatusAvailable {
			return Status{UserID: id, Status: StatusAway, Note: manual.Note, UpdatedAt: now, Auto: true}, true
		}
	}
	return manual, hasManual
}

// autoNow is what the system knows this moment: the rooms first (in a
// room is in a call; sharing, presenting), then idleness (away) for
// everyone else who is connected. Nil when nothing is wired.
func (s *Service) autoNow() map[string]string {
	out := map[string]string{}
	if s.LiveNow != nil {
		for id, presenting := range s.LiveNow() {
			if presenting {
				out[id] = StatusPresenting
			} else {
				out[id] = StatusInCall
			}
		}
	}
	if s.hub != nil {
		for _, id := range s.hub.IdleUsers() {
			if _, have := out[id]; !have {
				out[id] = StatusAway
			}
		}
	}
	return out
}

// presenceOf adds presence to a status: online unless they appear offline;
// last seen when they are not online, or are away.
func (s *Service) presenceOf(ctx context.Context, id string, st *Status) {
	seen, _ := s.repo.LastSeenFor(ctx, []string{id})
	s.decorateWith(id, st, seen)
}

func (s *Service) decorateWith(id string, st *Status, seen map[string]time.Time) {
	st.Online = s.hub != nil && s.hub.Online(id) && st.Status != StatusOffline
	st.LastSeen = nil
	if st.Status == StatusOffline {
		return // theirs to hide
	}
	if !st.Online || st.Status == StatusAway {
		if t, ok := seen[id]; ok {
			st.LastSeen = &t
		}
	}
}

// Statuses returns the statuses of the given people that the asker is
// allowed to see: themselves, and anyone they share a conversation with.
// An expired status reads as available; a meeting in progress reads as
// busy. Bounded to MaxStatusQuery ids.
func (s *Service) Statuses(ctx context.Context, a Actor, ids []string) (map[string]Status, error) {
	if len(ids) > MaxStatusQuery {
		ids = ids[:MaxStatusQuery]
	}
	peers, err := s.repo.PeersOf(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{a.ID: true}
	for _, p := range peers {
		allowed[p] = true
	}
	var ask []string
	for _, id := range ids {
		if allowed[id] {
			ask = append(ask, id)
		}
	}
	got, err := s.repo.StatusesFor(ctx, ask)
	if err != nil {
		return nil, err
	}
	seen, err := s.repo.LastSeenFor(ctx, ask)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	auto := s.autoNow()
	out := make(map[string]Status, len(ask))
	for _, id := range ask {
		manual, has := got[id]
		st, ok := effective(id, manual, has, auto, now)
		if !ok {
			// Nothing picked, nothing automatic: available -- and still an
			// answer, because presence and last seen belong to everyone.
			st = Status{UserID: id, Status: StatusAvailable, UpdatedAt: now}
		}
		s.decorateWith(id, &st, seen)
		out[id] = st
	}
	return out, nil
}

// NudgeAutoStatus wakes the automatic-status loop now. The gateway calls
// it when somebody joins or leaves a room or starts or stops sharing, so
// "In a call" appears the moment the call connects and disappears the
// moment it drops. Never blocks.
func (s *Service) NudgeAutoStatus() {
	select {
	case s.nudgeCh() <- struct{}{}:
	default:
	}
}

func (s *Service) nudgeCh() chan struct{} {
	s.nudgeOnce.Do(func() { s.nudge = make(chan struct{}, 1) })
	return s.nudge
}

// RunAutoStatus tells peers when somebody enters or leaves a call, starts
// or stops presenting, or goes idle or wakes. It re-reads the rooms and the
// sockets on every nudge and, as a safety net, every tick; diffs against
// the last reading; and pushes the effective status for each person who
// changed -- to their peers and to their own devices, so their own dot
// moves too. Bounded by the number of people whose state changed since the
// last reading.
func (s *Service) RunAutoStatus(ctx context.Context, every time.Duration) {
	var prev map[string]string
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		now := time.Now().UTC()
		cur := s.autoNow()
		for _, id := range autoStatusChanges(prev, cur) {
			manual, has, _ := s.manualStatus(ctx, id)
			st, ok := effective(id, manual, has, cur, now)
			if !ok {
				// Nothing picked, no longer in a room: available, said out
				// loud so the dot that turned red turns back.
				st = Status{UserID: id, Status: StatusAvailable, UpdatedAt: now}
			}
			s.presenceOf(ctx, id, &st)
			if peers, err := s.repo.PeersOf(ctx, id); err == nil {
				for _, p := range peers {
					s.pushAuto(p, st)
				}
			}
			s.pushAuto(id, st)
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case <-s.nudgeCh():
		case <-t.C:
		}
	}
}

func (s *Service) pushAuto(to string, st Status) {
	if s.hub != nil {
		s.hub.PushStatus(to, st)
	}
	if s.autoPushed != nil {
		s.autoPushed(to, st)
	}
}

// autoStatusChanges lists the ids whose automatic state differs between
// two readings: entered or left a room, started or stopped presenting, went
// idle or woke. Pure.
func autoStatusChanges(prev, cur map[string]string) []string {
	var out []string
	for id, kind := range cur {
		if p, was := prev[id]; !was || p != kind {
			out = append(out, id)
		}
	}
	for id := range prev {
		if _, still := cur[id]; !still {
			out = append(out, id)
		}
	}
	return out
}

func (s *Service) manualStatus(ctx context.Context, id string) (Status, bool, error) {
	got, err := s.repo.StatusesFor(ctx, []string{id})
	if err != nil {
		return Status{}, false, err
	}
	st, ok := got[id]
	return st, ok, nil
}

// cleanLine is one line of human text: control characters out, runs of
// whitespace folded to a space, trimmed; and the bound refused rather than
// silently cut, so a caller one character over the line is told so.
// (cleanText, the search-query cleaner, truncates; a note or a poll option
// must not lose its tail without anybody knowing.)
func cleanLine(s string, max int) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		return "", false
	}
	return s, true
}
