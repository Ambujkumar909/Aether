package signal

import (
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

func (f *presenterFixture) setRole(from, id string, role wire.Role) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpSetRole, 0, false)
		w.Str(id)
		w.Bits(uint32(role), 2)
		w.Bits(0, 6)
	})
}

// Only a host changes roles; the change reaches everyone; the organizer's
// role cannot be changed.
func TestSetRoleThroughTheGateway(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c") // a hosts
	f.setRole("b", "c", wire.RoleModerator)
	if role, _ := f.rm.Role("c"); role == wire.RoleModerator {
		t.Fatal("a member made someone a presenter")
	}
	if got := f.drain("c", 300*time.Millisecond); count(got, wire.OpSetRole) != 0 {
		t.Fatalf("a refused change was broadcast: %+v", got)
	}
	f.setRole("a", "c", wire.RoleModerator)
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 400*time.Millisecond); count(got, wire.OpSetRole) != 1 {
			t.Fatalf("%s not told of the role change: %+v", id, got)
		}
	}
	f.setRole("c", "a", wire.RoleMember) // c is a presenter, not a host
	f.setRole("a", "a", wire.RoleMember) // the organizer
	if !f.rm.IsHost("a") {
		t.Fatal("the organizer lost hosting")
	}
}

// Remove: host only, never the organizer, and the removed person is told
// and disconnected.
func TestRemoveFromMeeting(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	f.send("b", func(w *wire.Writer) { w.WriteHeader(wire.OpRemove, 0, false); w.Str("c") })
	if r := f.conns["c"].CloseReason(); r != "" {
		t.Fatalf("a member removed someone (%q)", r)
	}
	f.rm.SetRole("b", wire.RoleHost) // a co-organizer
	f.send("b", func(w *wire.Writer) { w.WriteHeader(wire.OpRemove, 0, false); w.Str("a") })
	if r := f.conns["a"].CloseReason(); r != "" {
		t.Fatalf("the organizer was removed (%q)", r)
	}
	f.drain("c", 100*time.Millisecond)
	f.send("a", func(w *wire.Writer) { w.WriteHeader(wire.OpRemove, 0, false); w.Str("c") })
	if got := f.drain("c", 400*time.Millisecond); count(got, wire.OpRemove) != 1 {
		t.Fatalf("c was not told: %+v", got)
	}
	if r := f.conns["c"].CloseReason(); r != "removed" {
		t.Fatalf("c closed with %q", r)
	}
}

// Lower all hands: host only; each raised hand's owner is told; everyone
// gets the lowered state.
func TestLowerAllHands(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	f.parts["b"].State.HandRaised = true
	f.parts["c"].State.HandRaised = true
	f.send("b", func(w *wire.Writer) { w.WriteHeader(wire.OpLowerHands, 0, false) })
	if !f.parts["c"].State.HandRaised {
		t.Fatal("a member lowered all hands")
	}
	f.send("a", func(w *wire.Writer) { w.WriteHeader(wire.OpLowerHands, 0, false) })
	for _, id := range []string{"b", "c"} {
		got := f.drain(id, 400*time.Millisecond)
		if count(got, wire.OpLowerHands) != 1 {
			t.Fatalf("%s not asked to lower: %+v", id, got)
		}
	}
	got := f.drain("a", 400*time.Millisecond)
	if !has(got, wire.OpStateDelta, "b", "") || !has(got, wire.OpStateDelta, "c", "") || count(got, wire.OpLowerHands) != 0 {
		t.Fatalf("host's view not updated, or host asked to lower a hand it never raised: %+v", got)
	}
	if f.parts["b"].State.HandRaised || f.parts["c"].State.HandRaised {
		t.Fatal("hands still up on the server")
	}
}

// "Allow reactions" off: an attendee's reaction goes nowhere, a presenter's
// still reaches the room.
func TestReactionsControl(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	f.rm.SetPermissions(room.Permissions{Mic: true, Camera: true, Share: true, Chat: true, Reactions: false})
	react := func(id string) {
		f.send(id, func(w *wire.Writer) { w.WriteHeader(wire.OpReaction, 0, false); w.Str("\U0001F44D") })
	}
	react("c")
	if got := f.drain("a", 300*time.Millisecond); count(got, wire.OpReaction) != 0 {
		t.Fatalf("an attendee's reaction got through: %+v", got)
	}
	f.rm.SetRole("c", wire.RoleModerator)
	react("c")
	if got := f.drain("a", 300*time.Millisecond); count(got, wire.OpReaction) != 1 {
		t.Fatalf("a presenter's reaction was blocked: %+v", got)
	}
}

// "Disable mic" through the gateway: host only, announced to everyone, and
// the person's audio stops reaching the room at once; an arrival is told.
func TestBlockThroughGateway(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	block := func(from, id string, mic, cam bool) {
		f.send(from, func(w *wire.Writer) {
			w.WriteHeader(wire.OpBlock, 0, false)
			w.Str(id)
			w.Bool(mic)
			w.Bool(cam)
			w.Bits(0, 6)
		})
	}
	audio := func(id string) {
		f.send(id, func(w *wire.Writer) {
			w.WriteHeader(wire.OpMediaFrame, 0, false)
			w.Bits(0, 1) // audio
			w.Bits(0, 1)
			w.Bits(0, 6)
			w.Raw([]byte{1, 2, 3})
		})
	}
	block("b", "c", true, false)
	if !f.rm.May("c", "mic") {
		t.Fatal("a member disabled someone's mic")
	}
	block("a", "c", true, false)
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 400*time.Millisecond); count(got, wire.OpBlock) != 1 {
			t.Fatalf("%s not told: %+v", id, got)
		}
	}
	audio("c")
	if got := f.drain("a", 300*time.Millisecond); has(got, wire.OpMediaFrame, "c", "") {
		t.Fatal("a disabled mic still reached the room")
	}
	audio("b")
	if got := f.drain("a", 300*time.Millisecond); !has(got, wire.OpMediaFrame, "b", "") {
		t.Fatal("an attendee who was not disabled was silenced")
	}
}
