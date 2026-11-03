package signal

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func reactionsFrom(ss []seen, who string) int {
	n := 0
	for _, s := range ss {
		if s.op == wire.OpReaction && s.a == who {
			n++
		}
	}
	return n
}

// Reactions are relayed to everybody, so one sender's rate is multiplied by
// the room. A client that ignores its own throttle is held to the browser's:
// the burst, then two a second. Somebody else's allowance is their own.
func TestReactionsAreRateLimitedPerPerson(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	for _, id := range []string{"a", "b", "c"} {
		f.drain(id, 200*time.Millisecond)
	}
	react := func(from, emoji string) {
		f.send(from, func(w *wire.Writer) {
			w.WriteHeader(wire.OpReaction, 0, false)
			w.Str(emoji)
		})
	}
	for range 200 {
		react("c", "🎉")
	}
	// Straight after, before anything refills: another person is not held
	// to c's allowance.
	react("a", "👏")
	seenB := f.drain("b", 600*time.Millisecond)
	got := reactionsFrom(seenB, "c")
	// The burst, plus whatever refilled while 200 frames were read (a few at most).
	if got < int(room.AllowReaction.Burst()) || got > int(room.AllowReaction.Burst())+2 {
		t.Fatalf("200 reactions at once: %d relayed, want about %v", got, room.AllowReaction.Burst())
	}
	if n := reactionsFrom(seenB, "a"); n != 1 {
		t.Fatalf("a's reaction after c's flood: %d", n)
	}
	// It refills: a second later, two more.
	time.Sleep(1100 * time.Millisecond)
	react("c", "👍")
	react("c", "👍")
	if n := reactionsFrom(f.drain("b", 400*time.Millisecond), "c"); n != 2 {
		t.Fatalf("after a second: %d relayed, want 2", n)
	}
}
