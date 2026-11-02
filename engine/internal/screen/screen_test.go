package screen

import (
	"image"
	"image/color"
	"math/rand"
	"testing"
	"time"
)

// These tests are measurements. Each one prints the bytes a realistic desktop
// scenario actually costs, so a change that makes the encoder worse shows up
// as a number moving rather than as a vague sense that things feel slower.

const (
	scrW = 1920
	scrH = 1080
)

// desktop paints something with the statistical character of a real screen:
// a flat background, a window, and text-like rows of small dark runs. JPEG
// cares enormously about this — noise costs far more than flat colour, and a
// test on random pixels would measure a screen nobody has.
func desktop() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, scrW, scrH))
	bg := color.RGBA{32, 38, 52, 255}
	for y := 0; y < scrH; y++ {
		for x := 0; x < scrW; x++ {
			img.SetRGBA(x, y, bg)
		}
	}
	// A window.
	win := color.RGBA{246, 248, 251, 255}
	for y := 140; y < 900; y++ {
		for x := 240; x < 1680; x++ {
			img.SetRGBA(x, y, win)
		}
	}
	// Text-like rows.
	ink := color.RGBA{40, 44, 56, 255}
	rng := rand.New(rand.NewSource(7))
	for row := 0; row < 30; row++ {
		y := 200 + row*22
		x := 280
		for x < 1600 {
			runLen := 6 + rng.Intn(50)
			for i := 0; i < runLen && x < 1600; i++ {
				for dy := 0; dy < 11; dy++ {
					img.SetRGBA(x, y+dy, ink)
				}
				x++
			}
			x += 6 + rng.Intn(14)
		}
	}
	return img
}

