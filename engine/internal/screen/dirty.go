package screen

import (
	"image"
)

// Dirty-region detection.
//
// This is where the bytes are won. Typing a character changes a few hundred
// pixels out of two million; re-sending all of them costs roughly 65 KB
// against about 3 KB for the region that actually moved. On ordinary
// interaction that is a factor of twenty, and on a static screen it is the
// difference between a steady stream and complete silence.
//
// The screen is divided into tiles. Each tile is hashed, the hash compared
// with the previous capture, and the changed tiles merged into rectangles.
// Hashing beats comparing pixels directly because it is a single pass with
// no branch per pixel and the comparison afterwards is one 64-bit equality.

// TileSize is the granularity of change detection.
//
// The trade is straightforward and worth stating. Smaller tiles localise a
// change more tightly but cost more hashes and produce more, smaller
// rectangles — and every rectangle carries a 12-byte header plus a fresh
// JPEG header, so a hundred tiny rectangles is worse than one medium one.
// 32 is the point where, on a 1080p screen, a single-character edit resolves
// to one tile while a full-screen scroll still merges into a handful of
// rectangles rather than two thousand.
const TileSize = 32

// SampleStride subsamples within a tile when hashing.
//
// Reading every pixel of a 1080p frame is ~8 MB of memory traffic per
// capture, which at 20 fps is 160 MB/s spent purely to notice nothing
// changed. Sampling every 4th pixel on both axes reads a sixteenth of that
// and still catches any change large enough for a human to see, because a
// change too small to alter one sampled pixel in a 32x32 tile is a change
// too small to perceive.
//
// The blind spot this creates is small and exactly bounded, and the bound
// matters more than the size. Sampling every 4th pixel on both axes means
// every 4x4 block of the screen contains exactly one sampled pixel, so ANY
// change spanning a 4x4 block is caught with certainty. Only a change lying
// entirely within fewer than 4x4 pixels, and missing the one sample inside
// it, can hide. The verifier below closes even that, exactly and for free.
const SampleStride = 4

// VerifyRowsPerCapture is how many tile rows are re-checked at full
// precision on each capture, cycling through the screen.
//
// This is the mechanism that closes the sub-4x4 blind spot above, and it is
// worth being precise about why it replaced a periodic whole-screen frame.
//
// The old approach re-sent the ENTIRE SCREEN every 10 seconds on the theory
// that this repairs anything the sampling missed. Measured, that was 81% of
// all bytes in a realistic minute (1.24 MB of 1.54 MB) spent almost entirely
// on retransmitting pixels that had not changed, to fix a problem that
// usually did not exist.
//
// A verifier inverts that. Instead of blindly RESENDING a region in the hope
// it was stale, hash it EXACTLY (stride 1, every pixel) and compare with the
// last exact hash of the same region. If they match, nothing is sent at all.
// If they differ, the tile genuinely changed in a way subsampling missed, and
// only that tile is sent. The check therefore costs bytes only when it finds
// something, which on a static screen is never.
//
// At 2 rows per capture and 34 tile rows on a 1080p screen a full cycle takes
// 17 captures, under a second at 20 fps, against the 10 seconds the periodic
// full frame took. It is both far cheaper and roughly ten times faster to
// notice.
//
// The CPU is affordable because it covers a fraction of the screen: 2 rows of
// 1920px is ~123k pixels hashed exactly per capture, against ~130k sampled by
// the ordinary pass over the whole screen. It roughly doubles hashing work,
// which measured well under a millisecond per capture to begin with.
const VerifyRowsPerCapture = 2

// Detector finds what changed between captures.
type Detector struct {
	w, h   int
	cols   int
	rows   int
	hashes []uint64
	primed bool

	// exact holds a full-precision hash per tile, and verify is the cursor
	// of the rolling band being re-checked against it.
	exact  []uint64
	verify int

	// pending is what the VIEWER IS OWED, which is not the same thing as
	// what changed.
	//
	// A tile can be identical from one capture to the next and still be
	// wrong on the far canvas, because the frame carrying it was refused by
	// the byte budget. Comparing captures cannot possibly detect that: the
	// screen agrees with itself. So the encoder reports when a frame did not
	// go out, and those tiles stay dirty until one actually does.
	//
	// Without this the only repair was the next periodic full frame, which
	// left a visibly stale patch on screen for up to ten seconds, and did so
	// precisely when the link was already congested enough to drop frames.
	pending []bool
	owed    bool

	// found counts tiles the rolling verifier caught that the subsampled
	// pass missed. It should be near zero on ordinary content; a large
	// number means SampleStride is too coarse for this display.
	found int64
}

// NewDetector prepares a detector for a screen of the given size.
func NewDetector(w, h int) *Detector {
	cols := (w + TileSize - 1) / TileSize
	rows := (h + TileSize - 1) / TileSize
	return &Detector{
		w: w, h: h,
		cols: cols, rows: rows,
		hashes:  make([]uint64, cols*rows),
		exact:   make([]uint64, cols*rows),
		pending: make([]bool, cols*rows),
	}
}

