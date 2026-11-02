package signal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func (f *presenterFixture) rec(from string, what uint32, on bool) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpRecording, 0, false)
		w.Bits(recState, 3)
		w.Bits(what, 2)
		w.Bool(on)
		w.Bits(0, 2)
	})
}

func (f *presenterFixture) caption(from string, final bool, utt uint64, text string) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpRecording, 0, false)
		w.Bits(recCaption, 3)
		w.Bool(final)
		w.Bits(0, 4)
		w.Uvarint(utt)
		w.Blob([]byte(text))
	})
}

func recSeen(got []seen) []seen {
	var out []seen
	for _, s := range got {
		if s.op == wire.OpRecording {
			out = append(out, s)
		}
	}
	return out
}

// Through the dispatcher: who may start what, who is told, who is sent
// which captions, what is written to the transcript and the audit.
func TestRecordingThroughGateway(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c") // a hosts
	var mu sync.Mutex
	var audited []string
	var kept []string
	allowed := true
	f.g.RecordingAllowed = func(context.Context) bool { return allowed }
	f.g.OnRecording = func(_ context.Context, _, _ string, recording, on bool) {
		mu.Lock()
		audited = append(audited, map[bool]string{true: "rec", false: "tr"}[recording]+map[bool]string{true: "+", false: "-"}[on])
		mu.Unlock()
	}
	f.g.OnTranscript = func(_ context.Context, _ string, _ time.Time, _, name, text string, _ time.Time) {
		mu.Lock()
		kept = append(kept, name+": "+text)
		mu.Unlock()
	}
	quiet := func(why string, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if got := recSeen(f.drain(id, 250*time.Millisecond)); len(got) != 0 {
				t.Fatalf("%s: %s was sent %+v", why, id, got)
			}
		}
	}

	// An attendee cannot start a recording: they alone are told why, and
	// nothing is announced.
	f.rec("b", recWhatRecording, true)
	if got := recSeen(f.drain("b", 400*time.Millisecond)); len(got) != 1 || got[0].a != "refused" || got[0].b != "0 1" {
		t.Fatalf("b's refusal: %+v", got)
	}
	quiet("an attendee's start", "a", "c")

	// The deployment's ceiling refuses even the host, and says so.
	allowed = false
	f.rec("a", recWhatRecording, true)
	if got := recSeen(f.drain("a", 400*time.Millisecond)); len(got) != 1 || got[0].a != "refused" || got[0].b != "0 0" {
		t.Fatalf("a's refusal: %+v", got)
	}
	quiet("recording not allowed", "b", "c")
	allowed = true

	f.rec("a", recWhatRecording, true)
	for _, id := range []string{"a", "b", "c"} {
		got := recSeen(f.drain(id, 400*time.Millisecond))
		if len(got) != 1 || got[0].b != "true false false" {
			t.Fatalf("%s told of the recording: %+v", id, got)
		}
	}

	// Starting it again is not news, and not audited twice.
	f.rec("a", recWhatRecording, true)
	quiet("a second start", "a", "b", "c")

	// Nobody wants text: a caption goes nowhere.
	f.caption("b", true, 1, "hello")
	quiet("nobody asked for text", "a", "c")

	// c turns captions on: everyone learns that someone wants them.
	f.rec("c", recWhatCaptions, true)
	for _, id := range []string{"a", "b", "c"} {
		if got := recSeen(f.drain(id, 400*time.Millisecond)); len(got) != 1 || got[0].b != "true false true" {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	// b's partial reaches c only; not a, not b.
	f.caption("b", false, 2, "hel")
	if got := recSeen(f.drain("c", 400*time.Millisecond)); len(got) != 1 || got[0].a != "b" || got[0].b != "false:hel" {
		t.Fatalf("c's caption: %+v", got)
	}
	quiet("a partial", "a", "b")
	// A final line with captions on but no transcription: shown, not kept.
	f.caption("b", true, 2, "not kept")
	if got := recSeen(f.drain("c", 400*time.Millisecond)); len(got) != 1 {
		t.Fatalf("c's final caption: %+v", got)
	}
	quiet("a final line, not transcribing", "a")

	// Transcription on: final lines reach everyone but the speaker and are
	// kept; partials still only reach captions.
	f.rec("a", recWhatTranscr, true)
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 300*time.Millisecond)
	}
	f.caption("b", true, 2, "hello all")
	for _, id := range []string{"a", "c"} {
		if got := recSeen(f.drain(id, 400*time.Millisecond)); len(got) != 1 || got[0].b != "true:hello all" {
			t.Fatalf("%s: the final line %+v", id, got)
		}
	}
	quiet("the speaker's own line", "b")
	f.caption("b", false, 3, "and")
	quiet("a partial while transcribing", "a")
	f.drain("c", 300*time.Millisecond)

	// Muted, by yourself or by a host: your words are not published.
	f.parts["b"].State.MicMuted = true
	f.caption("b", true, 7, "muted words")
	quiet("a muted speaker", "a", "c")
	f.parts["b"].State.MicMuted = false
	if _, err := f.rm.SetBlocked("b", true, false); err != nil {
		t.Fatal(err)
	}
	f.caption("b", true, 8, "blocked words")
	quiet("a speaker whose mic a host disabled", "a", "c")
	if _, err := f.rm.SetBlocked("b", false, false); err != nil {
		t.Fatal(err)
	}

	// Bounds: one byte over, and text that is not UTF-8.
	f.caption("b", true, 4, strings.Repeat("x", MaxCaptionBytes+1))
	f.caption("b", true, 5, "\xff\xfe")
	quiet("a bad line", "a", "c")
	f.caption("b", true, 6, strings.Repeat("y", MaxCaptionBytes))
	if got := recSeen(f.drain("a", 400*time.Millisecond)); len(got) != 1 {
		t.Fatalf("a line exactly at the bound: %+v", got)
	}
	f.drain("c", 300*time.Millisecond)

	mu.Lock()
	if strings.Join(kept, "|") != "b: hello all|b: "+strings.Repeat("y", MaxCaptionBytes) {
		t.Fatalf("transcript kept %q", kept)
	}
	mu.Unlock()

	// Flooding: the allowance, not the client, decides how many get through.
	for i := 0; i < 60; i++ {
		f.caption("b", false, uint64(10+i), "spam")
	}
	if n := len(recSeen(f.drain("c", 600*time.Millisecond))); n > room.CaptionBurst+1 {
		t.Fatalf("%d of 60 flooded captions got through", n)
	}

	f.rec("a", recWhatRecording, false)
	f.rec("a", recWhatTranscr, false)
	mu.Lock()
	if strings.Join(audited, ",") != "rec+,tr+,rec-,tr-" {
		t.Fatalf("audited %v", audited)
	}
	mu.Unlock()
}
