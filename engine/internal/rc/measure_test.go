package rc

import (
	"encoding/json"
	"math/rand"
	"testing"
)

// Bit-level accounting for the input protocol.
//
// Input is the one place in this system where size is deliberately not the
// first consideration -- a misplaced click is irreversible, so every event
// carries its own absolute coordinates rather than a delta. These tests exist
// to keep that decision honest by pricing it, so the trade is a measured one
// rather than an assumed one.

func mustEncode(t *testing.T, e Event) []byte {
	t.Helper()
	b, err := e.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEventSizes(t *testing.T) {
	cases := []struct {
		name string
		e    Event
		want int
	}{
		{"mouse move", Event{Kind: KindMouseMove, X: 30000, Y: 20000}, 5},
		{"mouse button", Event{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 1, Y: 2}, 6},
		{"mouse wheel", Event{Kind: KindMouseWheel, WheelY: 3, X: 1, Y: 2}, 9},
		{"key", Event{Kind: KindKey, VK: 0x41, Down: true}, 4},
		{"text 'a'", Event{Kind: KindText, Text: "a"}, 3},
		{"release all", Event{Kind: KindRelease}, 1},
	}
	t.Log("")
	t.Log("  INPUT EVENT SIZES")
	t.Log("  ─────────────────────────────────")
	for _, c := range cases {
		got := len(mustEncode(t, c.e))
		if got != c.want {
			t.Errorf("%s: %d bytes, expected %d", c.name, got, c.want)
		}
		t.Logf("  %-14s %2d B", c.name, got)
	}
	t.Log("")
}

// jsonEvent is the shape a JSON control protocol would use, for comparison.
type jsonEvent struct {
	Type   string `json:"type"`
	X      uint16 `json:"x,omitempty"`
	Y      uint16 `json:"y,omitempty"`
	Button string `json:"button,omitempty"`
	Down   bool   `json:"down,omitempty"`
	DX     int16  `json:"dx,omitempty"`
	DY     int16  `json:"dy,omitempty"`
	Key    uint16 `json:"key,omitempty"`
	Text   string `json:"text,omitempty"`
}

func asJSON(e Event) []byte {
	j := jsonEvent{X: e.X, Y: e.Y, Down: e.Down, Key: e.VK, Text: e.Text,
		DX: e.WheelX, DY: e.WheelY}
	switch e.Kind {
	case KindMouseMove:
		j.Type = "mousemove"
	case KindMouseButton:
		j.Type = "mousebutton"
		j.Button = [...]string{"left", "right", "middle", "x1", "x2"}[e.Button]
	case KindMouseWheel:
		j.Type = "mousewheel"
	case KindKey:
		j.Type = "key"
	case KindText:
		j.Type = "text"
	case KindRelease:
		j.Type = "release"
	}
	b, _ := json.Marshal(j)
	return b
}

// A minute of active remote control, priced.
//
// The shape is taken from what a support operator actually does: long spells
// of pointing and clicking, punctuated by typing, with the mouse moving
// continuously while in use. Sixty samples a second while moving is the rate a
// browser delivers pointermove at.
func TestMinuteOfRemoteControl(t *testing.T) {
	rng := rand.New(rand.NewSource(5))

	var events []Event
	x, y := uint16(30000), uint16(30000)
	for second := 0; second < 60; second++ {
		switch {
		case second%10 < 6: // pointing and clicking
			for i := 0; i < 60; i++ {
				x = uint16(int(x) + rng.Intn(400) - 200)
				y = uint16(int(y) + rng.Intn(400) - 200)
				events = append(events, Event{Kind: KindMouseMove, X: x, Y: y})
			}
			events = append(events,
				Event{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: x, Y: y},
				Event{Kind: KindMouseButton, Button: ButtonLeft, Down: false, X: x, Y: y})
		case second%10 < 9: // typing
			for i := 0; i < 6; i++ {
				events = append(events,
					Event{Kind: KindKey, VK: uint16(0x41 + rng.Intn(26)), Down: true},
					Event{Kind: KindKey, VK: uint16(0x41 + rng.Intn(26)), Down: false})
			}
		default: // reading, nothing sent at all
		}
	}

	raw, jsonBytes := 0, 0
	for _, e := range events {
		raw += len(mustEncode(t, e))
		jsonBytes += len(asJSON(e))
	}

	// Coalescing in steady state does nothing, and it is worth saying so
	// rather than quoting a flattering number.
	//
	// A browser delivers pointermove at about 60 Hz, and the control socket
	// coalesces over 15 ms. That is 0.9 moves per window: fewer than one, so
	// there is never a pair of consecutive moves in a batch to collapse. The
	// saving is exactly zero, and any measurement claiming otherwise has
	// batched the events wrongly.
	steady := 0
	const perWindow = 1 // 60 Hz over a 15 ms window, rounded up
	for i := 0; i < len(events); i += perWindow {
		end := min(i+perWindow, len(events))
		for _, e := range Coalesce(events[i:end]) {
			steady += len(mustEncode(t, e))
		}
	}

	// Where coalescing does earn its place is after a stall. If the link
	// pauses for half a second and then flushes, the backlog holds ~30
	// consecutive moves, and replaying all of them drags the cursor through
	// history before arriving where the operator is now pointing. Collapsing
	// them is both smaller and more correct.
	const stallMoves = 30
	backlog := make([]Event, 0, stallMoves+2)
	for i := 0; i < stallMoves; i++ {
		backlog = append(backlog, Event{Kind: KindMouseMove, X: uint16(1000 + i*100), Y: 500})
	}
	backlog = append(backlog,
		Event{Kind: KindMouseButton, Button: ButtonLeft, Down: true, X: 4000, Y: 500})

	burstRaw := 0
	for _, e := range backlog {
		burstRaw += len(mustEncode(t, e))
	}
	burstCoalesced := 0
	for _, e := range Coalesce(backlog) {
		burstCoalesced += len(mustEncode(t, e))
	}

	t.Log("")
	t.Log("  ONE MINUTE OF ACTIVE REMOTE CONTROL")
	t.Log("  ────────────────────────────────────────────────")
	t.Logf("  events generated        %6d", len(events))
	t.Logf("  binary, every event     %6d B  (%.1f kbit/s)", raw, float64(raw)*8/60/1000)
	t.Logf("  after coalescing        %6d B  (unchanged: see below)", steady)
	t.Logf("  the same as JSON        %6d B  (%.1f kbit/s)", jsonBytes, float64(jsonBytes)*8/60/1000)
	t.Logf("  binary vs JSON          %6.1fx smaller", float64(jsonBytes)/float64(raw))
	t.Log("")
	t.Log("  For scale, the screen feed for the same minute is ~1.42 MB.")
	t.Logf("  Input is %.2f%% of that, which is what buys absolute coordinates",
		100*float64(raw)/1_423_604)
	t.Log("  on every event and therefore a click that cannot land in the")
	t.Log("  wrong place.")
	t.Log("")
	t.Log("  Coalescing saves nothing in steady state, and that is the honest")
	t.Log("  answer: 60 Hz input against a 15 ms window is 0.9 moves per batch,")
	t.Log("  so consecutive moves never meet. It earns its place only after a")
	t.Log("  stall, where the backlog is dense:")
	t.Logf("    500 ms backlog, replayed   %4d B  (%d events)", burstRaw, len(backlog))
	t.Logf("    the same, coalesced        %4d B  (%d events)", burstCoalesced, len(Coalesce(backlog)))
	t.Logf("    -> %.0f%% smaller, and the cursor stops being dragged through history",
		100*(1-float64(burstCoalesced)/float64(burstRaw)))
	t.Log("")

	if steady != raw {
		t.Fatalf("steady-state coalescing changed the byte count (%d vs %d); "+
			"at 60 Hz over 15 ms there is at most one move per batch, so it "+
			"cannot have collapsed anything", steady, raw)
	}
	if burstCoalesced >= burstRaw {
		t.Fatal("coalescing a dense backlog saved nothing")
	}
}
