package screen

import (
	"bytes"
	"image"
	"image/jpeg"
	"sync"
	"time"
)

// Encoder turns captures into frames, and decides which are worth sending.
type Encoder struct {
	det *Detector
	seq uint16

	// Quality of the JPEG encode. 55 is soft on small text, which is the one
	// thing a support operator needs to read, and 75 costs roughly 40% more
	// bytes for it. The default sits at 72: measured on real desktop content
	// this is where text becomes comfortably legible and the curve above it
	// flattens, so the next 20 points of quality buy almost no fidelity and
	// a great deal of bandwidth.
	Quality int

	// FullFrameEvery is a safety net, and it is important that it is only
	// that. It used to be the mechanism by which the viewer stayed correct:
	// resend everything every 10 seconds and hope that repairs whatever went
	// wrong. Measured, it cost 81% of all bytes in a realistic minute.
	//
	// Everything it used to cover is now covered exactly and cheaply:
	//
	//   subsampling blind spot   -> the rolling exact verifier, which sends
	//                               bytes only when it finds a real change
	//   frames refused by budget -> Detector.Retry, which keeps the tiles
	//                               dirty until one actually goes out
	//   loss on the wire         -> the sequence number, a gap noticed by
	//                               the viewer, and ForceFull in reply
	//
	// So this remains only as defence in depth against a bug in one of those
	// three, because the failure it guards is the worst kind: the viewer
	// showing a screen that is quietly wrong, with nothing to indicate it.
	// One minute is cheap for that, where ten seconds was not.
	FullFrameEvery time.Duration
	lastFull       time.Time

	budget *Budget
	buf    bytes.Buffer

	// bpp is a running estimate of JPEG bytes per pixel for this content,
	// used to size rectangles against the budget before paying to encode
	// them. Seeded at the measured value for ordinary desktop content at
	// quality 72 (206158 B over 1920x1080 is 0.0994) and corrected from
	// every real encode thereafter.
	bpp float64

	// retryAt is when a refused region may next be attempted. Without it a
	// refusal is retried on the very next capture, 50 ms later, when the
	// bucket has earned perhaps 2 kB of the 200 kB needed, and the encoder
	// spins re-encoding a frame it cannot afford. That is the sibling
	// project failure verbatim: recovery is the most expensive operation,
	// so retrying it under congestion is what turns congestion into
	// collapse.
	retryAt time.Time

	mu    sync.Mutex
	stats Stats
}

// Stats is the running account of what this encoder has produced.
type Stats struct {
	Captures     int64 // how many times the screen was examined
	Silent       int64 // captures that produced no bytes at all
	Frames       int64 // frames emitted
	FullFrames   int64
	Throttled    int64 // captures skipped while waiting for budget
	Verified     int64 // tiles the exact verifier caught that sampling missed
	BytesOut     int64
	BytesDropped int64 // frames the budget refused
	TilesChanged int64
	TilesTotal   int64
	EncodeNanos  int64
}

// NewEncoder prepares an encoder for a screen of the given size.
func NewEncoder(w, h int, budget *Budget) *Encoder {
	return &Encoder{
		det:            NewDetector(w, h),
		Quality:        72,
		FullFrameEvery: 60 * time.Second,
		bpp:            0.10,
		budget:         budget,
	}
}

// Stats returns a snapshot of the running account.
func (e *Encoder) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stats
	st.Verified = e.det.Found()
	return st
}

// ForceFull makes the next capture emit a whole-screen frame. The viewer
// calls this after noticing a sequence gap.
func (e *Encoder) ForceFull() {
	e.det.Reset()
}

