package messaging

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
)

// fakeSock is a hub entry without a network connection: it counts as a
// live device and swallows what is pushed to it.
func fakeSock(userID string) *sock {
	tx, _ := wire.NewPreloadedPair()
	return &sock{userID: userID, tx: tx, out: make(chan []byte, 64)}
}

// The rooms set a status nobody can pick, and only the rooms: in a call
// while you are in one, presenting while you share, whatever you had picked
// -- and the moment you leave, what you had picked, or available.
func TestOnlyARoomSetsInACallOrPresenting(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	live := map[string]bool{}
	s.LiveNow = func() map[string]bool { return live }
	get := func() Status {
		got, err := s.Statuses(ctx, you, []string{me.ID})
		if err != nil {
			t.Fatal(err)
		}
		st, ok := got[me.ID]
		if !ok {
			t.Fatalf("a peer always gets an answer: %+v", got)
		}
		return st
	}

	// Nothing picked, no room, no socket: available, offline, never seen.
	if st := get(); st.Status != StatusAvailable || st.Online || st.Auto || st.LastSeen != nil {
		t.Fatalf("baseline: %+v", st)
	}
	// In a room: in a call. Sharing: presenting.
	live[me.ID] = false
	if st := get(); st.Status != StatusInCall || !st.Auto || st.Note != "In a call" {
		t.Fatalf("in a call: %+v", st)
	}
	live[me.ID] = true
	if st := get(); st.Status != StatusPresenting || !st.Auto {
		t.Fatalf("presenting: %+v", st)
	}
	// Whatever was picked, the room wins while they are in it...
	if _, err := s.SetStatus(ctx, me, StatusDND, "heads down", nil); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.Status != StatusPresenting || !st.Auto {
		t.Fatalf("picked while presenting: %+v", st)
	}
	// ...and what SetStatus returns is what peers will see.
	if st, _ := s.SetStatus(ctx, me, StatusAway, "", nil); st.Status != StatusPresenting || !st.Auto {
		t.Fatalf("set while in a room returns presenting: %+v", st)
	}
	// Leaving: the pick is back, not automatic.
	delete(live, me.ID)
	if st := get(); st.Status != StatusAway || st.Auto {
		t.Fatalf("after the call: %+v", st)
	}
	// An expired pick after leaving: available.
	past := time.Now().Add(-time.Minute)
	s.repo.SetStatus(ctx, Status{UserID: me.ID, Status: StatusDND, Until: &past, UpdatedAt: time.Now()})
	if st := get(); st.Status != StatusAvailable || st.Auto {
		t.Fatalf("expired pick: %+v", st)
	}
	// Nobody can set the automatic ones by hand.
	for _, k := range []string{StatusInCall, StatusPresenting} {
		if _, err := s.SetStatus(ctx, me, k, "", nil); err == nil {
			t.Fatalf("%s was accepted as a manual status", k)
		}
	}
}

// Idle on every device reads Away, automatically, with last seen; a device
// with input again clears it; a manual busy is not overridden by idleness.
func TestIdleOnEveryDeviceReadsAway(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	laptop := fakeSock(me.ID)
	phone := fakeSock(me.ID)
	s.hub.add(laptop)
	s.hub.add(phone)
	defer s.hub.remove(laptop)
	defer s.hub.remove(phone)
	get := func() Status {
		got, _ := s.Statuses(ctx, you, []string{me.ID})
		return got[me.ID]
	}
	if st := get(); st.Status != StatusAvailable || !st.Online || st.LastSeen != nil {
		t.Fatalf("connected and active: %+v", st)
	}
	s.DeviceIdle(ctx, laptop, true)
	if st := get(); st.Status != StatusAvailable {
		t.Fatalf("one device idle, the other active: %+v", st)
	}
	s.DeviceIdle(ctx, phone, true)
	st := get()
	if st.Status != StatusAway || !st.Auto || !st.Online || st.LastSeen == nil {
		t.Fatalf("every device idle: %+v", st)
	}
	if time.Since(*st.LastSeen) > 5*time.Second {
		t.Fatalf("last seen is when they went idle: %v", st.LastSeen)
	}
	// A pick stronger than available stands over idleness.
	s.SetStatus(ctx, me, StatusBusy, "", nil)
	if st := get(); st.Status != StatusBusy || st.Auto {
		t.Fatalf("busy while idle: %+v", st)
	}
	s.SetStatus(ctx, me, StatusAvailable, "", nil)
	s.DeviceIdle(ctx, phone, false)
	if st := get(); st.Status != StatusAvailable || st.Auto {
		t.Fatalf("woke: %+v", st)
	}
}

