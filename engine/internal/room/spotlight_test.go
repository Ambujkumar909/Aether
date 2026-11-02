package room

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

func spotRoom(t *testing.T, n int) *Room {
	t.Helper()
	r := New("r", ledger.NewMeter("t"))
	for i := range n {
		if _, err := r.Join(&Participant{ID: fmt.Sprint("p", i)}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// Seven fit, the eighth is refused with the error the UI words as "No more
// people can be spotlighted", order is kept, and nobody absent can be added.
func TestSpotlightLimitOrderAndMembership(t *testing.T) {
	r := spotRoom(t, MaxSpotlights+1)
	for i := range MaxSpotlights {
		if ch, err := r.Spotlight(fmt.Sprint("p", i)); err != nil || !ch {
			t.Fatalf("spotlight p%d: changed=%v err=%v", i, ch, err)
		}
	}
	if _, err := r.Spotlight(fmt.Sprint("p", MaxSpotlights)); !errors.Is(err, ErrSpotlightFull) {
		t.Fatalf("eighth spotlight: err=%v, want ErrSpotlightFull", err)
	}
	if got := r.Spotlights(); len(got) != MaxSpotlights || got[0] != "p0" || got[6] != "p6" {
		t.Fatalf("list %v", got)
	}
	// Twice is not a change and does not use a slot.
	if ch, err := r.Spotlight("p3"); ch || err != nil {
		t.Fatalf("re-spotlight: changed=%v err=%v", ch, err)
	}
	if _, err := r.Spotlight("stranger"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger: err=%v", err)
	}
	// Freeing one slot lets the eighth in.
	if !r.Unspotlight("p2") || r.Unspotlight("p2") {
		t.Fatal("unspotlight should change once")
	}
	if ch, _ := r.Spotlight("p7"); !ch {
		t.Fatal("slot freed but p7 refused")
	}
	if got := r.Spotlights(); slices.Contains(got, "p2") || got[len(got)-1] != "p7" {
		t.Fatalf("list %v", got)
	}
}

// Leaving takes you off the stage: otherwise the slot and the stage are held
// by somebody who is not there.
func TestLeavingClearsSpotlight(t *testing.T) {
	r := spotRoom(t, 3)
	r.Spotlight("p1")
	_ = r.Leave("p1")
	if r.IsSpotlit("p1") || len(r.Spotlights()) != 0 {
		t.Fatalf("absent participant still spotlit: %v", r.Spotlights())
	}
}

// The returned slice is a copy: a caller ranging over it while another
// goroutine changes the list must not race (run under -race).
func TestSpotlightsConcurrent(t *testing.T) {
	r := spotRoom(t, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() { defer wg.Done(); r.Spotlight(fmt.Sprint("p", i)); r.Unspotlight(fmt.Sprint("p", i)) }()
		go func() {
			defer wg.Done()
			for _, id := range r.Spotlights() {
				_ = id
			}
		}()
	}
	wg.Wait()
	if n := len(r.Spotlights()); n != 0 {
		t.Fatalf("%d left after every add was undone", n)
	}
}

// End returns everybody, tells every knock "ended", and resets what a fresh
// meeting on the same link must not inherit.
func TestEndResetsTheMeeting(t *testing.T) {
	r := spotRoom(t, 3)
	r.Spotlight("p1")
	r.SetLocked(true)
	r.SetPermissions(Permissions{})
	w := &Waiter{ID: "w"}
	r.mu.Lock() // knock directly: Knock refuses while locked, which is the point of a lock
	w.decision = make(chan WaitState, 1)
	r.waiting["w"] = w
	r.mu.Unlock()

	ids := r.End()
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"p0", "p1", "p2"}) {
		t.Fatalf("End returned %v", ids)
	}
	if got := w.Wait(make(chan struct{}), time.Second); got != WaitEnded {
		t.Fatalf("waiter told %q, want ended", got)
	}
	if r.Locked() || len(r.Spotlights()) != 0 || r.Permissions() != DefaultPermissions() || r.WaitingCount() != 0 {
		t.Fatalf("not reset: locked=%v spot=%v perms=%+v waiting=%d",
			r.Locked(), r.Spotlights(), r.Permissions(), r.WaitingCount())
	}
	// Ending an already ended (or never started) meeting is harmless.
	r.End()
	New("x", ledger.NewMeter("t")).End()
}
