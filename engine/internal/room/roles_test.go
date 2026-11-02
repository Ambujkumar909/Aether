package room

import (
	"errors"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"

	"github.com/aether-rtc/aether/internal/wire"
)

// The organizer cannot be re-roled; only attendee, presenter and
// co-organizer can be given; a change is reported once.
func TestSetRoleRules(t *testing.T) {
	r := spotRoom(t, 3) // p0 hosts (first in)
	if _, err := r.SetRole("p0", wire.RoleMember); !errors.Is(err, ErrOrganizer) {
		t.Fatalf("organizer re-roled: %v", err)
	}
	if _, err := r.SetRole("p1", wire.RoleGuest); !errors.Is(err, ErrNotPermittedRole) {
		t.Fatalf("guest role given: %v", err)
	}
	if _, err := r.SetRole("ghost", wire.RoleModerator); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent person re-roled: %v", err)
	}
	if ch, err := r.SetRole("p1", wire.RoleModerator); !ch || err != nil {
		t.Fatalf("make presenter: %v %v", ch, err)
	}
	if ch, _ := r.SetRole("p1", wire.RoleModerator); ch {
		t.Fatal("same role reported as a change")
	}
	if role, _ := r.Role("p1"); role != wire.RoleModerator {
		t.Fatalf("role %v", role)
	}
	// A co-organizer hosts.
	r.SetRole("p2", wire.RoleHost)
	if !r.IsHost("p2") {
		t.Fatal("co-organizer is not a host")
	}
}

// The attendee controls bind attendees only: a presenter may unmute, share,
// show video, chat and react with every control off; an attendee may not.
func TestPresentersAreExemptFromAttendeeControls(t *testing.T) {
	r := spotRoom(t, 3)
	r.SetPermissions(Permissions{})
	r.SetRole("p1", wire.RoleModerator)
	for _, what := range []string{"mic", "camera", "share", "chat", "reactions"} {
		if !r.May("p1", what) {
			t.Errorf("presenter refused %s", what)
		}
		if r.May("p2", what) {
			t.Errorf("attendee allowed %s with it off", what)
		}
	}
	// Reactions are their own control now, not chat's.
	r.SetPermissions(Permissions{Chat: false, Reactions: true})
	if !r.May("p2", "reactions") || r.May("p2", "chat") {
		t.Fatal("reactions still tied to chat")
	}
	// Make an attendee again: bound again.
	r.SetRole("p1", wire.RoleMember)
	if r.May("p1", "mic") {
		t.Fatal("demoted presenter still exempt")
	}
}

