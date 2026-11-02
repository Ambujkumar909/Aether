package messaging

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// Calling: a ring reaches every socket the callee holds and nobody else's.
//
// The property is routing, so the test is at the hub, with real sockets
// attached under user ids. Delivery to the wrong person is the failure that
// matters -- a ring is an interruption -- and delivery to only ONE of a
// person's devices is the failure that looks like "calls sometimes do not
// ring", which is how Teams users describe the bug they hate most.

// nextFrame pulls one queued frame off a socket, or fails.
func nextFrame(t *testing.T, s *sock, within time.Duration) []byte {
	t.Helper()
	select {
	case b := <-s.out:
		return b
	case <-time.After(within):
		t.Fatalf("no frame queued for %s within %s", s.userID, within)
		return nil
	}
}

func noFrame(t *testing.T, s *sock, within time.Duration) {
	t.Helper()
	select {
	case b := <-s.out:
		r := wire.NewReader(b, nil)
		h, _ := r.ReadHeader()
		t.Fatalf("%s received an unexpected frame op=0x%x", s.userID, uint16(h.Op))
	case <-time.After(within):
	}
}

func TestRingReachesEveryDeviceOfTheCalleeOnly(t *testing.T) {
	h := NewHub(quietLog())

	// Alice on two devices, Bob on one, Carol on none.
	aliceLaptop, _, rxA1 := newSock(t, h, "u-alice", 8)
	alicePhone, _, rxA2 := newSock(t, h, "u-alice", 8)
	bob, _, _ := newSock(t, h, "u-bob", 8)

	h.PushRing("u-alice", "u-bob", "Bob", "meet-x")

	for _, dev := range []struct {
		s  *sock
		rx *wire.Dict
	}{{aliceLaptop, rxA1}, {alicePhone, rxA2}} {
		r := wire.NewReader(nextFrame(t, dev.s, time.Second), dev.rx)
		hd, err := r.ReadHeader()
		if err != nil || hd.Op != wire.OpRing {
			t.Fatalf("%s: op=0x%x err=%v, want OpRing", dev.s.userID, uint16(hd.Op), err)
		}
		if got := r.Str(); got != "u-bob" {
			t.Fatalf("caller id = %q", got)
		}
		if got := r.Str(); got != "Bob" {
			t.Fatalf("caller name = %q", got)
		}
		if got := r.Str(); got != "meet-x" {
			t.Fatalf("room = %q", got)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// The caller must not ring himself, and a stranger hears nothing.
	noFrame(t, bob, 150*time.Millisecond)
	h.PushRing("u-carol", "u-bob", "Bob", "meet-x") // nobody connected: no panic, no effect
}

func TestRingAnswerReachesTheCaller(t *testing.T) {
	h := NewHub(quietLog())
	bob, _, rxB := newSock(t, h, "u-bob", 8)
	alice, _, _ := newSock(t, h, "u-alice", 8)

	h.PushRingAnswer("u-bob", "u-alice", "Alice", "meet-x", false)

	r := wire.NewReader(nextFrame(t, bob, time.Second), rxB)
	hd, err := r.ReadHeader()
	if err != nil || hd.Op != wire.OpRingAnswer {
		t.Fatalf("op=0x%x err=%v, want OpRingAnswer", uint16(hd.Op), err)
	}
	if got := r.Str(); got != "u-alice" {
		t.Fatalf("callee id = %q", got)
	}
	if got := r.Str(); got != "Alice" {
		t.Fatalf("callee name = %q", got)
	}
	if got := r.Str(); got != "meet-x" {
		t.Fatalf("room = %q", got)
	}
	if acc := r.Bool(); acc {
		t.Fatal("declined answer decoded as accepted")
	}
	r.Bits(7)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	noFrame(t, alice, 150*time.Millisecond)
}
