package room

import (
	"testing"

	"github.com/aether-rtc/aether/internal/wire"
)

// The host and the lock they hold.
//
// Reported exactly like this: the host locked the meeting, reloaded their
// tab, and got "Meeting locked -- Leave" from their own meeting. The room knew
// its host by participant ID, which is minted per connection, so the person
// coming back through the door was a stranger to it. A lock has to know who
// holds the key, and the key is the account.

func signedIn(id, user string) *Participant {
	p := joiner(id)
	p.UserID = user
	return p
}

// The regression, as the room sees it. Against the old code IsHostAccount did
// not exist; the closest behaviour was "is this participant ID the host", and
// a fresh ID never is.
func TestHostIsRecognisedByAccountAcrossReconnect(t *testing.T) {
	r := testRoom(t)
	if _, err := r.Join(signedIn("conn-1", "alice")); err != nil {
		t.Fatal(err)
	}
	if !r.IsHostAccount("alice") {
		t.Fatal("the first joiner's account is not recorded as host")
	}

	// Reload: the connection goes, and with it the participant ID.
	if err := r.Leave("conn-1"); err != nil {
		t.Fatal(err)
	}
	if r.IsHost("conn-2") {
		t.Fatal("a brand-new participant ID is somehow already host")
	}
	// The account still holds the key, with nobody connected at all.
	if !r.IsHostAccount("alice") {
		t.Fatal("the host's account was forgotten when they disconnected")
	}
}

// The lock is what makes this matter. Locking refuses knocks; the host's
// account must not be subject to that.
func TestLockedMeetingStillKnowsItsHost(t *testing.T) {
	r := testRoom(t)
	if _, err := r.Join(signedIn("host-conn", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(signedIn("guest-conn", "bob")); err != nil {
		t.Fatal(err)
	}
	r.SetLocked(true)

	// Host drops; Bob is handed hosting so the room is not headless...
	if err := r.Leave("host-conn"); err != nil {
		t.Fatal(err)
	}
	// ...which is correct while Alice is away: the handoff moves the account.
	if !r.IsHostAccount("bob") {
		t.Fatal("hosting was handed to bob's connection but not his account")
	}
	if r.IsHostAccount("alice") {
		t.Fatal("alice still holds the key to a meeting bob now runs")
	}
	// A stranger's account holds nothing.
	if r.IsHostAccount("mallory") {
		t.Fatal("an account that never hosted is recognised as host")
	}
	// Guests have no durable identity to recognise.
	if r.IsHostAccount("") {
		t.Fatal("the empty account is recognised as host")
	}
}

// A guest hosting an ad-hoc meeting has nothing durable about them. If they
// drop out of a locked meeting they are locked out like anybody else -- and,
// importantly, a stale account key from an earlier host is kept rather than
// cleared, because a locked room with NO recognisable host account can never
// be re-entered by anyone.
func TestGuestSuccessorDoesNotEraseTheKey(t *testing.T) {
	r := testRoom(t)
	if _, err := r.Join(signedIn("a", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(joiner("g")); err != nil { // guest: no UserID
		t.Fatal(err)
	}
	if err := r.Leave("a"); err != nil {
		t.Fatal(err)
	}
	if !r.IsHost("g") {
		t.Fatal("the guest was not handed hosting")
	}
	if !r.IsHostAccount("alice") {
		t.Fatal("handing hosting to a guest erased the only account that can reopen a lock")
	}
}

// SetHost -- the scheduled-organiser path -- records the account as well.
func TestSetHostRecordsTheAccount(t *testing.T) {
	r := testRoom(t)
	if _, err := r.Join(signedIn("first", "bob")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(signedIn("org", "alice")); err != nil {
		t.Fatal(err)
	}
	r.SetHost("org")
	// HostID is the primary host. IsHost is broader -- it is also true for a
	// co-host, and the first joiner keeps the host ROLE after handing over,
	// which is deliberate -- so the primary is what is checked here.
	if r.HostID() != "org" {
		t.Fatalf("SetHost did not move hosting: primary is %q", r.HostID())
	}
	if !r.IsHostAccount("alice") || r.IsHostAccount("bob") {
		t.Fatal("SetHost moved the participant but not the account")
	}
	if p, _ := r.Get("org"); p.Role != wire.RoleHost {
		t.Fatal("the organiser was not given the host role")
	}
}
