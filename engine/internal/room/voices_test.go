package room

import (
	"fmt"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

// Which voices the socket path relays: everyone while no more microphones
// are open than it relays; past that, whoever is loud now, in the place of
// whoever has been quiet the longest, and never in the place of someone
// between two words; the frame before the one that won the place goes with
// it; a place is freed by silence on the wire or by leaving.

func voiceRoom(t *testing.T, n int) (*Room, []string) {
	t.Helper()
	r := New("v", ledger.NewMeter("t"))
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%02d", i)
		if _, err := r.Join(&Participant{ID: ids[i]}); err != nil {
			t.Fatal(err)
		}
	}
	return r, ids
}

const quiet, loud = 0, VoiceLoud

func TestUpToSixMicrophonesAreAllHeardWhateverTheirLevel(t *testing.T) {
	r, ids := voiceRoom(t, MaxVoices+1)
	now := time.Unix(1000, 0)
	// Six people sending silence for a second: every frame is relayed.
	for f := 0; f < 50; f++ {
		now = now.Add(20 * time.Millisecond)
		for _, id := range ids[:MaxVoices] {
			if relay, before := r.HearVoice(id, quiet, []byte{1}, now); !relay || before != nil {
				t.Fatalf("frame %d from %s: relay=%v before=%v", f, id, relay, before)
			}
		}
	}
	// One over: a seventh, as quiet as they are, is not.
	for f := 0; f < 5; f++ {
		now = now.Add(20 * time.Millisecond)
		if relay, _ := r.HearVoice(ids[MaxVoices], quiet, []byte{1}, now); relay {
			t.Fatal("a seventh quiet microphone was relayed")
		}
		for _, id := range ids[:MaxVoices] {
			if relay, _ := r.HearVoice(id, quiet, []byte{1}, now); !relay {
				t.Fatalf("%s lost their place to a quiet seventh", id)
			}
		}
	}
	if len(r.voices) != MaxVoices {
		t.Fatalf("%d heard, want %d", len(r.voices), MaxVoices)
	}
	// Somebody who is not in the room is never heard.
	if relay, _ := r.HearVoice("nobody", 63, []byte{1}, now); relay {
		t.Fatal("a stranger's frame was relayed")
	}
}

func TestALoudNewcomerTakesThePlaceOfWhoeverHasBeenQuietLongest(t *testing.T) {
	r, ids := voiceRoom(t, MaxVoices+2)
	six, seventh, eighth := ids[:MaxVoices], ids[MaxVoices], ids[MaxVoices+1]
	now := time.Unix(1000, 0)
	tick := func(levels map[string]int) map[string]bool {
		now = now.Add(20 * time.Millisecond)
		out := map[string]bool{}
		for id, lvl := range levels {
			out[id], _ = r.HearVoice(id, lvl, []byte{1}, now)
		}
		return out
	}
	all := func(lvl int, who ...string) map[string]int {
		m := map[string]int{}
		for _, id := range who {
			m[id] = lvl
		}
		return m
	}
	// The six are all talking. A seventh who talks too is not heard: nobody
	// has stopped.
	tick(all(loud, six...))
	for f := 0; f < 10; f++ {
		m := all(loud, six...)
		m[seventh] = loud
		if got := tick(m); got[seventh] {
			t.Fatal("a seventh voice displaced someone who was talking")
		}
	}
	// p00 goes quiet first, then p01; the others keep talking. For a second
	// and a half p00 is between words, and keeps the place.
	quietFrom := now
	for now.Sub(quietFrom) < voiceHold-40*time.Millisecond {
		m := all(loud, six[2:]...)
		m[six[0]], m[six[1]], m[seventh] = quiet, quiet, loud
		if got := tick(m); got[seventh] || !got[six[0]] {
			t.Fatalf("%v after p00 went quiet: seventh=%v p00=%v", now.Sub(quietFrom), got[seventh], got[six[0]])
		}
		if now.Sub(quietFrom) < 100*time.Millisecond {
			m[six[1]] = loud // p01 spoke a little longer than p00
			tick(all(loud, six[1]))
		}
	}
	// Past the hold, the seventh is heard, and it is p00 who is not.
	var got map[string]bool
	for f := 0; f < 5; f++ {
		m := all(loud, six[2:]...)
		m[six[0]], m[six[1]], m[seventh] = quiet, quiet, loud
		got = tick(m)
	}
	if !got[seventh] || got[six[0]] || !got[six[1]] {
		t.Fatalf("after the hold: seventh=%v p00=%v p01=%v", got[seventh], got[six[0]], got[six[1]])
	}
	// The eighth, loud a moment later, takes p01's: the next quietest.
	for f := 0; f < 5; f++ {
		m := all(loud, six[2:]...)
		m[six[1]], m[seventh], m[eighth] = quiet, loud, loud
		got = tick(m)
	}
	if !got[eighth] || got[six[1]] || !got[seventh] {
		t.Fatalf("the eighth: eighth=%v p01=%v seventh=%v", got[eighth], got[six[1]], got[seventh])
	}
	// p00 speaks again while all six heard are talking: not until one stops.
	m := all(loud, six[2:]...)
	m[seventh], m[eighth], m[six[0]] = loud, loud, loud
	if got := tick(m); got[six[0]] {
		t.Fatal("p00 came back over six people talking")
	}
	if len(r.voices) != MaxVoices {
		t.Fatalf("%d heard", len(r.voices))
	}
}

