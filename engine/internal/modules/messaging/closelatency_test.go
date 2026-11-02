package messaging

import (
	"testing"
	"time"
)

// Closing a socket must not block the caller.
//
// This is not a performance nicety, it is a liveness property, and the call
// graph is what makes it one. Hub.push walks every socket belonging to a user
// and closes any that has fallen behind, and Hub.push runs on the HTTP request
// goroutine of whoever sent the message. So if closing one socket blocks, the
// person who sent a chat message waits for it, and they wait once per stuck
// socket because the loop is serial.
//
// A graceful websocket close writes a close frame and then WAITS for the peer
// to echo one back. A peer that has stopped reading -- which is precisely the
// condition that made us close it -- never echoes, so the wait runs to the
// library timeout. That turns one wedged client into multi-second latency for
// everybody who tries to message them, from any request, and needs no malice
// to happen: a laptop that slept mid-conversation is enough.
func TestCloseDoesNotBlockTheCaller(t *testing.T) {
	h := NewHub(quietLog())
	// The client end exists and is connected, but nothing ever reads from it
	// or answers a close frame. This is a wedged client, not a dead one: a
	// dead one is easy, the socket errors immediately.
	s, _, _ := newSock(t, h, "u1", 4)

	start := time.Now()
	s.close("outbound queue overflow")
	elapsed := time.Since(start)

	t.Logf("close returned in %v", elapsed)

	// Generous: anything approaching a second is already a serious stall on a
	// request path, and the failure mode being guarded is measured in seconds.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("close blocked the caller for %v waiting on a peer that will "+
			"never answer; Hub.push does this inline on an HTTP request goroutine, "+
			"once per stuck socket", elapsed)
	}
}

// The same property when many sockets are wedged at once, which is the shape
// of the real incident: a push fans out to several of a user's tabs.
func TestClosingManyWedgedSocketsIsNotSerial(t *testing.T) {
	h := NewHub(quietLog())
	const n = 4
	socks := make([]*sock, n)
	for i := range socks {
		socks[i], _, _ = newSock(t, h, "u1", 1)
	}

	// Overflow every one of them, so push closes all of them in its loop.
	start := time.Now()
	for i := 0; i < 10; i++ {
		h.PushTyping("u1", "c1", "who", "who-id", true)
	}
	elapsed := time.Since(start)

	t.Logf("push closed %d wedged sockets in %v", n, elapsed)
	if elapsed > time.Second {
		t.Fatalf("pushing to %d wedged sockets took %v on the sender's request "+
			"goroutine", n, elapsed)
	}
}