// Appear offline: peers see offline, no last seen, not online -- through
// the read, the directory's Online(), and the connect-time snapshot -- and
// the person themselves sees their pick. Lifting it announces them back.
func TestAppearOfflineHidesPresenceAndLastSeen(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	ctx := context.Background()
	mine := fakeSock(me.ID)
	s.hub.add(mine)
	defer s.hub.remove(mine)
	s.repo.TouchSeen(ctx, me.ID, time.Now().Add(-time.Hour))
	if !s.Online(me.ID) {
		t.Fatal("connected should be online")
	}
	if _, err := s.SetStatus(ctx, me, StatusOffline, "", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Statuses(ctx, you, []string{me.ID})
	if st := got[me.ID]; st.Status != StatusOffline || st.Online || st.LastSeen != nil || st.Auto {
		t.Fatalf("peer's view while appearing offline: %+v", st)
	}
	if s.Online(me.ID) {
		t.Fatal("the directory must not say online")
	}
	if peers := s.OnlinePeers(ctx, you.ID); len(peers) != 0 {
		t.Fatalf("the snapshot must not include them: %v", peers)
	}
	own, _ := s.Statuses(ctx, me, []string{me.ID})
	if st := own[me.ID]; st.Status != StatusOffline {
		t.Fatalf("own view: %+v", st)
	}
	// In a room while appearing offline: still offline to peers.
	s.LiveNow = func() map[string]bool { return map[string]bool{me.ID: true} }
	got, _ = s.Statuses(ctx, you, []string{me.ID})
	if st := got[me.ID]; st.Status != StatusOffline {
		t.Fatalf("a room must not leak through appear offline: %+v", st)
	}
	s.LiveNow = nil
	// Lifted: online again, and the read says so.
	s.SetStatus(ctx, me, StatusAvailable, "", nil)
	got, _ = s.Statuses(ctx, you, []string{me.ID})
	if st := got[me.ID]; st.Status != StatusAvailable || !st.Online || st.LastSeen != nil {
		t.Fatalf("lifted: %+v", st)
	}
	// Disconnected: online for the grace, then offline with the last-seen
	// time from the moment the socket closed.
	s.hub.remove(mine)
	s.SocketClosed(ctx, me.ID)
	got, _ = s.Statuses(ctx, you, []string{me.ID})
	if st := got[me.ID]; !st.Online {
		t.Fatalf("inside the grace they still read online: %+v", st)
	}
	s.hub.now = func() time.Time { return time.Now().Add(OfflineGrace + time.Second) }
	got, _ = s.Statuses(ctx, you, []string{me.ID})
	if st := got[me.ID]; st.Online || st.LastSeen == nil || time.Since(*st.LastSeen) > 5*time.Second {
		t.Fatalf("gone: %+v", st)
	}
}

// The loop: a nudge is answered at once (the tick is an hour away), the
// change reaches the peer and the person, leaving pushes "available" out
// loud when nothing was picked, and nothing is pushed when nothing changed.
func TestAutoStatusLoopPushesOnNudgeAndFallsBackOnLeave(t *testing.T) {
	s, me, you, _ := twoPeople(t)
	var mu sync.Mutex
	live := map[string]bool{}
	s.LiveNow = func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(live))
		for k, v := range live {
			out[k] = v
		}
		return out
	}
	type push struct {
		to string
		st Status
	}
	got := make(chan push, 64)
	s.autoPushed = func(to string, st Status) { got <- push{to, st} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.RunAutoStatus(ctx, time.Hour); close(done) }()

	wait := func(n int) map[string]Status {
		out := map[string]Status{}
		deadline := time.After(3 * time.Second)
		for len(out) < n {
			select {
			case p := <-got:
				out[p.to] = p.st
			case <-deadline:
				t.Fatalf("waited for %d pushes, got %d: %v", n, len(out), out)
			}
		}
		return out
	}
	set := func(id string, presenting bool, in bool) {
		mu.Lock()
		if in {
			live[id] = presenting
		} else {
			delete(live, id)
		}
		mu.Unlock()
		s.NudgeAutoStatus()
	}

	set(me.ID, false, true)
	seen := wait(2) // the peer and the person themselves
	if seen[you.ID].Status != StatusInCall || seen[me.ID].Status != StatusInCall || !seen[me.ID].Auto {
		t.Fatalf("join: %+v", seen)
	}
	set(me.ID, true, true)
	if seen = wait(2); seen[you.ID].Status != StatusPresenting {
		t.Fatalf("share: %+v", seen)
	}
	// A nudge with nothing changed pushes nothing.
	s.NudgeAutoStatus()
	s.NudgeAutoStatus() // and a second one while one is pending does not block
	select {
	case p := <-got:
		t.Fatalf("pushed without a change: %+v", p)
	case <-time.After(150 * time.Millisecond):
	}
	set(me.ID, false, false)
	if seen = wait(2); seen[you.ID].Status != StatusAvailable || seen[you.ID].Auto {
		t.Fatalf("leave with nothing picked: %+v", seen)
	}
	// Idle is the loop's business too: a device going idle wakes it.
	sk := fakeSock(me.ID)
	s.hub.add(sk)
	s.DeviceIdle(context.Background(), sk, true)
	if seen = wait(2); seen[you.ID].Status != StatusAway || !seen[you.ID].Auto || seen[you.ID].LastSeen == nil {
		t.Fatalf("idle: %+v", seen)
	}
	s.hub.remove(sk)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}

func TestAutoStatusChangesAreExactlyTheDiff(t *testing.T) {
	prev := map[string]string{"a": StatusInCall, "b": StatusInCall, "c": StatusInCall, "e": StatusAway}
	cur := map[string]string{"b": StatusInCall, "c": StatusPresenting, "d": StatusInCall, "e": StatusAway}
	got := autoStatusChanges(prev, cur)
	sort.Strings(got)
	if strings.Join(got, ",") != "a,c,d" {
		t.Fatalf("changes: %v (a left, c started sharing, d joined; b and e unchanged)", got)
	}
	if n := len(autoStatusChanges(nil, nil)); n != 0 {
		t.Fatalf("nothing to nothing: %d", n)
	}
	if got := autoStatusChanges(nil, map[string]string{"x": StatusInCall}); len(got) != 1 || got[0] != "x" {
		t.Fatalf("first reading: %v", got)
	}
}