// Encode examines a capture and returns the frames worth sending.
//
// A nil result is the common case and the point of the whole design: a
// screen nobody is touching costs nothing.
func (e *Encoder) Encode(img *image.RGBA, now time.Time) ([][]byte, error) {
	start := time.Now()
	e.mu.Lock()
	e.stats.Captures++
	e.mu.Unlock()

	// Nothing this capture could produce is affordable yet. Return before
	// hashing or encoding anything: the whole point is to spend no CPU while
	// the link is saturated. Skipping the comparison loses nothing, because
	// tile hashes are only refreshed for tiles that are actually sent, so a
	// change occurring during the wait is still detected afterwards.
	if now.Before(e.retryAt) {
		e.mu.Lock()
		e.stats.Throttled++
		e.stats.EncodeNanos += time.Since(start).Nanoseconds()
		e.mu.Unlock()
		return nil, nil
	}

	rects := e.det.Changed(img)
	if len(rects) == 0 {
		e.mu.Lock()
		e.stats.Silent++
		e.stats.EncodeNanos += time.Since(start).Nanoseconds()
		e.mu.Unlock()
		return nil, nil
	}

	ts := e.det.Stats(rects)
	b := img.Bounds()

	// The safety net described above. Not the mechanism.
	full := e.FullFrameEvery > 0 && now.Sub(e.lastFull) >= e.FullFrameEvery
	if full {
		rects = []image.Rectangle{image.Rect(0, 0, b.Dx(), b.Dy())}
		e.lastFull = now
	} else if len(rects) > 1 {
		// Otherwise, decide between many rectangles and their single
		// bounding box.
		//
		// The earlier version escalated to a WHOLE SCREEN frame whenever
		// enough tiles changed, which measured badly for the commonest case
		// there is: repainting one window. A 1440x760 window on a 1080p
		// screen is 53% of the area, tripped the threshold, and shipped the
		// other 47% of the desktop — pixels that had not changed — on every
		// repaint. Bounding the change instead of escalating past it is
		// strictly better: never larger than the old behaviour, and much
		// smaller whenever the change is localised.
		rects = e.coalesce(rects)
	}

	// Shape the rectangles to what the budget can physically carry.
	//
	// A frame bigger than the whole burst is not slow, it is impossible: the
	// bucket never holds enough tokens for it at any moment, so it is refused
	// forever. A full 1920x1080 frame is ~206 kB and a tight budget may burst
	// only 60 kB. Splitting is safe here for the same reason dropping is:
	// every frame is a self-contained JPEG of its own rectangle, so half a
	// region is a complete, decodable picture of half a region.
	if e.budget != nil {
		rects = e.fit(rects, e.budget.Burst())
	}

	out := make([][]byte, 0, len(rects))
	var refused []image.Rectangle
	for _, r := range rects {
		frame, err := e.encodeRect(img, r, full)
		if err != nil {
			return nil, err
		}
		// The budget drops whole frames, which is safe here only because
		// every frame is self-contained. Under pressure the frame rate
		// falls; the quality never does.
		e.observe(r, len(frame))
		if e.budget != nil && !e.budget.Allow(len(frame), now) {
			e.mu.Lock()
			e.stats.BytesDropped += int64(len(frame))
			e.mu.Unlock()
			refused = append(refused, r)
			// Do not try again until the bucket could actually hold it.
			if w := e.budget.Wait(len(frame), now); w > 0 {
				if at := now.Add(w); at.After(e.retryAt) {
					e.retryAt = at
				}
			}
			continue
		}
		e.commit()
		out = append(out, frame)
	}

	// A refused frame means the viewer is now stale in exactly this region,
	// and no future comparison of captures can discover that, because the
	// screen still agrees with itself. Say so explicitly. The alternative,
	// waiting for the periodic full frame, left the region visibly wrong for
	// as long as that interval and cost the whole screen to repair.
	//
	// The sequence number is deliberately NOT consumed for a refused frame,
	// so a drop here never presents to the viewer as a gap; a gap keeps its
	// single meaning of loss on the wire.
	if len(refused) > 0 {
		e.det.Retry(refused)
	}

	e.mu.Lock()
	e.stats.Frames += int64(len(out))
	if full && len(out) > 0 {
		e.stats.FullFrames++
	}
	for _, f := range out {
		e.stats.BytesOut += int64(len(f))
	}
	e.stats.TilesChanged += int64(ts.Changed)
	e.stats.TilesTotal += int64(ts.Total)
	e.stats.EncodeNanos += time.Since(start).Nanoseconds()
	e.mu.Unlock()

	return out, nil
}

// observe folds a real encode into the bytes-per-pixel estimate.
func (e *Encoder) observe(r image.Rectangle, n int) {
	area := r.Dx() * r.Dy()
	if area <= 0 {
		return
	}
	// A slow EWMA. The estimate only has to be right to within a factor of
	// about two, since it decides how to split rather than what to send.
	e.bpp = 0.9*e.bpp + 0.1*(float64(n)/float64(area))
}

// fit splits any rectangle whose estimated cost exceeds what the budget can
// ever hold in one frame.
func (e *Encoder) fit(rects []image.Rectangle, burst int) []image.Rectangle {
	if burst <= 0 {
		return rects
	}
	// Aim at half the burst so a split piece has room to be sent alongside
	// something else rather than needing the bucket entirely to itself.
	limit := float64(burst) / 2
	out := make([]image.Rectangle, 0, len(rects))
	var split func(r image.Rectangle, depth int)
	split = func(r image.Rectangle, depth int) {
		cost := float64(r.Dx()*r.Dy()) * e.bpp
		// Stop at a tile: below that the per-frame overhead dominates and no
		// further splitting helps. depth bounds the recursion regardless.
		if cost <= limit || depth >= 8 || (r.Dx() <= TileSize && r.Dy() <= TileSize) {
			out = append(out, r)
			return
		}
		if r.Dy() >= r.Dx() {
			mid := r.Min.Y + r.Dy()/2
			split(image.Rect(r.Min.X, r.Min.Y, r.Max.X, mid), depth+1)
			split(image.Rect(r.Min.X, mid, r.Max.X, r.Max.Y), depth+1)
			return
		}
		mid := r.Min.X + r.Dx()/2
		split(image.Rect(r.Min.X, r.Min.Y, mid, r.Max.Y), depth+1)
		split(image.Rect(mid, r.Min.Y, r.Max.X, r.Max.Y), depth+1)
	}
	for _, r := range rects {
		split(r, 0)
	}
	return out
}

