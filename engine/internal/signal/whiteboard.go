package signal

import (
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// The whiteboard on the wire (OpWhiteboard 0x127). The board is room state
// (room/whiteboard.go); this is its encoding and who may do what.
//
// Strokes are the bulk of it, so they are packed: one style byte (tool 1 bit,
// palette colour 4 bits, width-1 3 bits), a point count, the first point
// absolute and the rest as zigzag deltas -- a pen moving a few units between
// samples costs two bytes a point. web/src/whiteboard.js is the other half;
// both are pinned to the same bytes by a golden test.

const (
	wbState  = 0 // server -> client: the board; client -> server: open it
	wbStroke = 1 // a finished stroke
	wbErase  = 2 // remove a stroke or note by id
	wbNote   = 3 // add or change a sticky note
	wbClear  = 4
	wbClose  = 5 // "Turn off whiteboard"
	wbLive   = 6 // a stroke in progress: relayed for the preview, never stored
)

// A finished stroke carries every point again, although the live frames
// already did. That is deliberate (CLAUDE.md rules 2 and 5): a stroke assembled
// from earlier live frames would be cross-frame state, and a person who joined
// mid-stroke, or whose preview was cleared, would hold a broken copy with no
// cheap way to repair it. The stroke frame is self-contained instead. The cost
// was measured: 409 B for a 200-point stroke after 798 B of live frames
// (docs/ARCHITECTURE.md, whiteboard).

func writeStyle(w *wire.Writer, tool, color, width uint8) {
	w.Bits(uint32(tool), 1)
	w.Bits(uint32(color), 4)
	w.Bits(uint32(width-1), 3)
}

func readStyle(r *wire.Reader) (tool, color, width uint8) {
	return uint8(r.Bits(1)), uint8(r.Bits(4)), uint8(r.Bits(3)) + 1
}

// writePoints: count, first point absolute, the rest as deltas.
func writePoints(w *wire.Writer, pts []int32) {
	n := len(pts) / 2
	w.Uvarint(uint64(n))
	var px, py int32
	for i := 0; i < n; i++ {
		x, y := pts[2*i], pts[2*i+1]
		if i == 0 {
			w.Uvarint(uint64(x))
			w.Uvarint(uint64(y))
		} else {
			w.Svarint(int64(x - px))
			w.Svarint(int64(y - py))
		}
		px, py = x, y
	}
}

// readPoints is bounded before it allocates: a count past the limit is an
// error, never a large slice.
func readPoints(r *wire.Reader) ([]int32, bool) {
	n := r.Uvarint()
	if n == 0 || n > room.MaxWBStrokePoints || r.Err() != nil {
		return nil, false
	}
	pts := make([]int32, 0, 2*n)
	var x, y int64
	for i := uint64(0); i < n; i++ {
		if i == 0 {
			x, y = int64(r.Uvarint()), int64(r.Uvarint())
		} else {
			x += r.Svarint()
			y += r.Svarint()
		}
		if r.Err() != nil || x < 0 || y < 0 || x >= room.BoardW || y >= room.BoardH {
			return nil, false
		}
		pts = append(pts, int32(x), int32(y))
	}
	return pts, true
}

func encodeStroke(s room.WBStroke, seq uint64) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpWhiteboard, 0, false)
		w.Bits(wbStroke, 3)
		w.Bits(0, 5)
		w.Uvarint(s.ID)
		w.Uvarint(seq) // the author's preview this replaces
		w.Str(s.Author)
		writeStyle(w, s.Tool, s.Color, s.Width)
		writePoints(w, s.Points)
	}
}

func encodeNote(n room.WBNote) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpWhiteboard, 0, false)
		w.Bits(wbNote, 3)
		w.Bits(0, 5)
		w.Uvarint(n.ID)
		w.Str(n.Author)
		w.Uvarint(uint64(n.X))
		w.Uvarint(uint64(n.Y))
		w.Bits(uint32(n.Color), 4)
		w.Bits(0, 4)
		w.Blob([]byte(n.Text))
	}
}

