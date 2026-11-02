package room

import (
	"errors"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// Recording, transcription and live captions (Teams: "Record and
// transcribe" > "Start recording" / "Start transcription", "Turn on live
// captions"; "Only meeting organizers and presenters can control
// transcription").
//
// The recording is made on the computer of whoever starts it -- Teams'
// "{{initiatorName}} owns the recording. It'll be saved to their computer" --
// so it stops when they leave. The transcript is made by each person's own
// browser from their own microphone and kept by the server; it outlives
// whoever started it. The room holds who started what, since when, and who
// wants captions, so that speakers know whether to turn speech into text.

var ErrRecDenied = errors.New("room: only organizers and presenters can record or transcribe")

// Caption frames per person: a sustained rate and a burst. Speech yields a
// final line every few seconds and a partial a few times a second; anything
// faster is not speech.
const (
	CaptionRate  = 6 // frames a second
	CaptionBurst = 12
)

// RecState is what everyone is told.
type RecState struct {
	Recording    bool
	RecBy        string // display name of whoever started it
	RecSince     time.Time
	Transcribing bool
	TrBy         string
	TrSince      time.Time
	Captions     bool // somebody has live captions on
}

type recording struct {
	RecState
	recByID  string
	captions map[string]bool       // who has captions on
	buckets  map[string]*capBucket // caption frames, per person
}

type capBucket struct {
	tokens float64
	at     time.Time
}

func (r *Room) recLocked() *recording {
	if r.rec == nil {
		r.rec = &recording{captions: map[string]bool{}, buckets: map[string]*capBucket{}}
	}
	return r.rec
}

func (r *Room) mayRecordLocked(id string) bool {
	if r.hostID == id {
		return true
	}
	p, ok := r.participants[id]
	return ok && p.Role >= wire.RoleModerator
}

// SetRecording starts or stops the recording. Organizers and presenters
// only; starting a running recording, or stopping a stopped one, changes
// nothing.
func (r *Room) SetRecording(by string, on bool, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.mayRecordLocked(by) {
		return false, ErrRecDenied
	}
	s := r.recLocked()
	if s.Recording == on {
		return false, nil
	}
	s.Recording, s.RecSince, s.RecBy, s.recByID = on, time.Time{}, "", ""
	if on {
		s.RecSince, s.recByID = now, by
		if p, ok := r.participants[by]; ok {
			s.RecBy = p.DisplayName
		}
	}
	return true, nil
}

// SetTranscribing starts or stops the transcript.
func (r *Room) SetTranscribing(by string, on bool, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.mayRecordLocked(by) {
		return false, ErrRecDenied
	}
	s := r.recLocked()
	if s.Transcribing == on {
		return false, nil
	}
	s.Transcribing, s.TrSince, s.TrBy = on, time.Time{}, ""
	if on {
		s.TrSince = now
		if p, ok := r.participants[by]; ok {
			s.TrBy = p.DisplayName
		}
	}
	return true, nil
}

// WantCaptions turns one person's captions on or off. Anyone in the
// meeting may. Reports whether "somebody wants captions" changed.
func (r *Room) WantCaptions(id string, on bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.participants[id]; !ok {
		return false
	}
	s := r.recLocked()
	before := len(s.captions) > 0
	if on {
		s.captions[id] = true
	} else {
		delete(s.captions, id)
	}
	s.Captions = len(s.captions) > 0
	return s.Captions != before
}

// CaptionsOn reports whether this person has captions on.
func (r *Room) CaptionsOn(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rec != nil && r.rec.captions[id]
}

// CaptionAllowed spends one caption frame from this person's allowance.
func (r *Room) CaptionAllowed(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.participants[id]; !ok {
		return false
	}
	s := r.recLocked()
	b := s.buckets[id]
	if b == nil {
		b = &capBucket{tokens: CaptionBurst, at: now}
		s.buckets[id] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * CaptionRate
	if b.tokens > CaptionBurst {
		b.tokens = CaptionBurst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// LeaveRecording forgets someone who left: their captions, their allowance,
// and the recording if it was theirs -- it was being made on their computer.
// Reports whether what everyone is told changed.
func (r *Room) LeaveRecording(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil {
		return false
	}
	s := r.rec
	delete(s.buckets, id)
	changed := false
	if s.captions[id] {
		delete(s.captions, id)
		if len(s.captions) == 0 {
			s.Captions, changed = false, true
		}
	}
	if s.Recording && s.recByID == id {
		s.Recording, s.RecSince, s.RecBy, s.recByID = false, time.Time{}, "", ""
		changed = true
	}
	return changed
}

// Recording reports the state everyone is told.
func (r *Room) Recording() RecState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.rec == nil {
		return RecState{}
	}
	return r.rec.RecState
}