func TestRemovable(t *testing.T) {
	r := spotRoom(t, 3)
	if err := r.Removable("p0", "p0"); err == nil {
		t.Fatal("host may remove themselves")
	}
	r.SetRole("p1", wire.RoleHost)
	if err := r.Removable("p1", "p0"); err == nil {
		t.Fatal("a co-organizer may remove the organizer")
	}
	if err := r.Removable("p0", "p2"); err != nil {
		t.Fatalf("host cannot remove an attendee: %v", err)
	}
	if err := r.Removable("p0", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
}

func TestLowerHands(t *testing.T) {
	r := spotRoom(t, 3)
	for _, p := range r.Snapshot() {
		if p.ID != "p0" {
			p.State.HandRaised = true
		}
	}
	got := r.LowerHands()
	if len(got) != 2 {
		t.Fatalf("lowered %d, want 2", len(got))
	}
	for _, s := range got {
		if s.HandRaised || s.Participant == "" {
			t.Fatalf("bad state %+v", s)
		}
	}
	for _, p := range r.Snapshot() {
		if p.State.HandRaised {
			t.Fatalf("%s still raised", p.ID)
		}
	}
	if len(r.LowerHands()) != 0 {
		t.Fatal("lowering twice reported hands")
	}
}

// The host leaving hands over to the earliest remaining participant.
func TestHostHandoverOnLeave(t *testing.T) {
	r := spotRoom(t, 3)
	_ = r.Leave("p0")
	if h := r.HostID(); h != "p1" {
		t.Fatalf("host after leave %q", h)
	}
	if role, _ := r.Role("p1"); role != wire.RoleHost {
		t.Fatalf("new host's role %v", role)
	}
}

// The attendance list goes to the organizer and co-organizers present now,
// by account; the session is the time since the room last became occupied.
func TestHostSession(t *testing.T) {
	r := New("r", ledger.NewMeter("t"))
	join := func(id, user string) {
		if _, err := r.Join(&Participant{ID: id, UserID: user}); err != nil {
			t.Fatal(err)
		}
	}
	join("p0", "u-org")
	first, ok := r.HostSession("u-org")
	if !ok || first.IsZero() {
		t.Fatal("the organizer is not a host of the session")
	}
	join("p1", "u-co")
	join("p2", "u-pres")
	r.SetRole("p1", wire.RoleHost)
	r.SetRole("p2", wire.RoleModerator)
	if _, ok := r.HostSession("u-co"); !ok {
		t.Fatal("a co-organizer may not export")
	}
	for _, u := range []string{"u-pres", "u-nobody", ""} {
		if _, ok := r.HostSession(u); ok {
			t.Fatalf("%q may export", u)
		}
	}
	// Everyone leaves; the next arrival starts a new session.
	for _, id := range []string{"p0", "p1", "p2"} {
		_ = r.Leave(id)
	}
	time.Sleep(2 * time.Millisecond)
	join("p3", "u-org")
	second, _ := r.HostSession("u-org")
	if !second.After(first) {
		t.Fatalf("the session did not restart: %v then %v", first, second)
	}
}

// "Disable mic / camera": attendees only; each enforced on its own; kept
// across a rejoin by account; lifted by making them a presenter; gone at End.
func TestBlockedAttendee(t *testing.T) {
	r := New("r", ledger.NewMeter("t"))
	join := func(id, user string) {
		if _, err := r.Join(&Participant{ID: id, UserID: user}); err != nil {
			t.Fatal(err)
		}
	}
	join("p0", "u-org")
	join("p1", "u-att")
	join("p2", "u-pres")
	r.SetRole("p1", wire.RoleMember)
	r.SetRole("p2", wire.RoleModerator)
	if _, err := r.SetBlocked("p0", true, true); !errors.Is(err, ErrNotAttendee) {
		t.Fatalf("the organizer was disabled: %v", err)
	}
	if _, err := r.SetBlocked("p2", true, true); !errors.Is(err, ErrNotAttendee) {
		t.Fatalf("a presenter was disabled: %v", err)
	}
	if ch, err := r.SetBlocked("p1", true, false); !ch || err != nil {
		t.Fatalf("disable mic: %v %v", ch, err)
	}
	if r.May("p1", "mic") || !r.May("p1", "camera") || !r.May("p1", "chat") {
		t.Fatal("mic block not enforced on its own")
	}
	if ch, _ := r.SetBlocked("p1", true, false); ch {
		t.Fatal("the same block reported as a change")
	}
	if l := r.BlockedList(); len(l) != 1 || l[0].ID != "p1" || !l[0].Mic || l[0].Cam {
		t.Fatalf("list %+v", l)
	}
	// Leave and come back under a new participant id: still disabled.
	_ = r.Leave("p1")
	join("p1b", "u-att")
	if r.May("p1b", "mic") {
		t.Fatal("a rejoin lifted the block")
	}
	// Made a presenter: lifted, and not re-applied on the next rejoin.
	r.SetRole("p1b", wire.RoleModerator)
	r.SetRole("p1b", wire.RoleMember)
	if !r.May("p1b", "mic") {
		t.Fatal("making them a presenter did not lift the block")
	}
	_ = r.Leave("p1b")
	join("p1c", "u-att")
	if !r.May("p1c", "mic") {
		t.Fatal("a block lifted by a promotion came back on the next rejoin")
	}
	r.SetBlocked("p1c", false, true)
	r.End()
	join("p9", "u-att")
	if !r.May("p9", "camera") {
		t.Fatal("a block outlived the meeting")
	}
}