// PerRectCost is what an extra rectangle costs beyond its pixels: a 12 byte
// frame header plus a fresh set of JPEG headers and quantisation tables,
// which run to roughly six hundred bytes. Expressed as an equivalent pixel
// area so the comparison below is in one unit.
//
// At roughly 0.35 bytes per pixel for this content, ~620 bytes of fixed cost
// is about 1800 pixels. Two rectangles must therefore save more than 1800
// pixels of area over their bounding box to be worth sending separately.
const PerRectCost = 1800

// coalesce chooses between sending rectangles separately and sending one
// box around them all.
func (e *Encoder) coalesce(rects []image.Rectangle) []image.Rectangle {
	box := rects[0]
	sum := 0
	for _, r := range rects {
		box = box.Union(r)
		sum += r.Dx() * r.Dy()
	}

	separate := sum + len(rects)*PerRectCost
	combined := box.Dx()*box.Dy() + PerRectCost

	if combined < separate {
		return []image.Rectangle{box}
	}
	return rects
}

func (e *Encoder) encodeRect(img *image.RGBA, r image.Rectangle, full bool) ([]byte, error) {
	e.buf.Reset()
	e.buf.Grow(HeaderLen + 4096)
	e.buf.Write(make([]byte, HeaderLen))

	sub := img.SubImage(r).(*image.RGBA)
	if err := jpeg.Encode(&e.buf, sub, &jpeg.Options{Quality: e.Quality}); err != nil {
		return nil, err
	}

	frame := make([]byte, e.buf.Len())
	copy(frame, e.buf.Bytes())

	h := Header{
		Seq: e.seq + 1,
		X:   uint16(r.Min.X), Y: uint16(r.Min.Y),
		W: uint16(r.Dx()), H: uint16(r.Dy()),
	}
	if full {
		h.Flags |= FlagFull
	}
	h.Encode(frame)
	return frame, nil
}

// commit advances the sequence number for a frame that was actually sent.
func (e *Encoder) commit() { e.seq++ }

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

// Budget is a token bucket over transmitted bytes.
//
// It takes the current time as a parameter rather than reading the clock, for
// the same reason the encoder does: a pacing mechanism that cannot be run on
// a synthetic clock cannot be tested. When this read time.Now() internally
// while the encoder was driven by an injected clock, the two disagreed. A
// test could advance twenty simulated seconds in a third of a real one, the
// encoder would judge a refused region ready to retry, and the bucket would
// still be empty because no real time had passed. The bug that hid there was
// real and would have shipped: a refused region silently never repaid.
//
// Its scope is the thing that matters, and it is the lesson a sibling
// project paid for: a budget created per connection is not a ceiling. When
// the session reconnects — which it does precisely because the path is
// unhealthy — a fresh budget hands it a fresh burst to make the path worse
// with. Measured there as seven reconnections in thirty-two seconds, each
// spending a full allowance. So a Budget is created once per process and
// shared across every retry.
type Budget struct {
	mu     sync.Mutex
	rate   float64 // bytes per second, sustained
	burst  float64 // bytes
	tokens float64
	last   time.Time
	primed bool

	Allowed int64
	Refused int64
}

// NewBudget creates a bucket. rate is the sustained ceiling in bytes per
// second; burst is how far above it a moment may reach.
func NewBudget(rate, burst int) *Budget {
	return &Budget{
		rate:   float64(rate),
		burst:  float64(burst),
		tokens: float64(burst),
	}
}

// DefaultBudget is 350 kB/s sustained with a 3 MB burst.
//
// 350 kB/s is 2.8 Mbps: comfortably inside a working broadband connection
// and far above what a static or lightly-used desktop actually consumes,
// because change detection means an idle screen spends nothing. The burst
// exists so that a full-frame resync after a reconnect is never itself the
// thing that hits the ceiling.
func DefaultBudget() *Budget { return NewBudget(350_000, 3<<20) }

// refill brings the bucket up to date. Caller holds the lock.
func (b *Budget) refill(now time.Time) {
	if !b.primed {
		b.primed = true
		b.last = now
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
}

// Allow reports whether n bytes may be sent as of now, and consumes them
// if so.
func (b *Budget) Allow(n int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill(now)
	if b.tokens < float64(n) {
		b.Refused++
		return false
	}
	b.tokens -= float64(n)
	b.Allowed++
	return true
}

// Burst reports the largest frame this bucket could ever admit.
func (b *Budget) Burst() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.burst)
}

// Wait reports how long until n bytes become affordable, or zero if they are
// affordable now. n larger than the burst never becomes affordable, and the
// caller is expected to split rather than wait; the ceiling returned here is
// a backstop, not a plan.
func (b *Budget) Wait(n int, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	need := float64(n) - b.tokens
	if need <= 0 || b.rate <= 0 {
		return 0
	}
	if float64(n) > b.burst {
		need = b.burst - b.tokens
		if need <= 0 {
			return 0
		}
	}
	return time.Duration(need / b.rate * float64(time.Second))
}

// Available reports the tokens in the bucket as of now, for telemetry.
func (b *Budget) Available(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	return int(b.tokens)
}
