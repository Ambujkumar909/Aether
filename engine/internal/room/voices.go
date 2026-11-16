package room

import "time"

// Which voices are relayed on the socket media path.
//
// Audio over the socket used to go from every unmuted microphone to
// everyone: with a hundred microphones open each person was sent a hundred
// streams, 3.2 Mbit/s, of which a handful were anybody speaking
// (ARCHITECTURE 6at, the ceiling row). The WebRTC path has given a receiver
// six voices from the start (sfu.MaxAudioSlots); this is the socket path's
// equivalent, and it is decided per frame from the level the sender put in
// the frame's spare bits, so a new speaker is heard from the frame they
// became loud in rather than from the next energy report, a quarter of a
// second later.
//
// The heard are at most MaxVoices people. While no more than that are
// sending, everyone sending is heard and nothing here ever drops a frame:
// a meeting with six microphones open behaves exactly as before. Past that,
// a place goes to whoever is loud now, taken from the member who has been
// quiet the longest, and only if that member has been quiet for voiceHold
// (the gap between two words is not the end of a turn). A member who sends
// nothing for voiceGone has muted or left, and their place is free.
//
// One frame is kept for each person not heard: the frame before the one
// that gets them a place is the start of their word, and it is relayed
// first. Bounded at one frame a person, and it goes with the person.
const (
	// MaxVoices is the most microphones relayed at once.
	MaxVoices = 6
	// VoiceLoud is the level, 0 to 63, at and above which a frame is
	// speech: the client's own measure (wsmedia.js frameLevel), on which
	// SpeakingFloor is 4.
	VoiceLoud = 4

	voiceHold = 1500 * time.Millisecond
	voiceGone = 2 * time.Second
	// voiceBefore is how old a kept frame may be and still be the start of
	// the word: the one frame before, with room for a late timer.
	voiceBefore = 60 * time.Millisecond
)

// voiceState is one person's part in the selection.
type voiceState struct {
	sentAt, loudAt time.Time
	kept           []byte // the last frame not relayed
	keptAt         time.Time
}

// HearVoice is asked for every audio frame from id on the socket path, with
// the level the frame carries. It answers whether the frame is relayed,
// and, when this frame is what got id a place, the frame of theirs that was
// held back just before it, to be relayed first. frame is retained when it
// is not relayed; the caller must not write to it afterwards.
func (r *Room) HearVoice(id string, level int, frame []byte, now time.Time) (relay bool, before []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.participants[id]
	if !ok {
		return false, nil
	}
	// Loud by the frame's own level, or because the room already ranks them
	// a speaker (the energy report, which a client that does not mark its
	// frames still sends).
	loud := level >= VoiceLoud
	if !loud {
		for _, s := range r.speakers {
			if s == id {
				loud = true
				break
			}
		}
	}
	v := &p.voice
	v.sentAt = now
	if loud {
		v.loudAt = now
	}
	place := -1
	for i, h := range r.voices {
		if h == id {
			return true, nil
		}
		if q := r.participants[h]; q == nil || now.Sub(q.voice.sentAt) > voiceGone {
			place = i // gone or muted: free
		}
	}
	switch {
	case place >= 0:
	case len(r.voices) < MaxVoices:
		r.voices = append(r.voices, id)
		place = len(r.voices) - 1
	case loud:
		// The member quiet the longest, if that is long enough.
		oldest := 0
		for i, h := range r.voices {
			if r.participants[h].voice.loudAt.Before(r.participants[r.voices[oldest]].voice.loudAt) {
				oldest = i
			}
		}
		if now.Sub(r.participants[r.voices[oldest]].voice.loudAt) >= voiceHold {
			place = oldest
		}
	}
	if place < 0 {
		v.kept, v.keptAt = frame, now
		return false, nil
	}
	r.voices[place] = id
	// Only a frame kept a moment ago is the start of this word. (An older
	// one stays where it is, one frame, until the next is kept over it.)
	if v.kept != nil && now.Sub(v.keptAt) <= voiceBefore {
		before = v.kept
	}
	return true, before
}
