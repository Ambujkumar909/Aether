package room

import (
	"errors"
	"sort"
	"unicode/utf8"
)

// The whiteboard: "Share" > "Whiteboard", drawn on the main stage for
// everyone (Teams: "Collaborate on a whiteboard", "Start Whiteboard", "Turn
// off whiteboard", "Show Whiteboard details on the main stage"). Teams
// embeds Microsoft Whiteboard; this is Aether's own board.
//
// Held by the room: committed strokes and sticky notes, each whole, so a
// late arrival is sent the board and a lost frame is repaired by the next
// (rule 5). A stroke in progress is relayed for the live preview and never
// stored. The board lives as long as the meeting room; keeping it after the
// meeting is not built.

// Board coordinates: a 16:9 board of 4096 x 2304 units, whatever the screen.
const (
	BoardW = 4096
	BoardH = 2304

	MaxWBStrokes      = 5000
	MaxWBStrokePoints = 4096
	MaxWBPoints       = 500000 // all strokes together
	MaxWBNotes        = 200
	MaxWBNoteLen      = 500
	MaxWBPalette      = 16
	MaxWBWidth        = 8
)

var (
	ErrWBClosed  = errors.New("room: the whiteboard is not open")
	ErrWBFull    = errors.New("room: the whiteboard is full")
	ErrWBStroke  = errors.New("room: a stroke needs 1 to 4096 points inside the board")
	ErrWBNote    = errors.New("room: a sticky note needs 1 to 500 characters inside the board")
	ErrWBDenied  = errors.New("room: only whoever started the whiteboard, or a host, can do that")
	ErrWBMissing = errors.New("room: no such item on the whiteboard")
)

// Tools.
const (
	ToolPen         = 0
	ToolHighlighter = 1
)

type WBStroke struct {
	ID     uint64
	Author string // participant id
	Tool   uint8
	Color  uint8 // palette index
	Width  uint8 // 1..8
	Points []int32
}

type WBNote struct {
	ID     uint64
	Author string
	X, Y   int32
	Color  uint8
	Text   string
}

type whiteboard struct {
	open    bool
	opener  string // participant id
	strokes []WBStroke
	notes   map[uint64]*WBNote
	nextID  uint64
	points  int
}

// WBState is a copy of the board for someone arriving or for a test.
type WBState struct {
	Open    bool
	Opener  string
	Strokes []WBStroke
	Notes   []WBNote
}

// OpenWhiteboard: anyone who may present may start it ("share" decides).
// Opening an open board is not an error and changes nothing.
func (r *Room) OpenWhiteboard(by string) (bool, error) {
	if !r.May(by, "share") {
		return false, ErrWBDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb != nil && r.wb.open {
		return false, nil
	}
	if r.wb == nil {
		r.wb = &whiteboard{notes: map[uint64]*WBNote{}}
	}
	r.wb.open, r.wb.opener = true, by
	return true, nil
}

// mayManage: whoever started it, or a host, closes and clears it.
func (r *Room) mayManageWB(by string) bool {
	if r.wb != nil && r.wb.opener == by {
		return true
	}
	if by == r.hostID {
		return true
	}
	p, ok := r.participants[by]
	return ok && p.Role >= hostRole
}

// CloseWhiteboard: "Turn off whiteboard". The drawing is kept, so turning it
// on again in this meeting brings it back.
func (r *Room) CloseWhiteboard(by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb == nil || !r.wb.open {
		return ErrWBClosed
	}
	if !r.mayManageWB(by) {
		return ErrWBDenied
	}
	r.wb.open = false
	return nil
}

// ClearWhiteboard removes everything on it.
func (r *Room) ClearWhiteboard(by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb == nil || !r.wb.open {
		return ErrWBClosed
	}
	if !r.mayManageWB(by) {
		return ErrWBDenied
	}
	r.wb.strokes, r.wb.notes, r.wb.points = nil, map[uint64]*WBNote{}, 0
	return nil
}

func inBoard(x, y int32) bool { return x >= 0 && y >= 0 && x < BoardW && y < BoardH }

// AddStroke commits a finished stroke and returns it with its id.
func (r *Room) AddStroke(s WBStroke) (WBStroke, error) {
	n := len(s.Points) / 2
	if len(s.Points)%2 != 0 || n < 1 || n > MaxWBStrokePoints || s.Color >= MaxWBPalette || s.Width < 1 || s.Width > MaxWBWidth || s.Tool > ToolHighlighter {
		return WBStroke{}, ErrWBStroke
	}
	for i := 0; i < len(s.Points); i += 2 {
		if !inBoard(s.Points[i], s.Points[i+1]) {
			return WBStroke{}, ErrWBStroke
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb == nil || !r.wb.open {
		return WBStroke{}, ErrWBClosed
	}
	if len(r.wb.strokes) >= MaxWBStrokes || r.wb.points+n > MaxWBPoints {
		return WBStroke{}, ErrWBFull
	}
	r.wb.nextID++
	s.ID = r.wb.nextID
	s.Points = append([]int32(nil), s.Points...)
	r.wb.strokes = append(r.wb.strokes, s)
	r.wb.points += n
	return s, nil
}

// EraseItem removes a stroke or a sticky note by id. Anyone drawing may
// erase, as on a shared board.
func (r *Room) EraseItem(id uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb == nil || !r.wb.open {
		return ErrWBClosed
	}
	for i, s := range r.wb.strokes {
		if s.ID == id {
			r.wb.points -= len(s.Points) / 2
			r.wb.strokes = append(r.wb.strokes[:i], r.wb.strokes[i+1:]...)
			return nil
		}
	}
	if _, ok := r.wb.notes[id]; ok {
		delete(r.wb.notes, id)
		return nil
	}
	return ErrWBMissing
}

// PutNote adds a sticky note (ID 0) or replaces one, and returns it.
func (r *Room) PutNote(n WBNote) (WBNote, error) {
	if !inBoard(n.X, n.Y) || n.Color >= MaxWBPalette || n.Text == "" || !utf8.ValidString(n.Text) || utf8.RuneCountInString(n.Text) > MaxWBNoteLen {
		return WBNote{}, ErrWBNote
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wb == nil || !r.wb.open {
		return WBNote{}, ErrWBClosed
	}
	if n.ID == 0 {
		if len(r.wb.notes) >= MaxWBNotes {
			return WBNote{}, ErrWBFull
		}
		r.wb.nextID++
		n.ID = r.wb.nextID
	} else if _, ok := r.wb.notes[n.ID]; !ok {
		return WBNote{}, ErrWBMissing
	}
	c := n
	r.wb.notes[n.ID] = &c
	return n, nil
}

// WhiteboardOpen reports whether a board is on, without copying it: asked
// for every live-preview frame.
func (r *Room) WhiteboardOpen() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.wb != nil && r.wb.open
}

// Whiteboard returns a copy of the board.
func (r *Room) Whiteboard() WBState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.wb == nil {
		return WBState{}
	}
	st := WBState{Open: r.wb.open, Opener: r.wb.opener}
	for _, s := range r.wb.strokes {
		s.Points = append([]int32(nil), s.Points...)
		st.Strokes = append(st.Strokes, s)
	}
	for _, n := range r.wb.notes {
		st.Notes = append(st.Notes, *n)
	}
	sort.Slice(st.Notes, func(i, j int) bool { return st.Notes[i].ID < st.Notes[j].ID })
	return st
}
