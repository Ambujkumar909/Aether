package room

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/wire"
)

func cfgOf(names []string, members [][]string) BreakoutConfig {
	return BreakoutConfig{Names: names, Members: members, AllowReturn: true, AutoMove: true}
}

// Teams' limits, each one over the line: 50 rooms, 35-character names; and
// the people: present, not a host ("managers can't be assigned"), once.
func TestBreakoutLimits(t *testing.T) {
	r := spotRoom(t, 4) // p0 hosts
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "Room"
		}
		return out
	}
	if err := r.SetBreakouts(cfgOf(nil, nil), "n"); !errors.Is(err, ErrBreakoutCount) {
		t.Fatalf("zero rooms: %v", err)
	}
	if err := r.SetBreakouts(cfgOf(names(MaxBreakoutRooms+1), make([][]string, MaxBreakoutRooms+1)), "n"); !errors.Is(err, ErrBreakoutCount) {
		t.Fatalf("51 rooms: %v", err)
	}
	if err := r.SetBreakouts(cfgOf(names(MaxBreakoutRooms), make([][]string, MaxBreakoutRooms)), "n"); err != nil {
		t.Fatalf("50 rooms refused: %v", err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxBreakoutNameLen+1)} {
		if err := r.SetBreakouts(cfgOf([]string{bad}, [][]string{nil}), "n"); !errors.Is(err, ErrBreakoutName) {
			t.Fatalf("name %q: %v", bad, err)
		}
	}
	if err := r.SetBreakouts(cfgOf([]string{strings.Repeat("é", MaxBreakoutNameLen)}, [][]string{nil}), "n"); err != nil {
		t.Fatalf("35 characters (70 bytes) refused: %v", err)
	}
	for _, members := range [][]string{{"ghost"}, {"p0"}, {"p1", "p1"}} {
		if err := r.SetBreakouts(cfgOf([]string{"A"}, [][]string{members}), "n"); !errors.Is(err, ErrBreakoutMember) {
			t.Fatalf("members %v: %v", members, err)
		}
	}
	r.SetRole("p3", wire.RoleHost)
	if err := r.SetBreakouts(cfgOf([]string{"A"}, [][]string{{"p3"}}), "n"); !errors.Is(err, ErrBreakoutMember) {
		t.Fatalf("a co-organizer was assigned: %v", err)
	}
	if err := r.SetBreakouts(BreakoutConfig{Names: []string{"A"}, Members: [][]string{nil}, TimeLimit: MaxBreakoutTimeLimit + time.Second}, "n"); !errors.Is(err, ErrBreakoutTimeLimit) {
		t.Fatalf("time limit: %v", err)
	}
}

// Set, open, close, open again, delete: and the ids name their meeting.
func TestBreakoutLifecycle(t *testing.T) {
	r := spotRoom(t, 3)
	if _, err := r.OpenBreakouts(time.Now(), nil); !errors.Is(err, ErrBreakoutsNone) {
		t.Fatalf("open with none: %v", err)
	}
	cfg := BreakoutConfig{Names: []string{" Room 1 ", "Room 2"}, Members: [][]string{{"p1"}, {"p2"}}, TimeLimit: 10 * time.Minute}
	if err := r.SetBreakouts(cfg, "nonce"); err != nil {
		t.Fatal(err)
	}
	st, _ := r.BreakoutState()
	if st.Rooms[0].Name != "Room 1" || st.Rooms[0].ID != "r~br~nonce~1" || BreakoutParent(st.Rooms[1].ID) != "r" {
		t.Fatalf("rooms %+v", st.Rooms)
	}
	if BreakoutParent("plain") != "" || BreakoutParent("~br~x") != "" {
		t.Fatal("a plain id read as a breakout room")
	}
	now := time.Now()
	open, err := r.OpenBreakouts(now, map[string]bool{"u-org": true})
	if err != nil || !open.ClosesAt.Equal(now.Add(10*time.Minute)) || !open.Managers["u-org"] {
		t.Fatalf("open: %+v %v", open, err)
	}
	if _, err := r.OpenBreakouts(now, nil); !errors.Is(err, ErrBreakoutsOpen) {
		t.Fatalf("opened twice: %v", err)
	}
	if err := r.SetBreakouts(cfg, "x"); !errors.Is(err, ErrBreakoutsOpen) {
		t.Fatalf("reconfigured while open: %v", err)
	}
	if err := r.DeleteBreakouts(); !errors.Is(err, ErrBreakoutsOpen) {
		t.Fatalf("deleted while open: %v", err)
	}
	// The copy handed out is not the room's own.
	open.Rooms[0].Name = "changed"
	open.Managers["intruder"] = true
	if st, _ := r.BreakoutState(); st.Rooms[0].Name == "changed" || st.Managers["intruder"] {
		t.Fatal("caller's copy mutated the room's state")
	}
	if _, ok := r.CloseBreakouts(); !ok || r.BreakoutsOpen() {
		t.Fatal("close")
	}
	if _, ok := r.CloseBreakouts(); ok {
		t.Fatal("closed twice")
	}
	if _, err := r.OpenBreakouts(time.Now(), nil); err != nil {
		t.Fatalf("rooms kept after close cannot reopen: %v", err)
	}
	r.CloseBreakouts()
	if err := r.DeleteBreakouts(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.BreakoutState(); ok {
		t.Fatal("deleted rooms still there")
	}
}

// A meeting whose people are all in rooms is not reaped; End forgets rooms.
func TestBreakoutReapAndEnd(t *testing.T) {
	rg := NewRegistry(ledger.NewMeter("t"))
	r := rg.GetOrCreate("m")
	r.Created = time.Now().Add(-time.Hour)
	if _, err := r.Join(&Participant{ID: "p0"}); err != nil {
		t.Fatal(err)
	}
	r.Join(&Participant{ID: "p1"})
	r.SetBreakouts(cfgOf([]string{"A"}, [][]string{{"p1"}}), "n")
	r.OpenBreakouts(time.Now(), nil)
	r.Leave("p0")
	r.Leave("p1")
	if n := rg.Reap(time.Minute); n != 0 {
		t.Fatal("reaped a meeting whose rooms are open")
	}
	r.CloseBreakouts()
	if n := rg.Reap(time.Minute); n != 1 {
		t.Fatal("an empty meeting with closed rooms was kept")
	}

	r2 := spotRoom(t, 2)
	r2.SetBreakouts(cfgOf([]string{"A"}, [][]string{{"p1"}}), "n")
	r2.OpenBreakouts(time.Now(), nil)
	r2.End()
	if _, ok := r2.BreakoutState(); ok || r2.BreakoutsOpen() {
		t.Fatal("End left the rooms")
	}
}
