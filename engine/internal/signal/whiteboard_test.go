package signal

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// The bytes, worked out by hand and pinned on both sides (web/src/
// whiteboard.test.mjs has the same hex): style 0x19 = pen, colour 3, width 2;
// points (100,200) (103,198) (110,210) = count 3, 100 and 200 as varints,
// then zigzag deltas +3 -2 +7 +12.
const wbGolden = "19" + "0364c80106030e18"

func TestWhiteboardCodecGolden(t *testing.T) {
	w := wire.NewWriter(nil)
	writeStyle(w, room.ToolPen, 3, 2)
	writePoints(w, []int32{100, 200, 103, 198, 110, 210})
	if got := hex.EncodeToString(w.Bytes()); got != wbGolden {
		t.Fatalf("encoded %s, want %s", got, wbGolden)
	}
	b, _ := hex.DecodeString(wbGolden)
	r := wire.NewReader(b, nil)
	tool, color, width := readStyle(r)
	pts, ok := readPoints(r)
	if !ok || tool != 0 || color != 3 || width != 2 || len(pts) != 6 || pts[2] != 103 || pts[3] != 198 || pts[5] != 210 {
		t.Fatalf("decoded tool %d colour %d width %d points %v ok %v", tool, color, width, pts, ok)
	}
}

// A pen moving a few units between samples costs about two bytes a point.
func TestWhiteboardStrokeIsCompact(t *testing.T) {
	pts := []int32{}
	for i := int32(0); i < 200; i++ {
		pts = append(pts, 500+i*3, 900+(i%7)-3)
	}
	w := wire.NewWriter(nil)
	writePoints(w, pts)
	per := float64(len(w.Bytes())) / 200
	t.Logf("200 points: %d bytes, %.2f a point", len(w.Bytes()), per)
	if per > 2.2 {
		t.Fatalf("%.2f bytes a point", per)
	}
}

// A count past the bound, or a point off the board, is refused before
// anything is allocated for it.
func TestWhiteboardReadBounds(t *testing.T) {
	// 4097 real points, every one on the board: refused for the count alone.
	many := make([]int32, 0, 2*(room.MaxWBStrokePoints+1))
	for i := 0; i <= room.MaxWBStrokePoints; i++ {
		many = append(many, int32(i%4000), 5)
	}
	w := wire.NewWriter(nil)
	writePoints(w, many)
	if _, ok := readPoints(wire.NewReader(w.Bytes(), nil)); ok {
		t.Fatal("accepted 4097 points")
	}
	w = wire.NewWriter(nil)
	writePoints(w, []int32{4000, 10, 4000, 10})
	b := w.Bytes()
	// Push the second point off the right edge: delta +200.
	w2 := wire.NewWriter(nil)
	w2.Uvarint(2)
	w2.Uvarint(4000)
	w2.Uvarint(10)
	w2.Svarint(200)
	w2.Svarint(0)
	if _, ok := readPoints(wire.NewReader(w2.Bytes(), nil)); ok {
		t.Fatal("accepted a point off the board")
	}
	if _, ok := readPoints(wire.NewReader(b, nil)); !ok {
		t.Fatal("refused a good stroke")
	}
}

func (f *presenterFixture) wb(from string, enc func(w *wire.Writer)) {
	f.send(from, func(w *wire.Writer) {
		w.WriteHeader(wire.OpWhiteboard, 0, false)
		enc(w)
	})
}

func stroke(kind uint32, seq uint64) func(w *wire.Writer) {
	return func(w *wire.Writer) {
		w.Bits(kind, 3)
		w.Bits(0, 5)
		w.Uvarint(seq)
		writeStyle(w, room.ToolPen, 2, 3)
		writePoints(w, []int32{10, 10, 20, 15, 30, 25})
	}
}

// Through the dispatcher: open reaches everyone; a stroke reaches everyone
// and is kept; a live preview reaches everyone else and is not kept; only
// the opener or a host turns it off; nothing is drawn on a closed board.
func TestWhiteboardThroughGateway(t *testing.T) {
	f := newPresenterFixture(t, "a", "b", "c")
	f.wb("b", func(w *wire.Writer) { w.Bits(wbState, 3); w.Bits(0, 5) }) // b opens it
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 400*time.Millisecond); count(got, wire.OpWhiteboard) != 1 {
			t.Fatalf("%s not given the board: %+v", id, got)
		}
	}
	f.wb("c", stroke(wbStroke, 7))
	for _, id := range []string{"a", "b", "c"} {
		if got := f.drain(id, 400*time.Millisecond); count(got, wire.OpWhiteboard) != 1 {
			t.Fatalf("%s not sent the stroke: %+v", id, got)
		}
	}
	if n := len(f.rm.Whiteboard().Strokes); n != 1 {
		t.Fatalf("%d strokes kept", n)
	}
	f.wb("c", stroke(wbLive, 8))
	if got := f.drain("a", 400*time.Millisecond); count(got, wire.OpWhiteboard) != 1 {
		t.Fatalf("a did not get the live preview: %+v", got)
	}
	if got := f.drain("c", 300*time.Millisecond); count(got, wire.OpWhiteboard) != 0 {
		t.Fatal("the drawer was sent their own preview")
	}
	if n := len(f.rm.Whiteboard().Strokes); n != 1 {
		t.Fatal("a live preview was stored")
	}
	f.drain("b", 200*time.Millisecond)
	f.wb("c", func(w *wire.Writer) { w.Bits(wbClose, 3); w.Bits(0, 5) })
	if !f.rm.WhiteboardOpen() {
		t.Fatal("someone other than the opener or a host turned it off")
	}
	f.wb("b", func(w *wire.Writer) { w.Bits(wbClose, 3); w.Bits(0, 5) })
	if f.rm.WhiteboardOpen() {
		t.Fatal("the opener could not turn it off")
	}
	f.drain("a", 300*time.Millisecond)
	f.wb("c", stroke(wbStroke, 9))
	if got := f.drain("a", 300*time.Millisecond); count(got, wire.OpWhiteboard) != 0 {
		t.Fatal("a stroke on a closed board was sent")
	}
}
