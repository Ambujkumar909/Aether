package messaging

import (
	"fmt"
	"testing"
	"time"
)

func TestTypingGateCoalescesAndRelaysStopsOnce(t *testing.T) {
	g := newTypingGate()
	t0 := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	if !g.allow("u", "c", true, t0) {
		t.Fatal("the first typing signal relays")
	}
	if g.allow("u", "c", true, t0.Add(time.Second)) {
		t.Fatal("a second signal inside typingRepeat is swallowed")
	}
	if !g.allow("u", "c", true, t0.Add(typingRepeat)) {
		t.Fatal("on the repeat line it relays again")
	}
	if !g.typing("u", "c", t0.Add(typingRepeat)) {
		t.Fatal("known to be typing")
	}
	if !g.allow("u", "c", false, t0.Add(3*time.Second)) {
		t.Fatal("a stop while typing relays")
	}
	if g.allow("u", "c", false, t0.Add(4*time.Second)) {
		t.Fatal("a second stop relays nothing")
	}
	if g.allow("v", "c", false, t0) {
		t.Fatal("a stop from somebody never seen typing relays nothing")
	}
	// Another conversation is another key.
	if !g.allow("u", "d", true, t0.Add(time.Second)) {
		t.Fatal("conversations are independent")
	}
	// A long-forgotten typist is a fresh one.
	if !g.allow("u", "d", true, t0.Add(time.Second+typingForget+time.Second)) {
		t.Fatal("after typingForget a typing signal relays again")
	}
	if g.typing("u", "d", t0.Add(2*time.Minute)) {
		t.Fatal("a stale entry is not typing")
	}
}

func TestTypingGateSweepsStaleEntries(t *testing.T) {
	g := newTypingGate()
	t0 := time.Now()
	for i := 0; i < typingSweepAt+10; i++ {
		g.allow(fmt.Sprintf("u%d", i), "c", true, t0)
	}
	if len(g.last) <= typingSweepAt {
		t.Fatalf("expected the table to have grown past the sweep line: %d", len(g.last))
	}
	// One more signal, well after typingForget: the sweep runs and the old
	// entries go.
	g.allow("late", "c", true, t0.Add(typingForget+time.Second))
	if n := len(g.last); n != 1 {
		t.Fatalf("after the sweep %d entries remain, want 1", n)
	}
}
