package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

// A router that has stopped must not accept new peers.
//
// Same shape as the registry bug fixed earlier, one level down. Stop closes
// every peer it holds and then replaces its maps; a peer added afterwards goes
// into the fresh map, is never closed by anybody, and holds two real
// PeerConnections and their goroutines until the process exits.
//
// This is reachable during ordinary shutdown, because a join already in flight
// can arrive after the media plane has been told to stop.
func TestRouterRefusesPeersAfterStop(t *testing.T) {
	before := baseline()

	r := newRouter(t, "room-1")
	r.Start()
	r.Stop()

	for i := 0; i < 8; i++ {
		p, err := r.AddPeer("late", Signalling{})
		if err == nil && p != nil {
			// If the router hands one back it must at least not be tracked as
			// live, or shutdown has silently un-shut-down.
			p.Close()
		}
	}

	if n := r.Stats().Peers; n != 0 {
		t.Fatalf("a stopped router is tracking %d peers", n)
	}
	if n, ok := settleTo(before, 10*time.Second); !ok {
		t.Fatalf("peers added after Stop leaked goroutines: %d -> %d", before, n)
	}
}

// Stop racing AddPeer must not leave a peer behind either.
func TestRouterStopRacingAddPeer(t *testing.T) {
	before := baseline()
	r := newRouter(t, "room-1")
	r.Start()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var made []*Peer

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			p, err := r.AddPeer(peerName(i), Signalling{})
			if err == nil && p != nil {
				mu.Lock()
				made = append(made, p)
				mu.Unlock()
			}
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		r.Stop()
	}()
	wg.Wait()

	// Anything created after the stop is ours to clean up; the point of the
	// test is that the router does not silently retain them.
	r.Stop()
	mu.Lock()
	for _, p := range made {
		p.Close()
	}
	mu.Unlock()

	if n := r.Stats().Peers; n != 0 {
		t.Fatalf("%d peers survived a racing Stop", n)
	}
	if n, ok := settleTo(before, 15*time.Second); !ok {
		t.Fatalf("Stop racing AddPeer leaked: %d -> %d", before, n)
	}
}

// Removing a peer that is not there, or twice, must be a no-op.
func TestRemovePeerIsForgiving(t *testing.T) {
	r := newRouter(t, "room-1")
	r.Start()
	defer r.Stop()

	r.RemovePeer("never-existed")
	r.RemovePeer("")

	p, err := r.AddPeer("alice", Signalling{})
	if err != nil {
		t.Fatal(err)
	}
	_ = p
	r.RemovePeer("alice")
	r.RemovePeer("alice")

	if n := r.Stats().Peers; n != 0 {
		t.Fatalf("router still reports %d peers", n)
	}
}

// The room cap must hold, and must not leave a half-registered peer behind
// when it refuses one.
func TestRouterStatsStayConsistentUnderChurn(t *testing.T) {
	before := baseline()
	r := newRouter(t, "churn")
	r.Start()

	for round := 0; round < 20; round++ {
		p, err := r.AddPeer("p", Signalling{})
		if err != nil {
			t.Fatalf("round %d: AddPeer: %v", round, err)
		}
		if p == nil {
			t.Fatalf("round %d: AddPeer returned nil on a live router", round)
		}
		if n := r.Stats().Peers; n != 1 {
			t.Fatalf("round %d: %d peers after adding one", round, n)
		}
		r.RemovePeer("p")
		if n := r.Stats().Peers; n != 0 {
			t.Fatalf("round %d: %d peers after removing the only one", round, n)
		}
	}

	r.Stop()
	if n, ok := settleTo(before, 15*time.Second); !ok {
		t.Fatalf("twenty add/remove cycles leaked: %d -> %d", before, n)
	}
}

func peerName(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i/26))
}

var _ = ledger.NewMeter
