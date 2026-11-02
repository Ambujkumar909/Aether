package rc

import (
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A recording injector
// ---------------------------------------------------------------------------

type call struct {
	op   string
	x, y uint16
	btn  Button
	down bool
	dx   int16
	dy   int16
	vk   uint16
	text string
}

type fakeInjector struct {
	mu    sync.Mutex
	calls []call
	// failOn makes one operation fail, to prove release still completes.
	failOn string
}

func (f *fakeInjector) record(c call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.failOn != "" && c.op == f.failOn {
		return errors.New("injected failure")
	}
	return nil
}

func (f *fakeInjector) MouseMove(x, y uint16) error {
	return f.record(call{op: "move", x: x, y: y})
}
func (f *fakeInjector) MouseButton(b Button, down bool) error {
	return f.record(call{op: "button", btn: b, down: down})
}
func (f *fakeInjector) MouseWheel(dx, dy int16) error {
	return f.record(call{op: "wheel", dx: dx, dy: dy})
}
func (f *fakeInjector) Key(vk uint16, down bool) error {
	return f.record(call{op: "key", vk: vk, down: down})
}
func (f *fakeInjector) Text(s string) error {
	return f.record(call{op: "text", text: s})
}

func (f *fakeInjector) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// heldByOS replays what the injector was told and reports what the operating
// system would still consider pressed.
//
// This deliberately does not consult Session's own bookkeeping. A test that
// asks the session whether it thinks anything is held proves only that its
// counter is consistent with itself; what matters is the state of the machine
// underneath, which is the sequence of downs and ups actually delivered.
func (f *fakeInjector) heldByOS() (buttons map[Button]bool, keys map[uint16]bool) {
	buttons = map[Button]bool{}
	keys = map[uint16]bool{}
	for _, c := range f.snapshot() {
		switch c.op {
		case "button":
			if c.down {
				buttons[c.btn] = true
			} else {
				delete(buttons, c.btn)
			}
		case "key":
			if c.down {
				keys[c.vk] = true
			} else {
				delete(keys, c.vk)
			}
		}
	}
	return buttons, keys
}

func grantedSession(t *testing.T, until time.Duration) (*Session, *fakeInjector, time.Time) {
	t.Helper()
	f := &fakeInjector{}
	s := NewSession(f)
	now := time.Now()
	s.Grant(Grant{SessionID: "sess-1", ControllerID: "op-1", Expires: now.Add(until)})
	return s, f, now
}

// ---------------------------------------------------------------------------
// Codec
// ---------------------------------------------------------------------------

func TestEventRoundTrip(t *testing.T) {
	cases := []Event{
		{Kind: KindMouseMove, X: 0, Y: 0},
		{Kind: KindMouseMove, X: CoordMax, Y: CoordMax},
		{Kind: KindMouseMove, X: 32768, Y: 1234},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 100, Y: 200},
		{Kind: KindMouseButton, Button: ButtonX2, Down: false, X: 65535, Y: 1},
		{Kind: KindMouseWheel, WheelX: -3, WheelY: 5, X: 10, Y: 20},
		{Kind: KindMouseWheel, WheelX: 32767, WheelY: -32768, X: 0, Y: 0},
		{Kind: KindKey, VK: 0x41, Down: true},
		{Kind: KindKey, VK: 0xFFFF, Down: false},
		{Kind: KindText, Text: "hello"},
		{Kind: KindText, Text: "naïve café 日本語 🎉"},
		{Kind: KindRelease},
	}
	for i, want := range cases {
		enc, err := want.Encode(nil)
		if err != nil {
			t.Fatalf("case %d: encode: %v", i, err)
		}
		got, rest, err := DecodeEvent(enc)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		if len(rest) != 0 {
			t.Fatalf("case %d: %d bytes left over", i, len(rest))
		}
		if got != want {
			t.Fatalf("case %d: round trip\n got %+v\nwant %+v", i, got, want)
		}
	}
}

// The decoder parses bytes from another machine. It must never panic, and it
// must never invent a plausible event from malformed input, because a crash
// here is a crash in the agent that holds somebody's keyboard.
func TestDecoderSurvivesGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200000; i++ {
		b := make([]byte, rng.Intn(24))
		rng.Read(b)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic decoding % x: %v", b, p)
				}
			}()
			_, _, _ = DecodeEvent(b)
			_, _ = DecodeAll(b)
		}()
	}
}

