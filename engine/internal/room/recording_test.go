package room

import (
	"errors"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// Organizers and presenters start and stop; attendees do not. Starting what
// is running changes nothing. Leaving takes your recording with you, not
// the transcript.
func TestRecordingWhoAndLeaving(t *testing.T) {
	r := spotRoom(t, 3) // p0 hosts; p1, p2
	r.SetRole("p1", wire.RoleMember)
	r.SetRole("p2", wire.RoleModerator) // a presenter
	r.participants["p0"].DisplayName, r.participants["p2"].DisplayName = "Hana", "Pia"
	now := time.Now()

	if _, err := r.SetRecording("p1", true, now); !errors.Is(err, ErrRecDenied) {
		t.Fatalf("an attendee started recording: %v", err)
	}
	if _, err := r.SetTranscribing("p1", true, now); !errors.Is(err, ErrRecDenied) {
		t.Fatalf("an attendee started transcription: %v", err)
	}
	if ok, err := r.SetRecording("p2", true, now); !ok || err != nil {
		t.Fatalf("a presenter could not record: %v %v", ok, err)
	}
	if ok, _ := r.SetRecording("p0", true, now); ok {
		t.Fatal("starting a running recording reported a change")
	}
	if ok, err := r.SetTranscribing("p0", true, now); !ok || err != nil {
		t.Fatalf("the host could not transcribe: %v %v", ok, err)
	}
	st := r.Recording()
	if !st.Recording || st.RecBy != "Pia" || !st.Transcribing || st.TrBy != "Hana" || !st.RecSince.Equal(now) {
		t.Fatalf("state %+v", st)
	}
	if _, err := r.SetRecording("p1", false, now); !errors.Is(err, ErrRecDenied) {
		t.Fatalf("an attendee stopped it: %v", err)
	}

	if r.LeaveRecording("p1") {
		t.Fatal("an attendee leaving changed the recording")
	}
	if !r.LeaveRecording("p2") {
		t.Fatal("the recorder left and nothing changed")
	}
	if st := r.Recording(); st.Recording || !st.Transcribing {
		t.Fatalf("after the recorder left: %+v", st)
	}
	if ok, _ := r.SetTranscribing("p0", false, now); !ok || r.Recording().Transcribing {
		t.Fatal("the host could not stop transcription")
	}
}

// Captions: "somebody wants them" changes only on the first on and the last
// off, and a leaver's captions go with them.
func TestCaptionsWanted(t *testing.T) {
	r := spotRoom(t, 3)
	if !r.WantCaptions("p1", true) || r.WantCaptions("p2", true) {
		t.Fatal("first on should change, second should not")
	}
	if !r.CaptionsOn("p1") || r.CaptionsOn("p0") {
		t.Fatal("who has them on")
	}
	if r.WantCaptions("p1", false) || !r.Recording().Captions {
		t.Fatal("one still has them on")
	}
	r.WantCaptions("p0", true)
	if r.LeaveRecording("p0") || !r.Recording().Captions {
		t.Fatal("one with captions left while another still has them on")
	}
	if !r.LeaveRecording("p2") || r.Recording().Captions {
		t.Fatal("the last one with captions left and they stayed on")
	}
	if r.WantCaptions("stranger", true) {
		t.Fatal("someone not in the room turned captions on")
	}
}

// The allowance: a burst, then the sustained rate.
func TestCaptionAllowance(t *testing.T) {
	r := spotRoom(t, 2)
	now := time.Now()
	n := 0
	for i := 0; i < 100; i++ {
		if r.CaptionAllowed("p1", now) {
			n++
		}
	}
	if n != CaptionBurst {
		t.Fatalf("%d allowed at once, want %d", n, CaptionBurst)
	}
	n = 0
	for i := 1; i <= 100; i++ { // ten seconds, a frame every 100 ms
		if r.CaptionAllowed("p1", now.Add(time.Duration(i)*100*time.Millisecond)) {
			n++
		}
	}
	if n < 10*CaptionRate-1 || n > 10*CaptionRate+1 {
		t.Fatalf("%d allowed in ten seconds, want about %d", n, 10*CaptionRate)
	}
	// A silence does not bank a bigger burst: five quiet seconds earn 30
	// frames, and only the burst may be spent at once.
	later, n := now.Add(15*time.Second), 0
	for i := 0; i < 100; i++ {
		if r.CaptionAllowed("p1", later) {
			n++
		}
	}
	if n != CaptionBurst {
		t.Fatalf("%d allowed after a silence, want %d", n, CaptionBurst)
	}
	if r.CaptionAllowed("stranger", now) {
		t.Fatal("someone not in the room")
	}
}
