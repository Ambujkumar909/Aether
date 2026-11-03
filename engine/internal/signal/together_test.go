package signal

import (
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func (f *presenterFixture) together(from string, on bool, scene uint64) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpTogether, 0, false)
		w.Bits(togState, 3)
		w.Bool(on)
		w.Bits(0, 4)
		w.Uvarint(scene)
	})
}

func (f *presenterFixture) mask(from string, rle []byte) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpTogether, 0, false)
		w.Bits(togMask, 3)
		w.Bits(0, 5)
		w.Blob(rle)
	})
}

func togSeen(got []seen) []seen {
	var out []seen
	for _, s := range got {
		if s.op == wire.OpTogether {
			out = append(out, s)
		}
	}
	return out
}

// Only an organizer applies it for everyone; outlines are relayed only while
// it is on, to everyone but the sender, bounded in size and rate.
func TestTogetherThroughGateway(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c") // a hosts
	f.rm.SetRole("b", wire.RoleModerator)      // a presenter is not enough
	quiet := func(why string, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if got := togSeen(f.drain(id, 250*time.Millisecond)); len(got) != 0 {
				t.Fatalf("%s: %s was sent %+v", why, id, got)
			}
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 200*time.Millisecond)
	}

	f.mask("b", []byte{1, 2, 3})
	quiet("an outline with Together mode off", "a", "c")

	f.together("b", true, 0)
	quiet("a presenter applying it", "a", "b", "c")
	f.together("a", true, room.TogetherScenes)
	quiet("a scene that does not exist", "a", "b", "c")

	f.together("a", true, 1)
	for _, id := range []string{"a", "b", "c"} {
		if got := togSeen(f.drain(id, 400*time.Millisecond)); len(got) != 1 || got[0].b != "on 1" {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	f.together("a", true, 1)
	quiet("the same again", "a", "b", "c")

	f.mask("b", []byte{9, 9, 9})
	for _, id := range []string{"a", "c"} {
		if got := togSeen(f.drain(id, 400*time.Millisecond)); len(got) != 1 || got[0].a != "b" || got[0].b != "3" {
			t.Fatalf("%s: the outline %+v", id, got)
		}
	}
	quiet("the sender's own outline", "b")

	f.mask("b", []byte(strings.Repeat("x", MaxMaskBytes+1)))
	f.mask("b", nil)
	quiet("an outline past the bound, or empty", "a", "c")
	f.mask("b", []byte(strings.Repeat("x", MaxMaskBytes)))
	if got := togSeen(f.drain("a", 400*time.Millisecond)); len(got) != 1 {
		t.Fatalf("an outline exactly at the bound: %+v", got)
	}
	f.drain("c", 200*time.Millisecond)

	for i := 0; i < 50; i++ {
		f.mask("c", []byte{1})
	}
	if n := len(togSeen(f.drain("a", 500*time.Millisecond))); n > room.MaskBurst+1 {
		t.Fatalf("%d of 50 flooded outlines got through", n)
	}
	f.drain("b", 300*time.Millisecond)

	f.together("a", false, 0)
	if got := togSeen(f.drain("b", 400*time.Millisecond)); len(got) != 1 || got[0].b != "off" {
		t.Fatalf("turned off: %+v", got)
	}
	f.drain("a", 200*time.Millisecond)
	f.drain("c", 200*time.Millisecond)
	f.mask("b", []byte{1})
	quiet("an outline after it was turned off", "a", "c")
}
