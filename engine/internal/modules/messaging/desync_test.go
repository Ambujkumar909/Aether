package messaging

import (
	"sync"
	"testing"

	"github.com/aether-rtc/aether/internal/wire"
)

// The regression test for the dictionary desync, kept in its own file because
// it is the one that actually reproduced the bug rather than merely asserting
// the absence of it.
//
// Every push carries the SAME two strings. The first frame encoded emits them
// as literals bound to fresh indices; every frame after it emits only the
// indices. A single reorder anywhere in the run therefore leaves the reader
// holding an index that nothing ever defined, which fails immediately and
// unambiguously.
//
// Against the version of Hub.push that released the socket lock after
// encoding and queued the frame afterwards, this reported corruption in
// 7 rounds out of 400 with no artificial delay at all, and 69 out of 400 with
// a single runtime.Gosched() widening the window. The gap was a few
// nanoseconds wide and entirely reachable, and what came out the other side
// was a chat connection that decoded every interned string to the wrong value
// for as long as it stayed open.
func TestDictionaryNeverDesyncsUnderConcurrentPush(t *testing.T) {
	noLeaks(t)
	const rounds, per = 200, 256

	for round := 0; round < rounds; round++ {
		h := NewHub(quietLog())
		tx, rx := wire.NewPreloadedPair()
		s := &sock{userID: "u1", tx: tx, out: make(chan []byte, per+8)}
		h.add(s)

		// Release every goroutine at once, so they pile onto the socket lock
		// the way concurrent request handlers do.
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < per; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				h.PushTyping("u1", "conversation-alpha", "witness-beta", "who-id", true)
			}()
		}
		close(start)
		wg.Wait()

		close(s.out)
		n := 0
		for frame := range s.out {
			r := wire.NewReader(frame, rx)
			if _, err := r.ReadHeader(); err != nil {
				t.Fatalf("round %d frame %d: header: %v", round, n, err)
			}
			conv, who := r.Str(), r.Str()
			if err := r.Err(); err != nil {
				t.Fatalf("round %d frame %d: %v -- a frame referenced a dictionary "+
					"index that no earlier frame defined, so frames were queued in a "+
					"different order than they were encoded", round, n, err)
			}
			if conv != "conversation-alpha" || who != "witness-beta" {
				t.Fatalf("round %d frame %d: decoded %q / %q; the transmit dictionary desynced",
					round, n, conv, who)
			}
			n++
		}
		if n != per {
			t.Fatalf("round %d: queued %d frames, expected %d", round, n, per)
		}
	}
	t.Logf("%d rounds x %d concurrent pushes: dictionary intact throughout", rounds, per)
}
