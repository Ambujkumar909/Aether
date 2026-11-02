package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// Publishing racing a peer closing.
//
// This reproduces a crash rather than a slowdown. Peer.Close used to set
// published and downstreams to nil, and both maps are written from goroutines
// Close cannot see: pion delivers OnTrack on its own goroutine, and the router
// fans a newly published track out to every peer in the room. One participant
// leaving at the moment another started sending therefore panicked with
// "assignment to entry in nil map".
//
// A panic in a Go server is not a dropped frame. It takes down the process,
// and with it every other meeting on the node. One person clicking Leave with
// unlucky timing ends everybody's call.
//
// These tests drive Subscribe and Close against each other directly, without
// ICE or DTLS, so the window is hit reliably instead of once in a hundred
// integration runs.

func testPeer(t *testing.T, id string) *Peer {
	t.Helper()
	p, err := NewPeer(id, "room", ledger.NewMeter("t"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func fakeTrack(id, publisher string) *PublishedTrack {
	return NewPublishedTrack(id, publisher, webrtc.RTPCodecTypeVideo,
		webrtc.MimeTypeVP8, false, ledger.NewMeter("t"), func(LayerID) {})
}

// Subscribe must never panic when the peer closes underneath it.
func TestSubscribeRacingCloseDoesNotPanic(t *testing.T) {
	for round := 0; round < 200; round++ {
		p := testPeer(t, "p1")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				// A panic here fails the test rather than killing the run,
				// because the testing framework recovers goroutine panics as
				// failures only if they happen on the test goroutine; a raw
				// panic elsewhere aborts everything. Recovering makes the
				// failure legible.
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("round %d: Subscribe panicked: %v", round, r)
						}
					}()
					_, _ = p.Subscribe(fakeTrack("track-a", "other"))
				}()
			}
		}()
		go func() {
			defer wg.Done()
			p.Close()
		}()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Subscribe racing Close deadlocked", round)
		}
		if t.Failed() {
			return
		}
	}
}

// Many goroutines subscribing while the peer closes, which is the shape of a
// busy room emptying: the router fans out to every peer at once.
func TestConcurrentSubscribeAndCloseIsSafe(t *testing.T) {
	p := testPeer(t, "p1")

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Subscribe panicked: %v", r)
				}
			}()
			for j := 0; j < 20; j++ {
				_, _ = p.Subscribe(fakeTrack("t", "other"))
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		p.Close()
		p.Close() // and again, because two paths reach it
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent subscribe and close deadlocked")
	}
}

// A closed peer must acquire no further feeds. Registering one would leak a
// downstream and a sender that nothing will ever tear down.
func TestClosedPeerAcceptsNoNewFeeds(t *testing.T) {
	p := testPeer(t, "p1")
	p.Close()

	for i := 0; i < 10; i++ {
		d, err := p.Subscribe(fakeTrack("track-x", "other"))
		if err != nil {
			t.Fatalf("Subscribe after close returned an error: %v", err)
		}
		if d != nil {
			t.Fatal("a closed peer accepted a new feed")
		}
	}
	if n := len(p.Downstreams()); n != 0 {
		t.Fatalf("a closed peer holds %d feeds", n)
	}
}

// Closing must be idempotent and must leave the peer usable as a no-op rather
// than a landmine. Shutdown reaches a peer from the room emptying and from the
// registry closing, and neither knows what the other did.
func TestPeerDoubleCloseIsSafe(t *testing.T) {
	p := testPeer(t, "p1")
	p.Close()
	p.Close()
	p.Close()

	// And every accessor must still be callable afterwards.
	_ = p.Downstreams()
	_ = p.Published()
	if _, err := p.Subscribe(fakeTrack("t", "other")); err != nil {
		t.Fatalf("Subscribe on a closed peer errored: %v", err)
	}
}

// Unsubscribing something that was never subscribed, or was already removed,
// must be a no-op rather than a panic.
func TestUnsubscribeIsForgiving(t *testing.T) {
	p := testPeer(t, "p1")
	p.Unsubscribe("never-existed")
	p.Close()
	p.Unsubscribe("never-existed")
	p.Unsubscribe("")
}
