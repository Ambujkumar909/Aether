package sfu

import (
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func liveGoroutines() int {
	for i := 0; i < 50; i++ {
		runtime.Gosched()
	}
	return runtime.NumGoroutine()
}

// baseline waits for the goroutine count to stop moving before recording it.
//
// Sampling once is not good enough here: the media tests in this package leave
// WebRTC goroutines winding down for a while after they finish, so a baseline
// taken immediately is inflated and every leak assertion measured against it
// is vacuous.
func baseline() int {
	last := -1
	for i := 0; i < 200; i++ {
		n := liveGoroutines()
		if n == last {
			return n
		}
		last = n
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// waitForIncrease waits until the goroutine count rises above from.
func waitForIncrease(from int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if liveGoroutines() > from {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// settleTo waits for the goroutine count to come back down, and reports what
// it actually reached.
func settleTo(target int, within time.Duration) (int, bool) {
	deadline := time.Now().Add(within)
	for {
		n := liveGoroutines()
		if n <= target {
			return n, true
		}
		if time.Now().After(deadline) {
			return n, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newRouter(t *testing.T, id string) *Router {
	t.Helper()
	return NewRouter(id, ledger.NewMeter("t"), quiet(), nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// Stopping a router must end its allocation loop. The loop runs for the life
// of a room and recomputes every subscriber's layer once a second, so one
// left running is a permanent CPU cost and a permanent reference to every
// peer it holds.
func TestRouterStopEndsAllocationLoop(t *testing.T) {
	before := baseline()

	r := newRouter(t, "room-1")
	r.Start()
	if !waitForIncrease(before, 3*time.Second) {
		t.Fatalf("Start did not launch the allocation loop (still %d)", before)
	}

	r.Stop()
	if n, ok := settleTo(before, 3*time.Second); !ok {
		t.Fatalf("allocation loop outlived Stop: %d goroutines before, %d after", before, n)
	}
}

// Stop must be safe more than once. Shutdown reaches a router from two
// directions -- the registry closing and a room emptying -- and neither knows
// what the other did.
func TestRouterDoubleStopIsSafe(t *testing.T) {
	before := baseline()
	r := newRouter(t, "room-1")
	r.Start()

	r.Stop()
	r.Stop()
	r.Stop()

	if n, ok := settleTo(before, 3*time.Second); !ok {
		t.Fatalf("goroutines left after repeated Stop: %d -> %d", before, n)
	}
}

// Stopping a router that was never started must not panic or hang. A room
// that fails during setup is torn down without ever having run.
func TestStopWithoutStartIsSafe(t *testing.T) {
	r := newRouter(t, "room-1")
	done := make(chan struct{})
	go func() { defer close(done); r.Stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on a router that was never started")
	}
}

// Concurrent Stop must not deadlock or race. Run under -race.
func TestConcurrentRouterStopIsSafe(t *testing.T) {
	before := baseline()
	r := newRouter(t, "room-1")
	r.Start()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Router.Stop deadlocked")
	}
	if n, ok := settleTo(before, 3*time.Second); !ok {
		t.Fatalf("goroutines left after concurrent Stop: %d -> %d", before, n)
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Closing the registry must stop every router it holds.
func TestRegistryCloseStopsEveryRouter(t *testing.T) {
	before := baseline()
	rg := NewRegistry(ledger.NewMeter("t"), quiet(), nil)

	for _, id := range []string{"a", "b", "c", "d"} {
		rg.GetOrCreate(id)
	}
	if !waitForIncrease(before, 3*time.Second) {
		t.Fatal("creating rooms launched no allocation loops")
	}

	rg.Close()
	if n, ok := settleTo(before, 5*time.Second); !ok {
		t.Fatalf("allocation loops outlived Registry.Close: %d -> %d", before, n)
	}
}

func TestRegistryDoubleCloseIsSafe(t *testing.T) {
	before := baseline()
	rg := NewRegistry(ledger.NewMeter("t"), quiet(), nil)
	rg.GetOrCreate("a")

	rg.Close()
	rg.Close()

	if n, ok := settleTo(before, 5*time.Second); !ok {
		t.Fatalf("goroutines left after repeated Close: %d -> %d", before, n)
	}
}

// A room created after the registry has closed must not start a loop that
// nothing will ever stop.
//
// This is reachable during ordinary shutdown: the media module stops before
// the HTTP server has finished draining, so a request already in flight can
// call GetOrCreate afterwards. Left unguarded, every such request starts a
// permanent allocation loop on a process that is trying to exit, and the
// registry no longer holds a reference to stop it.
func TestRegistryRefusesNewRoutersAfterClose(t *testing.T) {
	before := baseline()
	rg := NewRegistry(ledger.NewMeter("t"), quiet(), nil)
	rg.Close()

	for i := 0; i < 8; i++ {
		rg.GetOrCreate("late-room")
	}

	if n, ok := settleTo(before, 3*time.Second); !ok {
		t.Fatalf("creating rooms after Close leaked %d goroutines (%d -> %d); "+
			"these have no owner and run until the process dies", n-before, before, n)
	}
}

// Close racing GetOrCreate must not leak either. Run under -race.
func TestRegistryCloseRacingCreateIsSafe(t *testing.T) {
	before := baseline()
	rg := NewRegistry(ledger.NewMeter("t"), quiet(), nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rg.GetOrCreate("room-racing")
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		rg.Close()
	}()
	wg.Wait()
	rg.Close() // sweep anything created between the two

	if n, ok := settleTo(before, 5*time.Second); !ok {
		t.Fatalf("Close racing GetOrCreate leaked: %d -> %d", before, n)
	}
}
