package signal

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// Recording, transcription and live captions on the wire (OpRecording
// 0x102). The room keeps who started what (room/recording.go); this is the
// encoding, who is sent what, and where the transcript is written.
//
// Speech becomes text in each speaker's own browser, from their own
// microphone, so a line arrives already attributed: there is no guessing who
// spoke. A caption frame carries the speaker's utterance number, so the
// partial text being spoken is replaced in place and the final line closes
// it. Partials go only to people with captions on; final lines go to
// everyone while the meeting is transcribed (the transcript panel is live),
// otherwise also only to people with captions on. The speaker is never sent
// their own line back -- their client already has it.
//
//	client -> server
//	  set      kind 0 | what 2 bits (0 recording, 1 transcription,
//	           2 my captions) | on 1 bit | pad
//	  caption  kind 1 | final 1 bit | pad | utterance uvarint | text blob
//	server -> client
//	  state    kind 0 | recording, transcribing, captions 1 bit each | pad
//	           [if recording] by str, since uvarint (unix ms)
//	           [if transcribing] by str, since uvarint (unix ms)
//	  caption  kind 1 | final 1 bit | pad | speaker str | utterance uvarint | text blob
//	  refused  kind 2 | what 2 bits | reason 1 bit (0 the deployment has
//	           recording off, 1 not an organizer or presenter) | pad
//	           -- to the person who asked, only: Teams' "Recording not
//	           started. Your IT admin has disabled Recording."

const (
	recState   = 0
	recCaption = 1
	recRefused = 2

	refusedByDeployment = 0
	refusedByRole       = 1

	recWhatRecording = 0
	recWhatTranscr   = 1
	recWhatCaptions  = 2
)

// MaxCaptionBytes bounds one line of speech (UTF-8). Teams breaks captions
// into lines far shorter; a longer one is a client that is not speaking.
const MaxCaptionBytes = 500

func encodeRecState(s room.RecState) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpRecording, 0, false)
		w.Bits(recState, 3)
		w.Bool(s.Recording)
		w.Bool(s.Transcribing)
		w.Bool(s.Captions)
		w.Bits(0, 2)
		if s.Recording {
			w.Str(s.RecBy)
			w.Uvarint(uint64(s.RecSince.UnixMilli()))
		}
		if s.Transcribing {
			w.Str(s.TrBy)
			w.Uvarint(uint64(s.TrSince.UnixMilli()))
		}
	}
}

func encodeCaption(speaker string, final bool, utt uint64, text []byte) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpRecording, 0, false)
		w.Bits(recCaption, 3)
		w.Bool(final)
		w.Bits(0, 4)
		w.Str(speaker)
		w.Uvarint(utt)
		w.Blob(text)
	}
}

func encodeRecRefused(what, reason uint32) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpRecording, 0, false)
		w.Bits(recRefused, 3)
		w.Bits(what, 2)
		w.Bits(reason, 1)
		w.Bits(0, 2)
	}
}

func (g *Gateway) handleRecording(ctx context.Context, rm *room.Room, p *room.Participant, r *wire.Reader) error {
	kind := r.Bits(3)
	switch kind {
	case recState:
		what := r.Bits(2)
		on := r.Bool()
		r.Bits(2)
		if err := r.Err(); err != nil {
			return err
		}
		now := time.Now()
		var changed bool
		var err error
		switch what {
		case recWhatRecording:
			// The deployment's ceiling: "Recording allowed" off means
			// nobody records, whatever their role.
			if on && g.RecordingAllowed != nil && !g.RecordingAllowed(ctx) {
				if c := g.connFor(p.ID); c != nil {
					c.Emit(encodeRecRefused(what, refusedByDeployment))
				}
				return nil
			}
			changed, err = rm.SetRecording(p.ID, on, now)
		case recWhatTranscr:
			changed, err = rm.SetTranscribing(p.ID, on, now)
		case recWhatCaptions:
			changed = rm.WantCaptions(p.ID, on)
		default:
			return nil
		}
		if err != nil {
			if c := g.connFor(p.ID); c != nil {
				c.Emit(encodeRecRefused(what, refusedByRole))
			}
			return nil
		}
		if !changed {
			return nil
		}
		if what != recWhatCaptions && g.OnRecording != nil {
			g.OnRecording(ctx, p.UserID, rm.ID, what == recWhatRecording, on)
		}
		g.announceAll(rm, encodeRecState(rm.Recording()))
	case recCaption:
		final := r.Bool()
		r.Bits(4)
		utt := r.Uvarint()
		text := r.Blob()
		if err := r.Err(); err != nil {
			return err
		}
		if len(text) == 0 || len(text) > MaxCaptionBytes || !utf8.Valid(text) {
			return nil
		}
		// Captions are speech. Somebody muted -- by themselves, or by a host
		// who disabled their mic -- is not speaking to the room, so their
		// words do not reach it either. (OpCaption, the captions-only path
		// this replaces, had the host half of this rule; ARCHITECTURE §6q.)
		if p.State.MicMuted || !rm.May(p.ID, "mic") {
			return nil
		}
		st := rm.Recording()
		if !rm.CaptionAllowed(p.ID, time.Now()) {
			return nil
		}
		line := append([]byte(nil), text...)
		if final && st.Transcribing && g.OnTranscript != nil {
			g.OnTranscript(ctx, rm.ID, rm.Epoch(), p.UserID, p.DisplayName, string(line), time.Now())
		}
		enc := encodeCaption(p.ID, final, utt, line)
		for _, q := range rm.Snapshot() {
			if q.ID == p.ID {
				continue
			}
			if (final && st.Transcribing) || rm.CaptionsOn(q.ID) {
				if qc := g.connFor(q.ID); qc != nil {
					qc.Emit(enc)
				}
			}
		}
	}
	return nil
}
