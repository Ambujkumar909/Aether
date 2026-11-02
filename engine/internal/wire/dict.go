package wire

import "sync"

// MaxDictEntries bounds interning memory per direction per connection.
// 4096 entries at a ~40 byte average is ~160 KB worst case, and a real room
// never approaches it: entries are participant IDs, track IDs, and codec
// names, not user content.
const MaxDictEntries = 4096

// Dict is a session-scoped, append-only string interning table.
//
// CORRECTNESS NOTE, and this is the part that is easy to get wrong:
// a connection needs TWO dictionaries, not one. If both peers interned into
// a single shared table, two frames crossing in flight would each claim the
// same next index and the tables would silently diverge -- after which every
// handle resolves to the wrong string and the session is corrupt in a way
// that looks like application-level nonsense rather than a codec bug.
//
// Instead each direction owns its own table. A dictionary is mutated only by
// frames travelling one way, and the transport (WebSocket, or an ordered
// SCTP data channel) guarantees ordering within a direction. The sender's Tx
// table and the receiver's Rx table for that direction therefore observe an
// identical sequence of Intern calls and cannot diverge.
//
// Eviction is deliberately absent. Any eviction policy must be executed
// identically on both sides from partial information, and the failure mode
// of getting that wrong is the same silent corruption. Instead the table is
// append-only and simply stops interning once full: both sides apply that
// rule deterministically, and post-cap strings fall back to literals, which
// costs bytes but can never desynchronise.
type Dict struct {
	mu     sync.RWMutex
	byStr  map[string]uint32
	byHand []string
	sealed bool
}

func NewDict() *Dict {
	return &Dict{byStr: make(map[string]uint32, 64)}
}

// NewDictPair returns the (tx, rx) tables for one connection. See the type
// comment for why these must be distinct.
func NewDictPair() (tx, rx *Dict) { return NewDict(), NewDict() }

// Handle returns the existing handle for s, if interned.
func (d *Dict) Handle(s string) (uint32, bool) {
	d.mu.RLock()
	h, ok := d.byStr[s]
	d.mu.RUnlock()
	return h, ok
}

// Lookup resolves a handle to its string.
func (d *Dict) Lookup(h uint32) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if int(h) >= len(d.byHand) {
		return "", false
	}
	return d.byHand[h], true
}

// Intern appends s if absent and the table is not sealed. Interning a string
// longer than MaxInternedString is refused so the table cannot be inflated
// by a hostile peer.
func (d *Dict) Intern(s string) {
	if len(s) > MaxInternedString {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sealed {
		return
	}
	if _, ok := d.byStr[s]; ok {
		return
	}
	if len(d.byHand) >= MaxDictEntries {
		d.sealed = true
		return
	}
	d.byStr[s] = uint32(len(d.byHand))
	d.byHand = append(d.byHand, s)
}

// Len reports the number of interned entries.
func (d *Dict) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.byHand)
}

// Preload seeds both directions with protocol vocabulary that is certain to
// appear, so the very first frames referencing them already pay 1 byte
// instead of a literal. These must be seeded in the same order on both
// peers; the client mirrors this list verbatim.
var Preload = []string{
	"opus", "av1", "vp9", "vp8", "h264",
	"cam", "mic", "screen", "screen-audio",
	"f", "h", "q", // full / half / quarter spatial layers
	"active", "muted", "hidden", "paused",
}

// NewPreloadedPair returns a dictionary pair already seeded with Preload.
func NewPreloadedPair() (tx, rx *Dict) {
	tx, rx = NewDictPair()
	for _, s := range Preload {
		tx.Intern(s)
		rx.Intern(s)
	}
	return
}