func TestTheFrameBeforeTheOneThatWonThePlaceGoesFirst(t *testing.T) {
	r, ids := voiceRoom(t, MaxVoices+1)
	six, seventh := ids[:MaxVoices], ids[MaxVoices]
	now := time.Unix(1000, 0)
	for _, id := range six {
		r.HearVoice(id, quiet, []byte{1}, now)
	}
	// The six have long been quiet. The seventh's breath before the word is
	// held, and goes out in front of the first loud frame.
	now = now.Add(voiceHold)
	for _, id := range six {
		r.HearVoice(id, quiet, []byte{1}, now)
	}
	if relay, _ := r.HearVoice(seventh, quiet, []byte("older"), now); relay {
		t.Fatal("quiet and over the limit, yet relayed")
	}
	now = now.Add(20 * time.Millisecond)
	if relay, _ := r.HearVoice(seventh, quiet, []byte("onset"), now); relay {
		t.Fatal("quiet and over the limit, yet relayed")
	}
	now = now.Add(20 * time.Millisecond)
	relay, before := r.HearVoice(seventh, loud, []byte("word"), now)
	if !relay || string(before) != "onset" {
		t.Fatalf("the first loud frame: relay=%v before=%q, want the frame just before it", relay, before)
	}
	// Once: the next frame has nothing in front of it.
	now = now.Add(20 * time.Millisecond)
	if relay, before := r.HearVoice(seventh, loud, []byte("word2"), now); !relay || before != nil {
		t.Fatalf("the second: relay=%v before=%q", relay, before)
	}

	// A frame held long ago is not the start of this word. Displace the
	// seventh (quiet past the hold, somebody else loud), let time pass.
	now = now.Add(20 * time.Millisecond)
	r.HearVoice(seventh, quiet, []byte{1}, now)
	now = now.Add(voiceHold + time.Second)
	for _, id := range six[1:] {
		r.HearVoice(id, quiet, []byte{1}, now) // still sending, so their places are not free
	}
	if relay, _ := r.HearVoice(six[0], loud, []byte{1}, now); !relay {
		t.Fatal("p00, long displaced... should be heard: it never lost its place")
	}
	// Whoever is out now holds a stale frame; when they win a place much
	// later, it is not sent.
	out := ""
	for _, id := range ids {
		heard := false
		for _, h := range r.voices {
			heard = heard || h == id
		}
		if !heard {
			out = id
		}
	}
	if relay, _ := r.HearVoice(out, quiet, []byte("stale"), now); relay {
		t.Fatalf("%s is out and quiet, yet relayed", out)
	}
	now = now.Add(voiceBefore + 20*time.Millisecond)
	for _, id := range r.voices {
		r.participants[id].voice.loudAt = now.Add(-voiceHold) // all quiet long enough
		r.participants[id].voice.sentAt = now
	}
	if relay, before := r.HearVoice(out, loud, []byte("word"), now); !relay || before != nil {
		t.Fatalf("a frame held %v ago: relay=%v before=%q", voiceBefore+20*time.Millisecond, relay, before)
	}
}

