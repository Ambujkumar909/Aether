package signal

import (
	"fmt"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func (f *presenterFixture) spotlight(from, id string, on bool) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpSpotlight, 0, false)
		w.Str(id)
		w.Bool(on)
		w.Bits(0, 7)
	})
}

func count(ss []seen, op wire.Op) int {
	n := 0
	for _, s := range ss {
		if s.op == op {
			n++
		}
	}
	return n
}

// Through the real dispatcher on real sockets. a hosts (first in).
func TestSpotlightRules(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")

	// A member cannot spotlight anybody, themselves included.
	f.spotlight("b", "b", true)
	f.spotlight("b", "c", true)
	if got := f.drain("c", 300*time.Millisecond); count(got, wire.OpSpotlight) != 0 || len(f.rm.Spotlights()) != 0 {
		t.Fatalf("a member's spotlight was applied: %v %+v", f.rm.Spotlights(), got)
	}

	// The host spotlights b: everybody, b included, gets the whole list.
	f.spotlight("a", "b", true)
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 400*time.Millisecond); !has(got, wire.OpSpotlight, "b", "") {
			t.Fatalf("%s was not told b is spotlighted: %+v", id, got)
		}
	}

	// No change, no frame: spotlighting b again sends nothing (rule 2).
	f.spotlight("a", "b", true)
	if got := f.drain("c", 300*time.Millisecond); count(got, wire.OpSpotlight) != 0 {
		t.Fatalf("an unchanged list was resent: %+v", got)
	}

	// c (not spotlighted, not host) cannot take b off the stage, nor stop all.
	f.spotlight("c", "b", false)
	f.spotlight("c", "", false)
	if !f.rm.IsSpotlit("b") {
		t.Fatal("a member removed somebody else's spotlight")
	}

	// b exits their own spotlight.
	f.spotlight("b", "b", false)
	if got := f.drain("c", 400*time.Millisecond); !has(got, wire.OpSpotlight, "", "") || f.rm.IsSpotlit("b") {
		t.Fatalf("exit spotlight not applied/broadcast: %v %+v", f.rm.Spotlights(), got)
	}

	// Host: two on, then stop all.
	f.spotlight("a", "b", true)
	f.spotlight("a", "c", true)
	if got := f.drain("a", 400*time.Millisecond); !has(got, wire.OpSpotlight, "b,c", "") {
		t.Fatalf("order not kept: %+v", got)
	}
	f.spotlight("a", "", false)
	if got := f.drain("b", 400*time.Millisecond); !has(got, wire.OpSpotlight, "", "") || len(f.rm.Spotlights()) != 0 {
		t.Fatalf("stop all: %v %+v", f.rm.Spotlights(), got)
	}
}

// The eighth is refused at the server whatever the client does, and the
// refusal sends nothing.
func TestSpotlightLimitEnforcedAtTheServer(t *testing.T) {
	ids := []string{"host"}
	for i := range room.MaxSpotlights + 1 {
		ids = append(ids, fmt.Sprint("p", i))
	}
	f := newPresenterFixture(t, ids...)
	for _, id := range ids[1 : room.MaxSpotlights+1] {
		f.spotlight("host", id, true)
	}
	f.drain("host", 400*time.Millisecond)
	f.spotlight("host", ids[len(ids)-1], true)
	if n := len(f.rm.Spotlights()); n != room.MaxSpotlights {
		t.Fatalf("%d spotlighted, limit %d", n, room.MaxSpotlights)
	}
	if got := f.drain("host", 300*time.Millisecond); count(got, wire.OpSpotlight) != 0 {
		t.Fatalf("a refused spotlight was broadcast: %+v", got)
	}
}

// A member cannot end the meeting. The host can: every participant, the host
// included, is told and then disconnected, and the room is reset.
func TestEndMeetingForEveryone(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	f.rm.SetLocked(true)
	f.rm.Spotlight("b")

	f.send("b", func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
	if r := f.conns["c"].CloseReason(); r != "" || !f.rm.Locked() {
		t.Fatalf("a member ended the meeting (c closed %q, locked %v)", r, f.rm.Locked())
	}
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 100*time.Millisecond)
	}

	f.send("a", func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 500*time.Millisecond); count(got, wire.OpEndMeeting) != 1 {
			t.Fatalf("%s did not receive the end exactly once: %+v", id, got)
		}
		if r := f.conns[id].CloseReason(); r != "meeting ended" {
			t.Fatalf("%s closed with %q", id, r)
		}
	}
	if f.rm.Locked() || len(f.rm.Spotlights()) != 0 {
		t.Fatal("room not reset by End")
	}
	// Ending again (a second host's click arriving late) is harmless.
	f.send("a", func(w *wire.Writer) { w.WriteHeader(wire.OpEndMeeting, 0, false) })
}