// Every prefix of a valid event must be rejected, not partially accepted.
func TestTruncatedEventsAreRejected(t *testing.T) {
	for _, e := range []Event{
		{Kind: KindMouseMove, X: 1, Y: 2},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 3, Y: 4},
		{Kind: KindMouseWheel, WheelY: 1, X: 5, Y: 6},
		{Kind: KindKey, VK: 0x41, Down: true},
		{Kind: KindText, Text: "abcdef"},
	} {
		full, err := e.Encode(nil)
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n < len(full); n++ {
			if _, _, err := DecodeEvent(full[:n]); err == nil {
				t.Fatalf("kind %d truncated to %d/%d bytes decoded without error",
					e.Kind, n, len(full))
			}
		}
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	for k := 6; k < 256; k++ {
		if _, _, err := DecodeEvent([]byte{byte(k), 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
			t.Fatalf("unknown kind %d was accepted", k)
		}
	}
}

func TestInvalidUTF8TextIsRejected(t *testing.T) {
	// 0xff is never valid UTF-8.
	if _, _, err := DecodeEvent([]byte{KindText, 2, 0xff, 0xfe}); err == nil {
		t.Fatal("invalid utf-8 text was accepted")
	}
	bad := Event{Kind: KindText, Text: string([]byte{0xff, 0xfe})}
	if _, err := bad.Encode(nil); err == nil {
		t.Fatal("invalid utf-8 text was encoded")
	}
}

// ---------------------------------------------------------------------------
// Coalescing
// ---------------------------------------------------------------------------

// Consecutive moves collapse, but a move immediately before any other event
// must survive: the cursor position at the moment of a click is part of what
// the click means.
func TestCoalesceKeepsThePositionOfEveryClick(t *testing.T) {
	in := []Event{
		{Kind: KindMouseMove, X: 1},
		{Kind: KindMouseMove, X: 2},
		{Kind: KindMouseMove, X: 3},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 3},
		{Kind: KindMouseMove, X: 4},
		{Kind: KindMouseMove, X: 5},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: false, X: 5},
		{Kind: KindKey, VK: 0x41, Down: true},
	}
	out := Coalesce(in)

	want := []Event{
		{Kind: KindMouseMove, X: 3},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 3},
		{Kind: KindMouseMove, X: 5},
		{Kind: KindMouseButton, Button: ButtonLeft, Down: false, X: 5},
		{Kind: KindKey, VK: 0x41, Down: true},
	}
	if len(out) != len(want) {
		t.Fatalf("coalesced to %d events, want %d: %+v", len(out), len(want), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("event %d: got %+v want %+v", i, out[i], want[i])
		}
	}
	t.Logf("a drag of %d events coalesced to %d without moving any click", len(in), len(out))
}