func clone(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func total(frames [][]byte) int {
	n := 0
	for _, f := range frames {
		n += len(f)
	}
	return n
}

// ---------------------------------------------------------------------------
// The measurements
// ---------------------------------------------------------------------------

// A screen nobody is touching must cost nothing at all. This is the single
// largest saving in the path and the easiest to regress.
func TestIdleScreenCostsNothing(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()

	first, err := enc.Encode(img, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("the first capture must send a full frame")
	}
	t.Logf("first frame (full 1920x1080): %d B", total(first))

	// Sixty more captures of an unchanged screen, three seconds at 20 fps.
	sent := 0
	for i := 0; i < 60; i++ {
		now = now.Add(50 * time.Millisecond)
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		sent += total(fs)
	}
	if sent != 0 {
		t.Fatalf("an idle screen sent %d B across 60 captures; it must send nothing", sent)
	}

	s := enc.Stats()
	t.Logf("60 idle captures: %d B, %d silent captures, %.2f ms per capture",
		sent, s.Silent, float64(s.EncodeNanos)/float64(s.Captures)/1e6)
}

// Typing is the scenario that decides whether remote control feels usable.
// One character changes a few hundred pixels out of two million.
func TestTypingSendsOnlyWhatChanged(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()
	// Prime with the initial full frame.
	firstFrames, _ := enc.Encode(img, now)
	fullCost := total(firstFrames)

	// Type twenty characters, one per capture.
	perChar := make([]int, 0, 20)
	x := 400
	for i := 0; i < 20; i++ {
		now = now.Add(50 * time.Millisecond)
		fill(img, image.Rect(x, 300, x+9, 311), color.RGBA{40, 44, 56, 255})
		x += 11
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		perChar = append(perChar, total(fs))
	}

	sum := 0
	for _, n := range perChar {
		sum += n
	}
	avg := sum / len(perChar)

	t.Logf("full frame:            %6d B", fullCost)
	t.Logf("per keystroke:         %6d B  (avg over 20)", avg)
	t.Logf("ratio:                 %6.1fx cheaper than resending the screen", float64(fullCost)/float64(avg))

	if avg == 0 {
		t.Fatal("typing produced no frames; change detection missed it")
	}
	// The whole justification for dirty regions. If a keystroke ever costs
	// more than a tenth of a full frame, the tiling has gone wrong.
	if float64(avg) > float64(fullCost)/10 {
		t.Fatalf("a keystroke cost %d B against a %d B full frame; dirty regions are not working",
			avg, fullCost)
	}
}

// A large change should collapse into one full frame rather than a swarm of
// rectangles, because per-rectangle headers and repeated JPEG tables cost
// more than the pixels saved past roughly a third of the screen.
func TestLargeChangeCollapsesToOneFrame(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()
	enc.Encode(img, now)

	// Repaint most of the window, as a scroll or a window switch would.
	now = now.Add(50 * time.Millisecond)
	fill(img, image.Rect(240, 140, 1680, 900), color.RGBA{250, 250, 250, 255})
	fs, err := enc.Encode(img, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("a large change produced %d frames; it should collapse to one", len(fs))
	}
	h, _, err := DecodeHeader(fs[0])
	if err != nil {
		t.Fatal(err)
	}

	// The collapsed frame must cover the change and NOT be marked full.
	//
	// FlagFull means "your whole canvas is now correct, discard what you
	// had", and a bounding box around one window does not do that. Setting
	// it here would tell a viewer that had missed an earlier frame elsewhere
	// on the screen that it was resynchronised when it was not, and the
	// stale patch would persist until the next periodic full frame.
	if h.Full() {
		t.Fatal("a bounding-box frame must not claim to be a full resync")
	}
	if int(h.W) >= scrW && int(h.H) >= scrH {
		t.Fatalf("collapsed to the whole screen (%dx%d); it should bound the change",
			h.W, h.H)
	}

	waste := float64(scrW*scrH-int(h.W)*int(h.H)) / float64(scrW*scrH)
	t.Logf("large change: one frame %dx%d at (%d,%d), %d B", h.W, h.H, h.X, h.Y, len(fs[0]))
	t.Logf("  avoided sending %.0f%% of the screen that did not change", waste*100)
}

// Loss recovery must cost the same as an ordinary frame. This is the
// property that a delta codec did not have, and the reason this design
// survives a dropped packet where that one collapsed.
func TestRecoveryCostsOneOrdinaryFrame(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()
	first, _ := enc.Encode(img, now)
	fullCost := total(first)

	// The viewer notices a gap and asks for a resync.
	enc.ForceFull()
	now = now.Add(50 * time.Millisecond)
	recovery, err := enc.Encode(img, now)
	if err != nil {
		t.Fatal(err)
	}
	cost := total(recovery)

	t.Logf("initial full frame: %d B", fullCost)
	t.Logf("recovery frame:     %d B", cost)

	if cost == 0 {
		t.Fatal("recovery produced nothing")
	}
	// Within a factor of two of an ordinary full frame. A delta codec's
	// recovery was a keyframe plus a cache rebuild, which is what made loss
	// catastrophic rather than merely expensive.
	if cost > fullCost*2 {
		t.Fatalf("recovery cost %d B against a %d B frame; it must not be a special expensive path",
			cost, fullCost)
	}
}

// Sequence numbers must survive the 16-bit wrap, or a session longer than
// 65535 frames reports a spurious enormous gap and resyncs forever.
func TestSequenceGapAcrossWrap(t *testing.T) {
	cases := []struct {
		prev, next uint16
		want       int
	}{
		{1, 2, 0},
		{1, 5, 3},
		{65535, 0, 0},
		{65534, 1, 2},
		{65535, 3, 3},
	}
	for _, c := range cases {
		if got := SeqGap(c.prev, c.next); got != c.want {
			t.Errorf("SeqGap(%d,%d) = %d, want %d", c.prev, c.next, got, c.want)
		}
	}
}

// The budget must be a real ceiling, and it must drop whole frames rather
// than degrade them.
func TestBudgetDropsFramesRatherThanQuality(t *testing.T) {
	img := desktop()
	// A deliberately tight budget: 40 kB/s, 60 kB burst.
	b := NewBudget(40_000, 60_000)
	enc := NewEncoder(scrW, scrH, b)
	now := time.Now()

	sent := 0
	for i := 0; i < 25; i++ {
		fill(img, image.Rect(300+i*20, 400, 300+i*20+400, 700), color.RGBA{uint8(i * 9), 120, 200, 255})
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		sent += total(fs)
		now = now.Add(50 * time.Millisecond)
	}

	s := enc.Stats()
	t.Logf("under a 40 kB/s budget: %d B sent, %d B refused, quality still %d",
		sent, s.BytesDropped, enc.Quality)

	if s.BytesDropped == 0 {
		t.Fatal("the budget never refused anything; it is not a ceiling")
	}
	if enc.Quality != 72 {
		t.Fatal("the encoder lowered quality under pressure; it must drop frames instead")
	}
}

// A malformed frame must never panic the viewer. This is reachable by
// anything that can send bytes.
func TestDecoderSurvivesGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(40))
		rng.Read(b)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic decoding % x: %v", b, p)
				}
			}()
			DecodeHeader(b)
		}()
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{Flags: FlagFull, Seq: 40000, X: 1900, Y: 1000, W: 20, H: 80}
	buf := make([]byte, HeaderLen+3)
	h.Encode(buf)
	got, payload, err := DecodeHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("round trip: got %+v want %+v", got, h)
	}
	if len(payload) != 3 {
		t.Fatalf("payload length %d, want 3", len(payload))
	}
}

