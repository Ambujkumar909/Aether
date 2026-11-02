package room

import (
	"errors"
	"strings"
	"testing"

	"github.com/aether-rtc/aether/internal/wire"
)

func line(n int) []int32 {
	pts := make([]int32, 0, 2*n)
	for i := 0; i < n; i++ {
		pts = append(pts, int32(10+i%4000), 20)
	}
	return pts
}

// Who may start, close and clear it; drawing needs it open.
func TestWhiteboardPermissions(t *testing.T) {
	r := spotRoom(t, 3) // p0 hosts; p1, p2 attendees
	r.SetRole("p1", wire.RoleMember)
	r.SetRole("p2", wire.RoleMember)
	if _, err := r.AddStroke(WBStroke{Author: "p1", Color: 1, Width: 2, Points: line(3)}); !errors.Is(err, ErrWBClosed) {
		t.Fatalf("drew on a board that is not open: %v", err)
	}
	r.SetPermissions(Permissions{Mic: true, Camera: true, Share: false, Chat: true, Reactions: true})
	if _, err := r.OpenWhiteboard("p1"); !errors.Is(err, ErrWBDenied) {
		t.Fatalf("an attendee who may not present started it: %v", err)
	}
	r.SetPermissions(DefaultPermissions())
	if ok, err := r.OpenWhiteboard("p1"); !ok || err != nil {
		t.Fatalf("open: %v %v", ok, err)
	}
	if ok, _ := r.OpenWhiteboard("p2"); ok {
		t.Fatal("opening an open board reported a change")
	}
	if err := r.ClearWhiteboard("p2"); !errors.Is(err, ErrWBDenied) {
		t.Fatalf("someone else cleared it: %v", err)
	}
	if err := r.CloseWhiteboard("p2"); !errors.Is(err, ErrWBDenied) {
		t.Fatalf("someone else turned it off: %v", err)
	}
	if err := r.ClearWhiteboard("p0"); err != nil {
		t.Fatalf("the host could not clear it: %v", err)
	}
	r.AddStroke(WBStroke{Author: "p2", Color: 1, Width: 2, Points: line(3)})
	if err := r.CloseWhiteboard("p1"); err != nil {
		t.Fatalf("the opener could not turn it off: %v", err)
	}
	// Turned on again: the drawing is still there.
	r.OpenWhiteboard("p0")
	if len(r.Whiteboard().Strokes) != 1 {
		t.Fatal("turning it off lost the drawing")
	}
	r.End()
	if r.WhiteboardOpen() || len(r.Whiteboard().Strokes) != 0 {
		t.Fatal("the board outlived the meeting")
	}
}

// Strokes and notes are checked whole, and the board is bounded.
func TestWhiteboardLimits(t *testing.T) {
	r := spotRoom(t, 2)
	r.OpenWhiteboard("p0")
	bad := []WBStroke{
		{Color: 1, Width: 2, Points: nil},
		{Color: 1, Width: 2, Points: line(MaxWBStrokePoints + 1)},
		{Color: 1, Width: 2, Points: []int32{BoardW, 10}},
		{Color: 1, Width: 2, Points: []int32{-1, 10}},
		{Color: MaxWBPalette, Width: 2, Points: line(2)},
		{Color: 1, Width: 0, Points: line(2)},
		{Color: 1, Width: MaxWBWidth + 1, Points: line(2)},
		{Tool: 2, Color: 1, Width: 2, Points: line(2)},
		{Color: 1, Width: 2, Points: []int32{1, 2, 3}},
	}
	for i, s := range bad {
		if _, err := r.AddStroke(s); !errors.Is(err, ErrWBStroke) {
			t.Fatalf("bad stroke %d accepted: %v", i, err)
		}
	}
	s, err := r.AddStroke(WBStroke{Author: "p1", Color: 15, Width: 8, Tool: ToolHighlighter, Points: line(MaxWBStrokePoints)})
	if err != nil || s.ID == 0 {
		t.Fatalf("a full-length stroke refused: %v", err)
	}
	// The total-points bound: fill to the limit, then one more is refused.
	for r.wb.points+MaxWBStrokePoints <= MaxWBPoints {
		if _, err := r.AddStroke(WBStroke{Color: 1, Width: 1, Points: line(MaxWBStrokePoints)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.AddStroke(WBStroke{Color: 1, Width: 1, Points: line(MaxWBStrokePoints)}); !errors.Is(err, ErrWBFull) {
		t.Fatalf("past the points bound: %v", err)
	}
	r.ClearWhiteboard("p0")
	for i := 0; i < MaxWBStrokes; i++ {
		if _, err := r.AddStroke(WBStroke{Color: 1, Width: 1, Points: line(1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.AddStroke(WBStroke{Color: 1, Width: 1, Points: line(1)}); !errors.Is(err, ErrWBFull) {
		t.Fatalf("stroke 5001: %v", err)
	}

	// Notes.
	for _, n := range []WBNote{{X: 10, Y: 10, Text: ""}, {X: 10, Y: 10, Text: strings.Repeat("x", MaxWBNoteLen+1)}, {X: BoardW, Y: 10, Text: "x"}, {X: 1, Y: 1, Color: MaxWBPalette, Text: "x"}} {
		if _, err := r.PutNote(n); !errors.Is(err, ErrWBNote) {
			t.Fatalf("bad note accepted: %+v %v", n, err)
		}
	}
	n, err := r.PutNote(WBNote{X: 100, Y: 100, Text: strings.Repeat("é", MaxWBNoteLen)})
	if err != nil {
		t.Fatal(err)
	}
	n.Text = "changed"
	if got, err := r.PutNote(n); err != nil || got.ID != n.ID {
		t.Fatalf("edit note: %v", err)
	}
	if _, err := r.PutNote(WBNote{ID: 99999999, X: 1, Y: 1, Text: "x"}); !errors.Is(err, ErrWBMissing) {
		t.Fatalf("editing a note that does not exist: %v", err)
	}
	if err := r.EraseItem(n.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.EraseItem(n.ID); !errors.Is(err, ErrWBMissing) {
		t.Fatalf("erased twice: %v", err)
	}
	for i := 0; i < MaxWBNotes; i++ {
		if _, err := r.PutNote(WBNote{X: 1, Y: 1, Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.PutNote(WBNote{X: 1, Y: 1, Text: "x"}); !errors.Is(err, ErrWBFull) {
		t.Fatalf("note 201: %v", err)
	}
	// The copy is not the room's own.
	st := r.Whiteboard()
	st.Strokes[0].Points[0] = 4000
	if r.Whiteboard().Strokes[0].Points[0] == 4000 {
		t.Fatal("a caller's copy changed the board")
	}
}