func TestCoalesceNeverDropsNonMoves(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for round := 0; round < 2000; round++ {
		n := rng.Intn(40)
		in := make([]Event, n)
		nonMoves := 0
		for i := range in {
			if rng.Intn(3) == 0 {
				in[i] = Event{Kind: KindKey, VK: uint16(rng.Intn(200)), Down: rng.Intn(2) == 0}
				nonMoves++
			} else {
				in[i] = Event{Kind: KindMouseMove, X: uint16(rng.Intn(CoordMax))}
			}
		}
		out := Coalesce(in)
		got := 0
		for _, e := range out {
			if e.Kind != KindMouseMove {
				got++
			}
		}
		if got != nonMoves {
			t.Fatalf("round %d: %d non-move events became %d", round, nonMoves, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Consent enforcement
// ---------------------------------------------------------------------------

// Rule 1 and rule 5: nothing is injected without a grant this machine holds.
func TestNoGrantInjectsNothing(t *testing.T) {
	f := &fakeInjector{}
	s := NewSession(f)

	err := s.Apply("sess-1", Event{Kind: KindMouseMove, X: 10, Y: 10}, time.Now())
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("ungranted injection returned %v, want ErrNotPermitted", err)
	}
	if len(f.snapshot()) != 0 {
		t.Fatalf("an ungranted session injected %d events", len(f.snapshot()))
	}
}

// A stale session must not inject into a live one. This is the revoked
// operator who keeps typing.
func TestWrongSessionIDIsRefused(t *testing.T) {
	s, f, now := grantedSession(t, time.Minute)

	err := s.Apply("sess-OTHER", Event{Kind: KindMouseMove, X: 1, Y: 1}, now)
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("foreign session id returned %v, want ErrNotPermitted", err)
	}
	if len(f.snapshot()) != 0 {
		t.Fatal("a foreign session id injected input")
	}
}

// Rule 2: the grant expires on its own, and expiry is checked per event
// rather than once at the start.
func TestGrantExpiresOnItsOwn(t *testing.T) {
	s, f, now := grantedSession(t, time.Second)

	if err := s.Apply("sess-1", Event{Kind: KindMouseMove, X: 1, Y: 1}, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Second)
	if err := s.Apply("sess-1", Event{Kind: KindMouseMove, X: 2, Y: 2}, later); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("injection after expiry returned %v, want ErrNotPermitted", err)
	}
	moves := 0
	for _, c := range f.snapshot() {
		if c.op == "move" {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("%d moves injected; the grant expired after the first", moves)
	}
}

// ---------------------------------------------------------------------------
// The invariant that matters most: nothing is left held down
// ---------------------------------------------------------------------------

// A grant that expires mid-drag must hand the button back.
//
// Left down, the person at the machine finds every subsequent mouse movement
// is a drag, with no visible cause and no obvious remedy.
func TestExpiryMidDragReleasesTheButton(t *testing.T) {
	s, f, now := grantedSession(t, time.Second)

	if err := s.Apply("sess-1", Event{
		Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 100, Y: 100,
	}, now); err != nil {
		t.Fatal(err)
	}
	if btns, _ := f.heldByOS(); !btns[ButtonLeft] {
		t.Fatal("the button was never pressed")
	}

	// The grant lapses while the button is still down.
	_ = s.Apply("sess-1", Event{Kind: KindMouseMove, X: 150, Y: 150}, now.Add(2*time.Second))

	btns, keys := f.heldByOS()
	if len(btns) != 0 || len(keys) != 0 {
		t.Fatalf("expiry left %d buttons and %d keys held down", len(btns), len(keys))
	}
	t.Log("expiry mid-drag released the held button")
}

// Rule 3: revocation is unilateral and instant, and it must not strand a
// modifier. A stuck Alt makes the keyboard unusable.
func TestRevokeReleasesEverythingHeld(t *testing.T) {
	s, f, now := grantedSession(t, time.Hour)

	for _, e := range []Event{
		{Kind: KindKey, VK: 0x12, Down: true}, // ALT
		{Kind: KindKey, VK: 0x11, Down: true}, // CTRL
		{Kind: KindKey, VK: 0x10, Down: true}, // SHIFT
		{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 5, Y: 5},
		{Kind: KindMouseButton, Button: ButtonRight, Down: true, X: 5, Y: 5},
	} {
		if err := s.Apply("sess-1", e, now); err != nil {
			t.Fatal(err)
		}
	}
	btns, keys := f.heldByOS()
	if len(btns) != 2 || len(keys) != 3 {
		t.Fatalf("setup wrong: %d buttons, %d keys held", len(btns), len(keys))
	}

	s.Revoke()

	btns, keys = f.heldByOS()
	if len(btns) != 0 || len(keys) != 0 {
		t.Fatalf("revocation left %d buttons and %d keys held down", len(btns), len(keys))
	}
	if b, k := s.Held(); b != 0 || k != 0 {
		t.Fatalf("session still believes %d buttons and %d keys are held", b, k)
	}
	t.Log("revocation released 2 buttons and 3 modifiers")
}

// A failure releasing one input must not abandon the others. A partial
// release is worse than none: it leaves something stuck with no pattern to it.
func TestReleaseContinuesPastAFailure(t *testing.T) {
	f := &fakeInjector{failOn: "button"}
	s := NewSession(f)
	now := time.Now()
	s.Grant(Grant{SessionID: "s", Expires: now.Add(time.Hour)})

	// The button press itself fails, but the session must still consider it
	// possibly-down and must still try to release the keys.
	_ = s.Apply("s", Event{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 1, Y: 1}, now)
	for _, vk := range []uint16{0x12, 0x11, 0x10} {
		if err := s.Apply("s", Event{Kind: KindKey, VK: vk, Down: true}, now); err != nil {
			t.Fatal(err)
		}
	}

	s.Revoke()

	_, keys := f.heldByOS()
	if len(keys) != 0 {
		t.Fatalf("a failing button release abandoned %d held keys", len(keys))
	}
}

// Pausing for the local user must also let go of everything, or the person
// who just grabbed their own mouse inherits whatever the operator was holding.
func TestPauseReleasesAndRefuses(t *testing.T) {
	s, f, now := grantedSession(t, time.Hour)

	if err := s.Apply("sess-1", Event{
		Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 1, Y: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	s.SetPaused(true)

	if btns, _ := f.heldByOS(); len(btns) != 0 {
		t.Fatalf("pausing left %d buttons held", len(btns))
	}
	if err := s.Apply("sess-1", Event{Kind: KindMouseMove, X: 9, Y: 9}, now); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a paused session injected: %v", err)
	}

	s.SetPaused(false)
	if err := s.Apply("sess-1", Event{Kind: KindMouseMove, X: 9, Y: 9}, now); err != nil {
		t.Fatalf("resuming did not restore injection: %v", err)
	}
}

// Replacing a grant must not strand what the previous one was holding.
func TestReplacingAGrantReleasesTheOldOne(t *testing.T) {
	s, f, now := grantedSession(t, time.Hour)
	if err := s.Apply("sess-1", Event{Kind: KindKey, VK: 0x12, Down: true}, now); err != nil {
		t.Fatal(err)
	}

	s.Grant(Grant{SessionID: "sess-2", Expires: now.Add(time.Hour)})

	if _, keys := f.heldByOS(); len(keys) != 0 {
		t.Fatalf("handing control to a new session left %d keys held", len(keys))
	}
}

// The property, over random traffic: after the session stops, for any reason,
// the machine holds nothing.
func TestNothingIsEverLeftHeldDown(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	for round := 0; round < 3000; round++ {
		f := &fakeInjector{}
		s := NewSession(f)
		now := time.Now()
		s.Grant(Grant{SessionID: "s", Expires: now.Add(time.Minute)})

		for i := 0; i < rng.Intn(40); i++ {
			var e Event
			switch rng.Intn(5) {
			case 0:
				e = Event{Kind: KindMouseMove, X: uint16(rng.Intn(CoordMax))}
			case 1:
				e = Event{Kind: KindMouseButton, Button: Button(rng.Intn(int(buttonCount))),
					Down: rng.Intn(2) == 0}
			case 2:
				e = Event{Kind: KindKey, VK: uint16(rng.Intn(256)), Down: rng.Intn(2) == 0}
			case 3:
				e = Event{Kind: KindMouseWheel, WheelY: int16(rng.Intn(5) - 2)}
			case 4:
				e = Event{Kind: KindText, Text: "x"}
			}
			_ = s.Apply("s", e, now)
		}

		// End the session the way the world ends it: at random.
		switch rng.Intn(3) {
		case 0:
			s.Revoke()
		case 1:
			s.SetPaused(true)
		case 2:
			_ = s.Apply("s", Event{Kind: KindMouseMove}, now.Add(2*time.Minute)) // expiry
		}

		btns, keys := f.heldByOS()
		if len(btns) != 0 || len(keys) != 0 {
			t.Fatalf("round %d: session ended with %d buttons and %d keys still down",
				round, len(btns), len(keys))
		}
	}
	t.Log("3000 random sessions, none left an input held down")
}

// Concurrent input and revocation must be safe. Run under -race.
func TestConcurrentApplyAndRevokeIsSafe(t *testing.T) {
	s, f, now := grantedSession(t, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = s.Apply("sess-1", Event{
					Kind: KindMouseButton, Button: Button(n % int(buttonCount)),
					Down: j%2 == 0, X: uint16(j), Y: uint16(j),
				}, now)
			}
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); time.Sleep(2 * time.Millisecond); s.Revoke() }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent apply and revoke deadlocked")
	}

	s.Revoke() // settle anything applied between the racing revoke and now
	btns, keys := f.heldByOS()
	if len(btns) != 0 || len(keys) != 0 {
		t.Fatalf("after a racing revoke, %d buttons and %d keys are held", len(btns), len(keys))
	}
}

// A click must carry its own position, and the injector must be told to move
// there before the button goes down. Relying on a preceding move having
// arrived is how a click lands somewhere the operator was not pointing.
func TestClickMovesToItsOwnCoordinatesFirst(t *testing.T) {
	s, f, now := grantedSession(t, time.Hour)

	if err := s.Apply("sess-1", Event{
		Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 4242, Y: 2424,
	}, now); err != nil {
		t.Fatal(err)
	}
	calls := f.snapshot()
	if len(calls) < 2 {
		t.Fatalf("a click produced %d injector calls", len(calls))
	}
	if calls[0].op != "move" || calls[0].x != 4242 || calls[0].y != 2424 {
		t.Fatalf("first call was %+v; a click must move to its own coordinates first", calls[0])
	}
	if calls[1].op != "button" || !calls[1].down {
		t.Fatalf("second call was %+v; want the button press", calls[1])
	}
}