func encodeWBSimple(kind uint32, id uint64) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpWhiteboard, 0, false)
		w.Bits(kind, 3)
		w.Bits(0, 5)
		if kind == wbErase {
			w.Uvarint(id)
		}
	}
}

// sendWhiteboard sends the whole board: to an arrival, and to everyone when
// it is turned on.
func encodeBoard(st room.WBState) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpWhiteboard, 0, false)
		w.Bits(wbState, 3)
		w.Bool(st.Open)
		w.Bits(0, 4)
		w.Str(st.Opener)
		w.Uvarint(uint64(len(st.Strokes)))
		for _, s := range st.Strokes {
			w.Uvarint(s.ID)
			w.Str(s.Author)
			writeStyle(w, s.Tool, s.Color, s.Width)
			writePoints(w, s.Points)
		}
		w.Uvarint(uint64(len(st.Notes)))
		for _, n := range st.Notes {
			w.Uvarint(n.ID)
			w.Str(n.Author)
			w.Uvarint(uint64(n.X))
			w.Uvarint(uint64(n.Y))
			w.Bits(uint32(n.Color), 4)
			w.Bits(0, 4)
			w.Blob([]byte(n.Text))
		}
	}
}

func (g *Gateway) handleWhiteboard(rm *room.Room, p *room.Participant, r *wire.Reader) error {
	kind := r.Bits(3)
	r.Bits(5)
	switch kind {
	case wbState: // open it
		if err := r.Err(); err != nil {
			return err
		}
		if opened, err := rm.OpenWhiteboard(p.ID); err == nil && opened {
			g.announceAll(rm, encodeBoard(rm.Whiteboard()))
		}
	case wbStroke, wbLive:
		seq := r.Uvarint()
		tool, color, width := readStyle(r)
		pts, ok := readPoints(r)
		if err := r.Err(); err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if kind == wbLive {
			// The preview: to everyone else, now, never stored. Only while
			// the board is open; a bounded batch like a stroke.
			if !rm.WhiteboardOpen() {
				return nil
			}
			g.announce(rm, p.ID, func(w *wire.Writer) {
				w.WriteHeader(wire.OpWhiteboard, 0, false)
				w.Bits(wbLive, 3)
				w.Bits(0, 5)
				w.Uvarint(seq)
				w.Str(p.ID)
				writeStyle(w, tool, color, width)
				writePoints(w, pts)
			})
			return nil
		}
		s, err := rm.AddStroke(room.WBStroke{Author: p.ID, Tool: tool, Color: color, Width: width, Points: pts})
		if err != nil {
			return nil
		}
		g.announceAll(rm, encodeStroke(s, seq))
	case wbErase:
		id := r.Uvarint()
		if err := r.Err(); err != nil {
			return err
		}
		if rm.EraseItem(id) == nil {
			g.announceAll(rm, encodeWBSimple(wbErase, id))
		}
	case wbNote:
		id := r.Uvarint()
		x, y := r.Uvarint(), r.Uvarint()
		color := uint8(r.Bits(4))
		r.Bits(4)
		text := r.Blob()
		if err := r.Err(); err != nil {
			return err
		}
		if x >= room.BoardW || y >= room.BoardH {
			return nil
		}
		n, err := rm.PutNote(room.WBNote{ID: id, Author: p.ID, X: int32(x), Y: int32(y), Color: color, Text: string(text)})
		if err != nil {
			return nil
		}
		g.announceAll(rm, encodeNote(n))
	case wbClear:
		if err := r.Err(); err != nil {
			return err
		}
		if rm.ClearWhiteboard(p.ID) == nil {
			g.announceAll(rm, encodeWBSimple(wbClear, 0))
		}
	case wbClose:
		if err := r.Err(); err != nil {
			return err
		}
		if rm.CloseWhiteboard(p.ID) == nil {
			g.announceAll(rm, encodeWBSimple(wbClose, 0))
		}
	}
	return nil
}