// ---------------------------------------------------------------------------
// The headline: a realistic minute, priced
// ---------------------------------------------------------------------------

func TestRealisticMinuteBudget(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()
	enc.Encode(img, now)

	// One minute at 20 fps. A support session is mostly reading, punctuated
	// by typing and the occasional scroll: 70% idle, 25% typing, 5% scroll.
	rng := rand.New(rand.NewSource(11))
	sent := 0
	for i := 0; i < 1200; i++ {
		now = now.Add(50 * time.Millisecond)
		switch r := rng.Float64(); {
		case r < 0.70:
			// nothing happens
		case r < 0.95:
			x := 300 + rng.Intn(1200)
			y := 200 + rng.Intn(600)
			fill(img, image.Rect(x, y, x+9, y+11), color.RGBA{40, 44, 56, 255})
		default:
			fill(img, image.Rect(240, 140, 1680, 900), color.RGBA{
				uint8(230 + rng.Intn(20)), 244, 250, 255})
		}
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		sent += total(fs)
	}

	s := enc.Stats()
	kbps := float64(sent) * 8 / 60 / 1000
	t.Logf("")
	t.Logf("  ONE MINUTE OF A REALISTIC SUPPORT SESSION, 1920x1080 @ 20 fps")
	t.Logf("  ────────────────────────────────────────────────────────────")
	t.Logf("  captures examined     %6d", s.Captures)
	t.Logf("  sent nothing at all   %6d  (%.0f%% of captures)",
		s.Silent, 100*float64(s.Silent)/float64(s.Captures))
	t.Logf("  frames emitted        %6d  (%d full)", s.Frames, s.FullFrames)
	t.Logf("  total bytes           %6d B  (%.2f MB)", sent, float64(sent)/1e6)
	t.Logf("  sustained             %6.0f kbps", kbps)
	t.Logf("  cpu per capture       %6.2f ms",
		float64(s.EncodeNanos)/float64(s.Captures)/1e6)
	t.Logf("  verifier finds        %6d tiles subsampling missed", s.Verified)
	t.Logf("  captures throttled    %6d", s.Throttled)
	t.Logf("  bytes per frame       %6d B average", sent/max(int(s.Frames), 1))
	t.Logf("")
	t.Logf("  Against the same minute before this package was optimised:")
	t.Logf("    2075371 B / 277 kbps, 49 full frames")
	t.Logf("    -> %.0f%% fewer bytes", 100*(1-float64(sent)/2075371))
	t.Logf("")

	if sent == 0 {
		t.Fatal("a minute of activity produced nothing")
	}
}

func BenchmarkDetectorIdle(b *testing.B) {
	img := desktop()
	d := NewDetector(scrW, scrH)
	d.Changed(img)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Changed(img)
	}
}

func BenchmarkEncodeFullFrame(b *testing.B) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.det.Reset()
		enc.Encode(img, now)
	}
}

// ---------------------------------------------------------------------------
// The two mechanisms that replaced the periodic full frame
// ---------------------------------------------------------------------------

