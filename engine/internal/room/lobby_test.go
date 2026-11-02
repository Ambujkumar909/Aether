package room

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

func testRoom(t *testing.T) *Room {
	t.Helper()
	return New("r1", ledger.NewMeter("t"))
}

func waiter(id string, authed bool) *Waiter {
	return &Waiter{ID: id, DisplayName: id, Authenticated: authed, UserID: id}
}

// ---------------------------------------------------------------------------
// The property the whole feature rests on
// ---------------------------------------------------------------------------

// Somebody waiting is not in the meeting, in every sense the system has.
//
// This is the test that decides whether the lobby is a security control or a
// label. A waiting room that hides a tile while the connection is already in
// the room is worse than none, because it tells the host something untrue: the
// person is there, hearing everything, and the interface says they are not.
func TestAWaiterIsNotInTheMeeting(t *testing.T) {
	r := testRoom(t)
	r.SetPolicy(AdmissionKnock)

	var delivered atomic.Int64
	insider := &Participant{
		ID: "inside", DisplayName: "Inside",
		Send: func([]byte) { delivered.Add(1) },
	}
	if _, err := r.Join(insider); err != nil {
		t.Fatal(err)
	}

	w := waiter("outside", true)
	if err := r.Knock(w); err != nil {
		t.Fatal(err)
	}

	// Not a participant.
	if got := len(r.Snapshot()); got != 1 {
		t.Fatalf("roster holds %d participants; a waiter is in the meeting", got)
	}
	for _, p := range r.Snapshot() {
		if p.ID == "outside" {
			t.Fatal("a waiter appears in the roster")
		}
	}

	// Not reachable by broadcast. This is the one that matters: a broadcast
	// carries whatever the meeting is saying.
	before := delivered.Load()
	r.Broadcast([]byte{0x01, 0x02, 0x03}, "")
	if delivered.Load() != before+1 {
		t.Fatalf("broadcast reached %d recipients; only the one participant should have got it",
			delivered.Load()-before)
	}

	// And leaving as a waiter is not the same as leaving as a participant.
	if err := r.Leave("outside"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a waiter could be removed as a participant: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

func TestAdmissionPolicyMatrix(t *testing.T) {
	cases := []struct {
		policy Admission
		authed bool
		wait   bool
	}{
		{AdmissionOpen, true, false},
		{AdmissionOpen, false, false},
		{AdmissionTrusted, true, false},
		{AdmissionTrusted, false, true},
		{AdmissionKnock, true, true},
		{AdmissionKnock, false, true},
	}
	for _, c := range cases {
		r := testRoom(t)
		r.SetPolicy(c.policy)
		if got := r.MustWait(c.authed); got != c.wait {
			t.Errorf("policy=%s authenticated=%v: MustWait=%v, want %v",
				c.policy, c.authed, got, c.wait)
		}
	}
}

// An unrecognised policy must fail CLOSED. A typo in a config file, a value
// from an older or newer version, a malformed API call: every one of those
// must make the meeting stricter, never open it to the world.
func TestUnknownPolicyFailsClosed(t *testing.T) {
	for _, bad := range []Admission{"", "OPEN", "public", "anyone", "1", "true", "Open"} {
		r := testRoom(t)
		r.SetPolicy(bad)
		if r.Policy() != AdmissionKnock {
			t.Fatalf("policy %q normalised to %q, not the strictest", bad, r.Policy())
		}
		if !r.MustWait(true) || !r.MustWait(false) {
			t.Fatalf("policy %q let somebody in without a decision", bad)
		}
	}
}

// The default for a brand new room must not be open.
func TestDefaultPolicyIsNotOpen(t *testing.T) {
	r := testRoom(t)
	if r.Policy() == AdmissionOpen {
		t.Fatal("a new room defaults to open; a leaked link would be an open door")
	}
	if !r.MustWait(false) {
		t.Fatal("an unauthenticated arrival is admitted by default")
	}
}

// ---------------------------------------------------------------------------
// Decisions
// ---------------------------------------------------------------------------

func TestAdmitReleasesTheWaiter(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	if err := r.Knock(w); err != nil {
		t.Fatal(err)
	}

	got := make(chan WaitState, 1)
	go func() { got <- w.Wait(nil, 5*time.Second) }()

	time.Sleep(20 * time.Millisecond)
	changed, err := r.Decide("a", true)
	if err != nil || !changed {
		t.Fatalf("Decide: changed=%v err=%v", changed, err)
	}

	select {
	case s := <-got:
		if s != WaitAdmitted {
			t.Fatalf("waiter resolved to %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitting did not release the waiter")
	}
	if r.WaitingCount() != 0 {
		t.Fatal("an admitted waiter is still queued")
	}
}

func TestDenyReleasesTheWaiter(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	_ = r.Knock(w)

	got := make(chan WaitState, 1)
	go func() { got <- w.Wait(nil, 5*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	if _, err := r.Decide("a", false); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != WaitDenied {
		t.Fatalf("waiter resolved to %q, want denied", s)
	}
}

// Two hosts clicking at the same instant must produce exactly one decision,
// and must not deadlock whichever of them loses.
func TestConcurrentDecisionsSettleExactlyOnce(t *testing.T) {
	for round := 0; round < 300; round++ {
		r := testRoom(t)
		w := waiter("a", false)
		_ = r.Knock(w)

		var wg sync.WaitGroup
		changes := make(chan bool, 32)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				changed, _ := r.Decide("a", n%2 == 0)
				changes <- changed
			}(i)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: concurrent decisions deadlocked", round)
		}
		close(changes)

		n := 0
		for c := range changes {
			if c {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: %d decisions took effect, want exactly 1", round, n)
		}
		if s := w.State(); s != WaitAdmitted && s != WaitDenied {
			t.Fatalf("round %d: waiter is %q after a decision", round, s)
		}
	}
}

// Deciding twice must not error the second time in a way that a host sees, and
// must not change the outcome.
func TestSecondDecisionDoesNotOverrideTheFirst(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	_ = r.Knock(w)

	if _, err := r.Decide("a", false); err != nil {
		t.Fatal(err)
	}
	// Admitting somebody already denied must not let them in.
	changed, err := r.Decide("a", true)
	if changed {
		t.Fatal("a denied waiter was admitted by a second decision")
	}
	if !errors.Is(err, ErrNoSuchWait) {
		t.Fatalf("second decision returned %v", err)
	}
	if w.State() != WaitDenied {
		t.Fatalf("waiter state is %q after being denied then admitted", w.State())
	}
}

func TestDecideUnknownWaiter(t *testing.T) {
	r := testRoom(t)
	if _, err := r.Decide("nobody", true); !errors.Is(err, ErrNoSuchWait) {
		t.Fatalf("deciding an unknown waiter returned %v", err)
	}
}

func TestAdmitAllReleasesEveryone(t *testing.T) {
	r := testRoom(t)
	const n = 25
	ws := make([]*Waiter, n)
	for i := range ws {
		ws[i] = waiter(fmt.Sprintf("w%02d", i), false)
		if err := r.Knock(ws[i]); err != nil {
			t.Fatal(err)
		}
	}
	if r.WaitingCount() != n {
		t.Fatalf("%d waiting, want %d", r.WaitingCount(), n)
	}

	if got := r.DecideAll(true); got != n {
		t.Fatalf("DecideAll settled %d, want %d", got, n)
	}
	for i, w := range ws {
		if w.State() != WaitAdmitted {
			t.Fatalf("waiter %d is %q", i, w.State())
		}
	}
	if r.WaitingCount() != 0 {
		t.Fatal("the lobby is not empty after admitting everybody")
	}
}

// ---------------------------------------------------------------------------
// Limits, which are the part an outsider can attack
// ---------------------------------------------------------------------------

// A locked meeting refuses knocks outright. Locking is what a host reaches for
// when something is going wrong, so it must be absolute.
func TestLockedMeetingRefusesKnocks(t *testing.T) {
	r := testRoom(t)
	r.SetLocked(true)
	if err := r.Knock(waiter("a", true)); !errors.Is(err, ErrLocked) {
		t.Fatalf("a locked meeting accepted a knock: %v", err)
	}
	if r.WaitingCount() != 0 {
		t.Fatal("a locked meeting queued somebody")
	}

	r.SetLocked(false)
	if err := r.Knock(waiter("a", true)); err != nil {
		t.Fatalf("unlocking did not restore knocking: %v", err)
	}
}

// The queue is bounded. An unbounded lobby is a resource an unauthenticated
// party can consume from outside the meeting, which is a denial of service
// that needs no credentials at all.
func TestLobbyQueueIsBounded(t *testing.T) {
	r := testRoom(t)
	for i := 0; i < MaxWaiting; i++ {
		if err := r.Knock(waiter(fmt.Sprintf("w%04d", i), false)); err != nil {
			t.Fatalf("knock %d refused early: %v", i, err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := r.Knock(waiter(fmt.Sprintf("over%04d", i), false)); !errors.Is(err, ErrLobbyFull) {
			t.Fatalf("knock past the cap returned %v", err)
		}
	}
	if got := r.WaitingCount(); got != MaxWaiting {
		t.Fatalf("%d waiting, cap is %d", got, MaxWaiting)
	}
}

// Nobody waits forever. An abandoned knock holds a connection.
func TestStaleKnocksAreSwept(t *testing.T) {
	r := testRoom(t)
	old := waiter("old", false)
	fresh := waiter("fresh", false)
	_ = r.Knock(old)
	_ = r.Knock(fresh)

	// Age one of them.
	old.KnockedAt = time.Now().Add(-2 * LobbyTimeout)

	swept := r.SweepLobby(time.Now(), LobbyTimeout)
	if len(swept) != 1 || swept[0].ID != "old" {
		t.Fatalf("sweep took %d waiters, want just the stale one", len(swept))
	}
	if old.State() != WaitExpired {
		t.Fatalf("the swept waiter is %q, want expired", old.State())
	}
	if r.WaitingCount() != 1 {
		t.Fatalf("%d still waiting, want 1", r.WaitingCount())
	}
	if fresh.State() != WaitPending {
		t.Fatal("the sweep expired somebody who had only just knocked")
	}
}

// Waiting must end when the connection goes away, or every abandoned tab
// leaves a goroutine parked in the lobby for the timeout.
func TestWaitEndsWhenTheConnectionDoes(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	_ = r.Knock(w)

	done := make(chan struct{})
	got := make(chan WaitState, 1)
	go func() { got <- w.Wait(done, time.Hour) }()

	time.Sleep(20 * time.Millisecond)
	close(done)

	select {
	case s := <-got:
		if s != WaitDenied {
			t.Fatalf("a disconnected waiter resolved to %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the connection did not end the wait")
	}
}

func TestWaitTimesOut(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	_ = r.Knock(w)

	start := time.Now()
	s := w.Wait(nil, 100*time.Millisecond)
	if s != WaitExpired {
		t.Fatalf("wait resolved to %q, want expired", s)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestWithdrawRemovesFromTheQueue(t *testing.T) {
	r := testRoom(t)
	w := waiter("a", false)
	_ = r.Knock(w)

	r.Withdraw("a")
	if r.WaitingCount() != 0 {
		t.Fatal("withdrawing left the waiter queued")
	}
	r.Withdraw("a")       // idempotent
	r.Withdraw("unknown") // and forgiving
}

// Everything at once, under -race: knocks, decisions, sweeps and listing.
func TestLobbyUnderConcurrency(t *testing.T) {
	r := testRoom(t)
	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				id := fmt.Sprintf("w%02d-%02d", n, j)
				if err := r.Knock(waiter(id, j%2 == 0)); err == nil {
					r.Decide(id, j%3 == 0)
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Waiting()
				_ = r.WaitingCount()
				_ = r.SweepLobby(time.Now(), time.Hour)
				r.SetPolicy(AdmissionTrusted)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("lobby operations deadlocked under concurrency")
	}

	// Whatever happened, the room itself must still be coherent.
	r.DecideAll(false)
	if r.WaitingCount() != 0 {
		t.Fatalf("%d waiters left after clearing the lobby", r.WaitingCount())
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("lobby churn leaked somebody into the participant list")
	}
}