// Found reports how many tiles the rolling verifier has caught that
// subsampled hashing missed.
func (d *Detector) Found() int64 { return d.found }

// Size reports the screen dimensions this detector was built for.
func (d *Detector) Size() (int, int) { return d.w, d.h }

// Reset forgets all history, so the next call reports everything changed.
func (d *Detector) Reset() { d.primed = false }

// hashTile computes a subsampled FNV-1a over one tile.
//
// FNV rather than a cryptographic hash: this is change detection, not
// integrity. Nobody is trying to forge a tile, and a 64-bit FNV over a few
// hundred sampled pixels collides far too rarely to matter against the cost
// of anything stronger on the hot path.
func hashTile(img *image.RGBA, x0, y0, x1, y1 int) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for y := y0; y < y1; y += SampleStride {
		row := img.Pix[y*img.Stride:]
		for x := x0; x < x1; x += SampleStride {
			i := x * 4
			// Red, green and blue. Alpha is skipped because a screen capture
			// is opaque and hashing a constant wastes a quarter of the work.
			h = (h ^ uint64(row[i])) * prime64
			h = (h ^ uint64(row[i+1])) * prime64
			h = (h ^ uint64(row[i+2])) * prime64
		}
	}
	return h
}

// hashTileExact reads every pixel of a tile. Used only by the rolling
// verifier, never by the per-capture pass over the whole screen.
func hashTileExact(img *image.RGBA, x0, y0, x1, y1 int) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for y := y0; y < y1; y++ {
		row := img.Pix[y*img.Stride:]
		for x := x0; x < x1; x++ {
			i := x * 4
			h = (h ^ uint64(row[i])) * prime64
			h = (h ^ uint64(row[i+1])) * prime64
			h = (h ^ uint64(row[i+2])) * prime64
		}
	}
	return h
}

func (d *Detector) tileBounds(c, r int) (int, int, int, int) {
	x0 := c * TileSize
	y0 := r * TileSize
	return x0, y0, min(x0+TileSize, d.w), min(y0+TileSize, d.h)
}

// Retry marks the tiles under these rectangles as still owed to the viewer,
// so a later capture re-sends them. The encoder calls this when the budget
// refuses a frame.
func (d *Detector) Retry(rects []image.Rectangle) {
	for _, rc := range rects {
		c0 := max(rc.Min.X/TileSize, 0)
		r0 := max(rc.Min.Y/TileSize, 0)
		c1 := min((rc.Max.X+TileSize-1)/TileSize, d.cols)
		r1 := min((rc.Max.Y+TileSize-1)/TileSize, d.rows)
		for r := r0; r < r1; r++ {
			for c := c0; c < c1; c++ {
				d.pending[r*d.cols+c] = true
				d.owed = true
			}
		}
	}
}

// primeExact records a full-precision hash of every tile. Called when the
// detector has just declared the whole screen changed, so the exact hashes
// start in agreement with what the viewer is about to receive.
func (d *Detector) primeExact(img *image.RGBA) {
	for r := 0; r < d.rows; r++ {
		for c := 0; c < d.cols; c++ {
			x0, y0, x1, y1 := d.tileBounds(c, r)
			d.exact[r*d.cols+c] = hashTileExact(img, x0, y0, x1, y1)
		}
	}
	d.verify = 0
}

// verifyBand re-checks one band of tile rows at full precision and reports
// whether it found a tile that changed without the subsampled pass noticing.
//
// It deliberately does NOT write d.exact. That array has exactly one writer,
// recordSent below, and the reason is a bug this had on the first attempt:
// when the verifier updated d.exact itself, a tile that changed normally and
// was sent normally still carried a stale exact hash until the cursor reached
// it, up to a full cycle later. The verifier then found the difference it had
// already been told about and sent the tile a SECOND time. Measured, that
// alone pushed a realistic minute from 1.54 MB to 2.54 MB and cut silent
// captures from 71% to 36%: the check meant to cost nothing became the single
// largest source of redundant bytes.
//
// With one writer the invariant is simple and checkable: d.exact is what the
// viewer was last sent, so any difference from it is genuinely unsent.
func (d *Detector) verifyBand(img *image.RGBA, changed []bool) bool {
	found := false
	for n := 0; n < VerifyRowsPerCapture; n++ {
		r := d.verify
		d.verify = (d.verify + 1) % d.rows
		for c := 0; c < d.cols; c++ {
			i := r*d.cols + c
			if changed[i] {
				continue // already going out; recordSent will refresh it
			}
			x0, y0, x1, y1 := d.tileBounds(c, r)
			if d.exact[i] != hashTileExact(img, x0, y0, x1, y1) {
				changed[i] = true
				found = true
				d.found++
			}
		}
	}
	return found
}

