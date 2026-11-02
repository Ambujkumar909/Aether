package signal

import (
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// Together mode on the wire (OpTogether 0x128); web/src/together.js is the
// other half.
//
//	client -> server
//	  set   kind 0 | on 1 bit | pad | scene uvarint
//	  mask  kind 1 | pad | rle blob (a 64 x 36 outline, run-length coded)
//	server -> client
//	  state kind 0 | on 1 bit | pad | [if on] by str, scene uvarint
//	  mask  kind 1 | pad | from str | rle blob
//
// Outlines are relayed to everyone else, only while Together mode is on, at
// most 12 a second each (room.MaskRate), at most MaxMaskBytes each. A mask
// is self-contained: a lost one costs a tenth of a second of outline.

const (
	togState = 0
	togMask  = 1

	MaxMaskBytes = 1024
)

func encodeTogether(t room.TogetherState) func(*wire.Writer) {
	return func(w *wire.Writer) {
		w.WriteHeader(wire.OpTogether, 0, false)
		w.Bits(togState, 3)
		w.Bool(t.On)
		w.Bits(0, 4)
		if t.On {
			w.Str(t.By)
			w.Uvarint(uint64(t.Scene))
		}
	}
}

func (g *Gateway) handleTogether(rm *room.Room, p *room.Participant, r *wire.Reader) error {
	kind := r.Bits(3)
	switch kind {
	case togState:
		on := r.Bool()
		r.Bits(4)
		scene := r.Uvarint()
		if err := r.Err(); err != nil {
			return err
		}
		if scene > 255 {
			return nil
		}
		if changed, err := rm.SetTogether(p.ID, on, uint8(scene)); err == nil && changed {
			g.announceAll(rm, encodeTogether(rm.Together()))
		}
	case togMask:
		r.Bits(5)
		rle := r.Blob()
		if err := r.Err(); err != nil {
			return err
		}
		if len(rle) == 0 || len(rle) > MaxMaskBytes || !rm.MaskAllowed(p.ID, time.Now()) {
			return nil
		}
		mask := append([]byte(nil), rle...)
		g.announce(rm, p.ID, func(w *wire.Writer) {
			w.WriteHeader(wire.OpTogether, 0, false)
			w.Bits(togMask, 3)
			w.Bits(0, 5)
			w.Str(p.ID)
			w.Blob(mask)
		})
	}
	return nil
}
