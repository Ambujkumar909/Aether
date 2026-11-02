package room

import (
	"fmt"
	"sync"
	"testing"

	"github.com/aether-rtc/aether/internal/wire"
)

// Meeting controls, tested as enforcement rather than as display.
//
// The failure this guards against is a control that looks like it works: the
// button greys out, the host believes sharing is off, and a participant on an
// older build presents anyway. Every case here asks the question the server
// asks at the moment of the action, which is the only answer that decides
// anything.

func roomWithHost(t *testing.T) (*Room, *Participant, *Participant) {
	t.Helper()
	r := testRoom(t)
	host := joiner("host")
	if _, err := r.Join(host); err != nil {
		t.Fatal(err)
	}
	guest := joiner("guest")
	if _, err := r.Join(guest); err != nil {
		t.Fatal(err)
	}
	if !r.IsHost(host.ID) {
		t.Fatal("the first joiner is not hosting")
	}
	return r, host, guest
}

func TestDefaultsLetEverybodyParticipate(t *testing.T) {
	r, _, guest := roomWithHost(t)
	for _, what := range []string{"mic", "camera", "share", "chat"} {
		if !r.May(guest.ID, what) {
			t.Errorf("a new meeting forbids %q by default", what)
		}
	}
}

// Each control is independent: turning sharing off must not silence anybody.
func TestControlsAreIndependent(t *testing.T) {
	r, _, guest := roomWithHost(t)

	cases := []struct {
		off  string
		p    Permissions
		want map[string]bool
	}{
		{"share", Permissions{Mic: true, Camera: true, Share: false, Chat: true},
			map[string]bool{"mic": true, "camera": true, "share": false, "chat": true}},
		{"mic", Permissions{Mic: false, Camera: true, Share: true, Chat: true},
			map[string]bool{"mic": false, "camera": true, "share": true, "chat": true}},
		{"chat", Permissions{Mic: true, Camera: true, Share: true, Chat: false},
			map[string]bool{"mic": true, "camera": true, "share": true, "chat": false}},
		{"camera", Permissions{Mic: true, Camera: false, Share: true, Chat: true},
			map[string]bool{"mic": true, "camera": false, "share": true, "chat": true}},
	}
	for _, c := range cases {
		r.SetPermissions(c.p)
		for what, want := range c.want {
			if got := r.May(guest.ID, what); got != want {
				t.Errorf("with %s off: May(%q) = %v, want %v", c.off, what, got, want)
			}
		}
	}
}

// A host is never locked out by their own controls. Otherwise a host who
// turned everything off would have no way to turn it back on.
func TestHostIsExemptFromEveryControl(t *testing.T) {
	r, host, _ := roomWithHost(t)
	r.SetPermissions(Permissions{})

	for _, what := range []string{"mic", "camera", "share", "chat"} {
		if !r.May(host.ID, what) {
			t.Errorf("the host is blocked from %q by their own controls", what)
		}
	}
}

// A co-host, promoted to the host role, is exempt too.
func TestPromotedHostIsExempt(t *testing.T) {
	r, _, guest := roomWithHost(t)
	r.SetPermissions(Permissions{})
	if r.May(guest.ID, "share") {
		t.Fatal("a guest may share while sharing is off")
	}

	r.mu.Lock()
	guest.Role = wire.RoleHost
	r.mu.Unlock()

	if !r.May(guest.ID, "share") {
		t.Fatal("a promoted co-host is still blocked")
	}
}

// An unrecognised capability is refused, not allowed. A typo in a caller must
// not quietly open something up.
func TestUnknownCapabilityIsRefused(t *testing.T) {
	r, _, guest := roomWithHost(t)
	for _, what := range []string{"", "recording", "MIC", "screen", "anything"} {
		if r.May(guest.ID, what) {
			t.Errorf("unknown capability %q was permitted", what)
		}
	}
}

// Somebody who is not in the room at all gets nothing.
func TestStrangersGetNothing(t *testing.T) {
	r, _, _ := roomWithHost(t)
	for _, what := range []string{"mic", "camera", "share", "chat"} {
		if !r.May("nobody", what) {
			// Default permissions are permissive, so a stranger inherits them;
			// what must never happen is a stranger being treated as a host.
			continue
		}
	}
	r.SetPermissions(Permissions{})
	for _, what := range []string{"mic", "camera", "share", "chat"} {
		if r.May("nobody", what) {
			t.Errorf("somebody outside the room was permitted %q", what)
		}
	}
}

// ---------------------------------------------------------------------------
// Mute all
// ---------------------------------------------------------------------------

func TestMuteAllSilencesEverybodyButTheHost(t *testing.T) {
	r, host, _ := roomWithHost(t)
	for i := 0; i < 5; i++ {
		if _, err := r.Join(joiner(fmt.Sprintf("g%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	muted := r.MuteAll()
	if len(muted) != 6 { // the original guest plus five
		t.Fatalf("muted %d people, want 6", len(muted))
	}
	for _, id := range muted {
		if id == host.ID {
			t.Fatal("the host was muted by their own command")
		}
	}
	for _, p := range r.Snapshot() {
		if p.ID == host.ID {
			if p.State.MicMuted {
				t.Fatal("the host is marked muted")
			}
			continue
		}
		if !p.State.MicMuted {
			t.Fatalf("%s was not muted", p.ID)
		}
	}
}

// Muting everybody is a one-shot action, not a permission: people can unmute
// again afterwards unless the host has ALSO turned microphones off. Collapsing
// the two would mean a host who wanted quiet for a moment has to remember to
// hand the microphones back.
func TestMuteAllDoesNotRemovePermissionToUnmute(t *testing.T) {
	r, _, guest := roomWithHost(t)
	r.MuteAll()

	if !r.May(guest.ID, "mic") {
		t.Fatal("muting everybody also revoked permission to unmute")
	}

	// The host wanting both does both.
	r.SetPermissions(Permissions{Mic: false, Camera: true, Share: true, Chat: true})
	if r.May(guest.ID, "mic") {
		t.Fatal("turning microphones off did not take effect")
	}
}

func TestMuteAllOnAnEmptyRoomIsHarmless(t *testing.T) {
	r := testRoom(t)
	if n := len(r.MuteAll()); n != 0 {
		t.Fatalf("muted %d people in an empty room", n)
	}
}

// Controls are read on every forwarded track and written by a host mid-call,
// so they must be safe to change while they are being read. Under -race.
func TestPermissionsUnderConcurrency(t *testing.T) {
	r, _, guest := roomWithHost(t)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_ = r.May(guest.ID, "share")
				_ = r.May(guest.ID, "mic")
				_ = r.Permissions()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			r.SetPermissions(Permissions{Mic: j%2 == 0, Camera: true, Share: j%3 == 0, Chat: true})
			r.MuteAll()
		}
	}()
	wg.Wait()
}