// recordSent refreshes the exact hash of every tile about to be transmitted,
// so d.exact always describes what the viewer holds.
//
// The cost is proportional to the area being sent, and that area is about to
// be JPEG encoded, which is more than an order of magnitude more expensive.
// On the common case, one keystroke, this hashes a single 32x32 tile.
func (d *Detector) recordSent(img *image.RGBA, changed []bool) {
	for r := 0; r < d.rows; r++ {
		for c := 0; c < d.cols; c++ {
			i := r*d.cols + c
			if !changed[i] {
				continue
			}
			x0, y0, x1, y1 := d.tileBounds(c, r)
			d.exact[i] = hashTileExact(img, x0, y0, x1, y1)
		}
	}
}

// Changed returns the rectangles that differ from the previous capture.
//
// A nil result means nothing changed, and nothing changed means nothing is
// sent — which is the single largest saving in the whole path, because most
// of the time a shared screen is somebody reading it.
func (d *Detector) Changed(img *image.RGBA) []image.Rectangle {
	b := img.Bounds()
	if b.Dx() != d.w || b.Dy() != d.h {
		// The screen was resized or the display changed. Everything is new.
		*d = *NewDetector(b.Dx(), b.Dy())
	}

	changed := make([]bool, d.cols*d.rows)
	any := false

	for r := 0; r < d.rows; r++ {
		y0 := r * TileSize
		y1 := min(y0+TileSize, d.h)
		for c := 0; c < d.cols; c++ {
			x0 := c * TileSize
			x1 := min(x0+TileSize, d.w)
			hv := hashTile(img, x0, y0, x1, y1)
			i := r*d.cols + c
			if !d.primed || d.hashes[i] != hv {
				changed[i] = true
				any = true
			}
			d.hashes[i] = hv
		}
	}

	if !d.primed {
		d.primed = true
		d.primeExact(img)
		clear(d.pending)
		d.owed = false
		return []image.Rectangle{image.Rect(0, 0, d.w, d.h)}
	}

	// Tiles the viewer is still owed from a refused frame. These are dirty
	// even though the screen itself has not moved.
	if d.owed {
		for i, p := range d.pending {
			if p && !changed[i] {
				changed[i] = true
				any = true
			}
		}
	}

	// The rolling exact re-check. Costs bytes only when it finds a real
	// change that subsampling missed.
	if d.verifyBand(img, changed) {
		any = true
	}

	// Everything reported now is assumed delivered. The encoder calls Retry
	// to put back whatever the budget actually refused, which is the only
	// way a tile becomes owed again.
	if d.owed {
		clear(d.pending)
		d.owed = false
	}

	if !any {
		return nil
	}
	d.recordSent(img, changed)
	return d.merge(changed)
}

// merge turns a grid of changed tiles into a small number of rectangles.
//
// Row-runs coalesced vertically: adjacent changed tiles in a row become one
// span, and a span directly below an identical span extends it. This is
// deliberately not an optimal rectangle cover — that problem is NP-hard and
// the optimum would save a few percent of pixels while costing far more than
// it saves to compute at capture rate.
func (d *Detector) merge(changed []bool) []image.Rectangle {
	type span struct{ c0, c1, r0, r1 int }
	var open []span
	var out []image.Rectangle

	flush := func(s span) {
		out = append(out, image.Rect(
			s.c0*TileSize,
			s.r0*TileSize,
			min(s.c1*TileSize, d.w),
			min(s.r1*TileSize, d.h),
		))
	}

	for r := 0; r < d.rows; r++ {
		var row []span
		c := 0
		for c < d.cols {
			if !changed[r*d.cols+c] {
				c++
				continue
			}
			start := c
			for c < d.cols && changed[r*d.cols+c] {
				c++
			}
			row = append(row, span{c0: start, c1: c, r0: r, r1: r + 1})
		}

		// Extend any open span that this row continues exactly.
		next := make([]span, 0, len(row))
		used := make([]bool, len(row))
		for _, o := range open {
			extended := false
			for i, s := range row {
				if !used[i] && s.c0 == o.c0 && s.c1 == o.c1 {
					o.r1 = s.r1
					next = append(next, o)
					used[i] = true
					extended = true
					break
				}
			}
			if !extended {
				flush(o)
			}
		}
		for i, s := range row {
			if !used[i] {
				next = append(next, s)
			}
		}
		open = next
	}
	for _, o := range open {
		flush(o)
	}
	return out
}

// TileStats reports how much of the screen changed, for telemetry and for
// the encoder's decision to send one full frame instead of many pieces.
type TileStats struct {
	Total   int
	Changed int
}

// Stats describes the last comparison.
func (d *Detector) Stats(rects []image.Rectangle) TileStats {
	total := d.cols * d.rows
	changed := 0
	for _, r := range rects {
		cw := (r.Dx() + TileSize - 1) / TileSize
		ch := (r.Dy() + TileSize - 1) / TileSize
		changed += cw * ch
	}
	if changed > total {
		changed = total
	}
	return TileStats{Total: total, Changed: changed}
}
