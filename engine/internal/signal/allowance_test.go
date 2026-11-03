package signal

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func countOp(ss []seen, op wire.Op) int {
	n := 0
	for _, s := range ss {
		if s.op == op {
			n++
		}
	}
	return n
}

// Everything relayed to the room is held to a per-person rate before the
// fan-out: a flood of each kind reaches the others as its burst, not as the
// flood. And a state that did not change goes to nobody.
func TestRelayedFramesAreHeldToAnAllowance(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c") // a hosts
	settle := func() {
		for _, id := range []string{"a", "b", "c"} {
			f.drain(id, 200*time.Millisecond)
		}
	}
	settle()
	state := func(from string, hand bool) {
		f.send(from, func(w *wire.Writer) {
			s := wire.State{Participant: from, MicMuted: true, CamOff: true, HandRaised: hand}
			s.Encode(w)
		})
	}

	// The same state again and again: the first is news, the rest are not.
	for range 50 {
		state("c", true)
	}
	if n := countOp(f.drain("b", 500*time.Millisecond), wire.OpStateDelta); n != 1 {
		t.Fatalf("50 identical states: %d relayed, want 1", n)
	}
	// Toggling as fast as it can: held to the burst.
	for i := range 300 {
		state("c", i%2 == 0)
	}
	if n := countOp(f.drain("b", 600*time.Millisecond), wire.OpStateDelta); n > int(room.AllowState.Burst())+3 {
		t.Fatalf("300 toggles: %d relayed, want about %v", n, room.AllowState.Burst())
	}

	for _, tc := range []struct {
		name  string
		op    wire.Op
		allow room.Allowance
		send  func(i int, w *wire.Writer)
	}{
		{"meeting chat", wire.OpChat, room.AllowChat, func(i int, w *wire.Writer) {
			w.WriteHeader(wire.OpChat, 0, false)
			w.Blob([]byte("spam"))
		}},
		{"whiteboard previews", wire.OpWhiteboard, room.AllowBoard, func(i int, w *wire.Writer) {
			w.WriteHeader(wire.OpWhiteboard, 0, false)
			w.Bits(wbLive, 3)
			w.Bits(0, 5)
			w.Uvarint(uint64(i))
			writeStyle(w, 0, 1, 4)
			writePoints(w, []int32{10, 10, 20, 20})
		}},
	} {
		// (Speech energy has an allowance too, but it is not testable from
		// here: energy is smoothed and only a change of rank is announced, so
		// 400 alternating reports relay once whatever the allowance. The
		// mechanism is room.TestAllowances.)
		if tc.op == wire.OpWhiteboard {
			f.send("a", func(w *wire.Writer) {
				w.WriteHeader(wire.OpWhiteboard, 0, false)
				w.Bits(wbState, 3)
				w.Bits(0, 5)
			})
			settle()
		}
		for i := range 400 {
			f.send("c", func(w *wire.Writer) { tc.send(i, w) })
		}
		n := countOp(f.drain("b", 800*time.Millisecond), tc.op)
		if n < int(tc.allow.Burst()) || n > int(tc.allow.Burst())+8 {
			t.Fatalf("%s: 400 sent, %d relayed, want about %v", tc.name, n, tc.allow.Burst())
		}
	}
}
