package room

import (
	"testing"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/wire"
)

// LiveAccounts: everyone signed in and in a room, presenting if sharing
// anywhere; guests left out; an empty registry is an empty map.
func TestLiveAccountsNamesSignedInPeopleAndWhoIsSharing(t *testing.T) {
	rg := NewRegistry(ledger.NewMeter("t"))
	if n := len(rg.LiveAccounts()); n != 0 {
		t.Fatalf("empty registry: %d", n)
	}
	a := rg.GetOrCreate("room-a")
	b := rg.GetOrCreate("room-b")
	a.Join(&Participant{ID: "p1", UserID: "u-1"})
	a.Join(&Participant{ID: "p2", UserID: "u-2", State: wire.State{Sharing: true}})
	a.Join(&Participant{ID: "p3"})                                                  // a guest
	b.Join(&Participant{ID: "p4", UserID: "u-1", State: wire.State{Sharing: true}}) // u-1 also presenting from another device
	live := rg.LiveAccounts()
	if len(live) != 2 {
		t.Fatalf("live: %v", live)
	}
	if !live["u-2"] || !live["u-1"] {
		t.Fatalf("presenting flags: %v", live)
	}
	a.Leave("p2")
	b.Leave("p4")
	live = rg.LiveAccounts()
	if len(live) != 1 || live["u-1"] {
		t.Fatalf("after leaving: %v", live)
	}
}