// A change too small for subsampled hashing to see must still reach the
// viewer, and must do so without resending the screen.
//
// This is the case the 10-second full frame used to cover at a cost of 81% of
// all traffic. Stride 4 guarantees any change spanning a 4x4 block is caught
// immediately, so the only thing that can hide is smaller than that; the
// rolling exact verifier must find it within one cycle.
func TestVerifierCatchesSubSampleChange(t *testing.T) {
	img := desktop()
	enc := NewEncoder(scrW, scrH, nil)
	enc.FullFrameEvery = 0 // prove the verifier does this, not a full frame
	now := time.Now()
	enc.Encode(img, now)

	// A 2x2 change placed to miss every sampled pixel. Sampling reads x and y
	// where both are multiples of SampleStride, so an odd-offset 2x2 block
	// touches none of them.
	x, y := 601, 401
	fill(img, image.Rect(x, y, x+2, y+2), color.RGBA{255, 0, 0, 255})

	// It is invisible to the fast path, by construction.
	now = now.Add(50 * time.Millisecond)
	if fs, _ := enc.Encode(img, now); len(fs) != 0 {
		t.Log("note: subsampled pass happened to catch it; the test still holds")
	}

	// Within one verify cycle it must be found and sent.
	rows := (scrH + TileSize - 1) / TileSize
	cycle := (rows+VerifyRowsPerCapture-1)/VerifyRowsPerCapture + 1
	bytes, frames := 0, 0
	for i := 0; i < cycle; i++ {
		now = now.Add(50 * time.Millisecond)
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		frames += len(fs)
		bytes += total(fs)
	}
	if frames == 0 {
		t.Fatal("a sub-sample change was never detected; the viewer is silently stale")
	}

	full := scrW * scrH
	if bytes > full/50 {
		t.Fatalf("repairing a 2x2 change cost %d B; it must not approach a full frame (%d B)",
			bytes, full)
	}
	t.Logf("2x2 change invisible to stride-%d sampling:", SampleStride)
	t.Logf("  found within        %d captures (%.0f ms at 20 fps)", cycle, float64(cycle)*50)
	t.Logf("  repaired with       %d frame(s), %d B", frames, bytes)
	t.Logf("  the old mechanism   1 full frame, up to 10 s late, 206158 B")
}

// A frame the budget refuses leaves the viewer stale in that region, and no
// comparison of captures can ever discover it, because the screen agrees with
// itself. The encoder must remember the debt and repay it.
func TestRefusedFrameIsRetriedNotForgotten(t *testing.T) {
	img := desktop()
	// Enough burst for the initial screen, then nothing to spare.
	b := NewBudget(20_000, 210_000)
	enc := NewEncoder(scrW, scrH, b)
	enc.FullFrameEvery = 0 // no full frame may paper over the gap
	now := time.Now()
	enc.Encode(img, now) // spends most of the burst

	// One change, which will not be affordable immediately.
	region := image.Rect(400, 300, 1400, 800)
	fill(img, region, color.RGBA{200, 30, 30, 255})

	now = now.Add(50 * time.Millisecond)
	fs, err := enc.Encode(img, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Skip("budget admitted the change immediately; nothing to retry")
	}
	if enc.Stats().BytesDropped == 0 {
		t.Fatal("nothing was sent and nothing was refused")
	}

	// The screen is now STATIC. Only the debt can produce bytes.
	got := 0
	for i := 0; i < 400 && got == 0; i++ {
		now = now.Add(50 * time.Millisecond)
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		got += total(fs)
	}
	if got == 0 {
		t.Fatal("a refused region was never resent; the viewer stays wrong forever")
	}
	t.Logf("refused region repaid without any further screen change: %d B", got)
}

// A frame larger than the budget burst can never be admitted. If the encoder
// keeps offering it whole, it is refused forever and the viewer never sees
// the screen at all. It must be split into pieces that fit.
func TestOversizeFrameIsSplitNotStarved(t *testing.T) {
	img := desktop()
	// A burst far smaller than a whole-screen frame (~206 kB).
	b := NewBudget(80_000, 60_000)
	enc := NewEncoder(scrW, scrH, b)
	now := time.Now()

	sent, captures := 0, 0
	for i := 0; i < 200 && sent < 150_000; i++ {
		fs, err := enc.Encode(img, now)
		if err != nil {
			t.Fatal(err)
		}
		sent += total(fs)
		captures++
		now = now.Add(50 * time.Millisecond)
	}

	if sent == 0 {
		t.Fatal("nothing was ever sent: an oversize frame starved the session")
	}
	s := enc.Stats()
	t.Logf("whole screen delivered through a %d B burst:", b.Burst())
	t.Logf("  bytes sent        %d B in %d captures", sent, captures)
	t.Logf("  captures skipped  %d (waiting for budget, no CPU spent)", s.Throttled)
	if s.Throttled == 0 {
		t.Fatal("the encoder never paced itself; it re-encoded on every capture")
	}
}
