package room

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

func joiner(id string) *Participant {
	return &Participant{ID: id, DisplayName: id, Send: func([]byte) {}}
}

// Somebody must always be able to admit people.
//
// A waiting room with no host is a door nobody can open: everybody who
// arrives waits until the timeout and the meeting quietly becomes unjoinable.
// That failure is silent from the outside, which is what makes it worth
// pinning down rather than assuming.
func TestFirstJoinerHostsTheMeeting(t *testing.T) {
	r := testRoom(t)
	first := joiner("a")
	if _, err := r.Join(first); err != nil {
		t.Fatal(err)
	}
	if r.HostID() != "a" {
		t.Fatalf("host is %q after the first join", r.HostID())
	}
	if !r.IsHost("a") {
		t.Fatal("the first joiner cannot admit anybody")
	}
	if first.Role != wire.RoleHost {
		t.Fatalf("the first joiner has role %v", first.Role)
	}

	// The second is an ordinary participant, and must not be able to admit.
	second := joiner("b")
	if _, err := r.Join(second); err != nil {
		t.Fatal(err)
	}
	if r.IsHost("b") {
		t.Fatal("the second joiner can admit people")
	}
	if r.HostID() != "a" {
		t.Fatal("the second joiner took over hosting")
	}
}

// When the host leaves, hosting passes to whoever has been there longest.
func TestHostingPassesOnWhenTheHostLeaves(t *testing.T) {
	r := testRoom(t)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := r.Join(joiner(id)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct join times
	}
	if r.HostID() != "a" {
		t.Fatalf("host is %q", r.HostID())
	}

	if err := r.Leave("a"); err != nil {
		t.Fatal(err)
	}
	if r.HostID() != "b" {
		t.Fatalf("after the host left, host is %q, want the next longest present", r.HostID())
	}
	if !r.IsHost("b") {
		t.Fatal("the new host cannot admit anybody")
	}

	if err := r.Leave("b"); err != nil {
		t.Fatal(err)
	}
	if r.HostID() != "c" {
		t.Fatalf("host is %q after two departures", r.HostID())
	}

	// The last person out leaves no host, and that is correct: there is no
	// meeting left to host.
	if err := r.Leave("c"); err != nil {
		t.Fatal(err)
	}
	if r.HostID() != "" {
		t.Fatalf("an empty room still names %q as host", r.HostID())
	}
}

// A non-host leaving must not disturb hosting.
func TestOrdinaryDepartureLeavesHostingAlone(t *testing.T) {
	r := testRoom(t)
	_, _ = r.Join(joiner("a"))
	_, _ = r.Join(joiner("b"))

	if err := r.Leave("b"); err != nil {
		t.Fatal(err)
	}
	if r.HostID() != "a" {
		t.Fatalf("host changed to %q when an ordinary participant left", r.HostID())
	}
}

// A scheduled meeting names its organiser, overriding first-in.
func TestExplicitHostOverridesFirstJoiner(t *testing.T) {
	r := testRoom(t)
	_, _ = r.Join(joiner("early"))
	organiser := joiner("organiser")
	_, _ = r.Join(organiser)

	r.SetHost("organiser")
	if r.HostID() != "organiser" {
		t.Fatalf("host is %q after being set explicitly", r.HostID())
	}
	if !r.IsHost("organiser") {
		t.Fatal("the named organiser cannot admit anybody")
	}
	if organiser.Role != wire.RoleHost {
		t.Fatal("the named organiser was not given the host role")
	}
}

// Nobody outside the room is a host, whatever their account role. A
// deployment administrator who walks into a meeting is a participant.
func TestStrangersAreNeverHosts(t *testing.T) {
	r := testRoom(t)
	_, _ = r.Join(joiner("a"))

	for _, id := range []string{"", "b", "admin", "a "} {
		if r.IsHost(id) {
			t.Fatalf("%q is treated as a host", id)
		}
	}
}

// Hosting must stay coherent when everybody joins and leaves at once.
func TestHostingUnderConcurrency(t *testing.T) {
	r := testRoom(t)
	var wg sync.WaitGroup

	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				id := fmt.Sprintf("p%02d-%02d", n, j)
				if _, err := r.Join(joiner(id)); err != nil {
					continue
				}
				_ = r.HostID()
				_ = r.IsHost(id)
				_ = r.Leave(id)
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("hosting operations deadlocked")
	}

	// Whoever is left, if anybody, must be the host; and if nobody is left
	// there must be no host.
	people := r.Snapshot()
	host := r.HostID()
	if len(people) == 0 {
		if host != "" {
			t.Fatalf("empty room names %q as host", host)
		}
		return
	}
	if host == "" {
		t.Fatalf("%d people are in the room and nobody hosts; nobody could be admitted",
			len(people))
	}
	found := false
	for _, p := range people {
		if p.ID == host {
			found = true
		}
	}
	if !found {
		t.Fatalf("host %q is not in the room", host)
	}
}
