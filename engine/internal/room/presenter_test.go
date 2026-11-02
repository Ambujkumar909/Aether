package room

import (
	"sync"
	"testing"

	"github.com/aether-rtc/aether/internal/ledger"
)

func presenterRoom(t *testing.T, ids ...string) *Room {
	t.Helper()
	r := New("p", ledger.NewMeter("t"))
	for _, id := range ids {
		if _, err := r.Join(&Participant{ID: id, DisplayName: id, Send: func([]byte) {}}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestOnePresenterAtATime(t *testing.T) {
	r := presenterRoom(t, "host", "a", "b")
	if !r.May("a", "share") || !r.May("b", "share") {
		t.Fatal("with nobody presenting, anybody allowed to share may")
	}
	if got := r.ClaimPresenter("a"); got != "" {
		t.Fatalf("first claim replaced %q", got)
	}
	if !r.May("a", "share") || r.May("b", "share") || r.May("host", "share") {
		t.Fatal("while a presents, only a's screen may be forwarded -- the host included")
	}
	if got := r.ClaimPresenter("a"); got != "" {
		t.Fatalf("a claiming again replaced %q", got)
	}
	// b takes over.
	if got := r.ClaimPresenter("b"); got != "a" {
		t.Fatalf("b's claim replaced %q, want a", got)
	}
	if r.May("a", "share") || !r.May("b", "share") {
		t.Fatal("after the takeover only b may share")
	}
	// a stopping (late, after being replaced) does not end b's presentation.
	if r.ReleasePresenter("a") || r.Presenter() != "b" {
		t.Fatalf("a's release took the stage from b (presenter %q)", r.Presenter())
	}
	// b stops: the stage is free.
	if !r.ReleasePresenter("b") || r.Presenter() != "" || !r.May("a", "share") {
		t.Fatal("b's release did not free the stage")
	}
	// A presenter who leaves frees it too.
	r.ClaimPresenter("host")
	if err := r.Leave("host"); err != nil {
		t.Fatal(err)
	}
	if r.Presenter() != "" || !r.May("a", "share") {
		t.Fatalf("the presenter left and the stage is still held by %q", r.Presenter())
	}
	if r.ReleasePresenter("") {
		t.Fatal("releasing nobody reported success")
	}
}

// The permission still applies: attendees barred from sharing stay barred
// even when nobody is presenting.
func TestPresenterRuleDoesNotOpenAClosedShare(t *testing.T) {
	r := presenterRoom(t, "host", "a")
	p := r.Permissions()
	p.Share = false
	r.SetPermissions(p)
	if r.May("a", "share") {
		t.Fatal("an attendee barred from sharing may share")
	}
	r.ClaimPresenter("a")
	if r.May("a", "share") {
		t.Fatal("claiming the stage let a barred attendee share")
	}
}

// Everyone presses Share in the same instant: exactly one presenter
// results, every claim but the first replaced somebody, and nobody is
// replaced twice (a chain, not a tree).
func TestSimultaneousClaimsResolveToOnePresenter(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for round := range 200 {
		r := presenterRoom(t, ids...)
		var mu sync.Mutex
		replaced := map[string]int{}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				if prev := r.ClaimPresenter(id); prev != "" {
					mu.Lock()
					replaced[prev]++
					mu.Unlock()
				}
			}(id)
		}
		close(start)
		wg.Wait()
		p := r.Presenter()
		if p == "" {
			t.Fatal("no presenter after everyone claimed")
		}
		n := 0
		for who, k := range replaced {
			if k != 1 {
				t.Fatalf("round %d: %s replaced %d times", round, who, k)
			}
			if who == p {
				t.Fatalf("round %d: the final presenter %s was recorded as replaced", round, p)
			}
			n++
		}
		if n != len(ids)-1 {
			t.Fatalf("round %d: %d replacements for %d claims", round, n, len(ids))
		}
		sharers := 0
		for _, id := range ids {
			if r.May(id, "share") {
				sharers++
			}
		}
		if sharers != 1 {
			t.Fatalf("round %d: %d participants may share", round, sharers)
		}
	}
}