func TestAPlaceIsFreedBySilenceOnTheWireOrByLeaving(t *testing.T) {
	r, ids := voiceRoom(t, MaxVoices+2)
	six, seventh, eighth := ids[:MaxVoices], ids[MaxVoices], ids[MaxVoices+1]
	now := time.Unix(1000, 0)
	for _, id := range six {
		r.HearVoice(id, loud, []byte{1}, now)
	}
	// p00 mutes: nothing more from them. The rest keep talking. Until two
	// seconds have passed a quiet seventh is not heard; after, they are.
	step := func(d time.Duration) {
		now = now.Add(d)
		for _, id := range six[1:] {
			r.HearVoice(id, loud, []byte{1}, now)
		}
	}
	step(voiceGone - 20*time.Millisecond)
	if relay, _ := r.HearVoice(seventh, quiet, []byte{1}, now); relay {
		t.Fatal("p00's place was given away before it had been silent long enough")
	}
	step(40 * time.Millisecond)
	if relay, _ := r.HearVoice(seventh, quiet, []byte{1}, now); !relay {
		t.Fatal("p00 sent nothing for two seconds and a quiet newcomer still was not heard")
	}
	// p01 leaves the meeting: the eighth, quiet, has the place at once.
	if relay, _ := r.HearVoice(eighth, quiet, []byte{1}, now); relay {
		t.Fatal("no place was free for the eighth yet")
	}
	if err := r.Leave(six[1]); err != nil {
		t.Fatal(err)
	}
	if relay, _ := r.HearVoice(eighth, quiet, []byte{1}, now); !relay {
		t.Fatal("somebody left and their place stayed taken")
	}
	if len(r.voices) != MaxVoices {
		t.Fatalf("%d heard", len(r.voices))
	}
}

// A client that does not mark its frames still reports its energy four
// times a second; being ranked a speaker by that counts as loud.
func TestARankedSpeakerIsLoudWithoutAMarkOnTheFrame(t *testing.T) {
	r, ids := voiceRoom(t, MaxVoices+1)
	six, seventh := ids[:MaxVoices], ids[MaxVoices]
	now := time.Unix(1000, 0)
	for _, id := range six {
		r.HearVoice(id, quiet, []byte{1}, now)
	}
	now = now.Add(voiceHold)
	for _, id := range six {
		r.HearVoice(id, quiet, []byte{1}, now)
	}
	if relay, _ := r.HearVoice(seventh, quiet, []byte{1}, now); relay {
		t.Fatal("unranked and unmarked, yet relayed")
	}
	for i := 0; i < 4; i++ {
		r.ObserveEnergy(seventh, 0.9)
	}
	if relay, _ := r.HearVoice(seventh, quiet, []byte{1}, now); !relay {
		t.Fatal("ranked a speaker by its energy reports, and still not heard")
	}
}

// The decision is taken on every audio frame of every sender.
func BenchmarkHearVoice(b *testing.B) {
	r := New("v", ledger.NewMeter("t"))
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%03d", i)
		_, _ = r.Join(&Participant{ID: ids[i]})
	}
	frame := make([]byte, 120)
	now := time.Unix(1000, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lvl := quiet
		if i%100 < 3 {
			lvl = loud
		}
		r.HearVoice(ids[i%100], lvl, frame, now.Add(time.Duration(i/100)*20*time.Millisecond))
	}
}
