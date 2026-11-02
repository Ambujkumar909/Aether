// Package sfu contains the media routing engine: what gets forwarded, at
// what layer, to whom, and why.
package sfu

import (
	"math"
	"sort"
)

// The allocator answers the only question that matters on the downlink:
// given a receiver with a finite estimate, which layer of which track should
// each of its subscriptions receive?
//
// Every product gets this wrong in the same way. They allocate by a fixed
// priority list, or round-robin, or "whatever the encoder produced", and
// then they send a 640x360 stream to a tile the compositor is drawing at
// 160x90. Those bits are encoded, transmitted, decrypted, decoded, scaled
// down, and discarded. They were never capable of becoming visible.
//
// This allocator is built on one principle: the utility of a layer saturates
// at the point where its resolution exceeds what the receiver will actually
// draw. Past that point additional bits have a marginal utility of exactly
// zero, and no amount of available bandwidth should buy them.

// Layer is one encoding rung the SFU can forward for a track.
type Layer struct {
	Spatial  uint8 // 0 = lowest
	Temporal uint8
	Width    int
	Height   int
	FPS      float64

	// Bitrate is the measured, not the configured, rate for this rung.
	// Configured rates lie; a static slide deck at "1.2 Mbps" transmits
	// almost nothing, and an allocator that budgets against the nameplate
	// figure will starve every other track for capacity that was never
	// going to be used.
	Bitrate int
}

// Pixels is the layer's spatial cost.
func (l Layer) Pixels() int { return l.Width * l.Height }

// Subscription is one receiver's interest in one track.
type Subscription struct {
	TrackID string
	Layers  []Layer

	// ViewportW/H are the CSS pixels the receiver will actually paint,
	// multiplied by devicePixelRatio. This is reported by the client from a
	// ResizeObserver, not guessed from a layout name, because "gallery tile"
	// means something different on a phone and a 34 inch monitor.
	ViewportW, ViewportH int

	// Visible is false when the element is scrolled out of view, in a
	// collapsed panel, or in a background tab. An invisible subscription is
	// allocated exactly zero bits -- not a low layer, zero.
	Visible bool

	// SpeakerRank is 0 for the dominant speaker, 1 for the next, and so on.
	// Negative means not ranked.
	SpeakerRank int

	// Pinned means the user explicitly asked for this feed. Explicit intent
	// outranks every heuristic in this file.
	Pinned bool

	// IsScreenShare content is judged on spatial detail rather than motion
	// smoothness: text must be legible, and a shared terminal at 5 fps is
	// vastly more useful than a blurry one at 30.
	IsScreenShare bool
}

// Allocation is the decision for one subscription.
type Allocation struct {
	TrackID  string
	Layer    Layer
	Selected bool // false means forward nothing at all
	Bitrate  int
	Reason   string
}

// Budget describes the receiver's capacity.
type Budget struct {
	// EstimateBps is the congestion controller's current view of the path.
	EstimateBps int

	// HeadroomRatio is the fraction of the estimate left unallocated so that
	// a motion burst has somewhere to go without immediately inducing loss.
	// This is not timidity: a scene-change spike that has to wait for the
	// next allocation cycle shows up to the user as a smeared frame, which
	// is precisely the artefact this system exists to eliminate.
	HeadroomRatio float64

	// MinUtilityPerMbps is the exchange rate below which an upgrade is not
	// worth buying even when the capacity is sitting idle.
	//
	// Without this floor, "unlimited bandwidth" degenerates into "buy every
	// upgrade with any positive utility at all", and the allocator will
	// happily spend four times the bitrate to raise a 160x90 thumbnail from
	// 15 fps to 30, a difference nobody has ever noticed on a tile that
	// size. Spatial saturation alone does not catch this, because the
	// temporal term keeps earning after the spatial term has flatlined.
	//
	// The floor is what makes idle capacity meaningful rather than merely
	// unspent. Bandwidth we decline to waste on an imperceptible upgrade is
	// bandwidth available the instant someone waves a hand or a slide
	// transitions, and a sharp burst is worth incomparably more than a
	// smoother thumbnail. Unallocated is not idle; it is loaded.
	MinUtilityPerMbps float64
}

func (b Budget) minDensity() float64 {
	if b.MinUtilityPerMbps > 0 {
		return b.MinUtilityPerMbps / 1e6
	}
	return DefaultMinUtilityPerMbps / 1e6
}

// DefaultMinUtilityPerMbps was chosen by working the arithmetic on the two
// cases that must land on opposite sides of it: promoting the dominant
// speaker from 360p to 720p on a 720p viewport scores about 1.8, and
// promoting a gallery thumbnail from 15 to 30 fps scores about 0.35. Any
// threshold in between separates them; 1.0 sits in the middle with room on
// each side for the scoring function to be retuned without the policy
// silently inverting.
const DefaultMinUtilityPerMbps = 1.0

func (b Budget) allocatable() int {
	h := b.HeadroomRatio
	if h <= 0 || h >= 1 {
		h = 0.15
	}
	return int(float64(b.EstimateBps) * (1 - h))
}

// utility scores a layer for a subscription on a scale where 1.0 is "fully
// satisfies what this receiver can display".
//
// The shape of this function is the entire policy of the system, so it is
// worth being explicit about each term.
func utility(s Subscription, l Layer) float64 {
	if !s.Visible {
		return 0
	}

	// Spatial term. Ratio of delivered pixels to painted pixels, clamped at
	// 1. Exceeding the viewport earns nothing -- this is the saturation that
	// stops us shipping detail into a downscaler.
	want := s.ViewportW * s.ViewportH
	if want <= 0 {
		want = 1
	}
	spatial := float64(l.Pixels()) / float64(want)
	if spatial > 1 {
		spatial = 1
	}
	// Below the viewport, quality falls off faster than linearly in pixels,
	// because upscaling artefacts are more objectionable than the pixel
	// count alone suggests. Square root is the standard perceptual fit.
	spatial = math.Sqrt(spatial)

	// Temporal term. Perceived smoothness saturates well before 60 fps and
	// has a hard floor below about 8 fps, where motion reads as a slideshow.
	temporal := math.Min(1, math.Log1p(l.FPS)/math.Log1p(30))

	// Screen share inverts the usual weighting: legibility beats smoothness.
	sw, tw := 0.65, 0.35
	if s.IsScreenShare {
		sw, tw = 0.9, 0.1
	}
	base := sw*spatial + tw*temporal

	// Attention multiplier. The person speaking is what the meeting is
	// about; a pinned feed is what this particular user said the meeting is
	// about, and that wins.
	att := 1.0
	switch {
	case s.Pinned:
		att = 3.0
	case s.SpeakerRank == 0:
		att = 2.2
	case s.SpeakerRank == 1:
		att = 1.4
	case s.SpeakerRank > 1:
		att = 1.1
	}
	return base * att
}

// addsVisiblePixels reports whether next paints more of the viewport than
// cur: its pixels exceed cur's while cur is still below what is painted.
func addsVisiblePixels(s Subscription, cur, next Layer) bool {
	want := s.ViewportW * s.ViewportH
	return next.Pixels() > cur.Pixels() && cur.Pixels() < want
}

type candidate struct {
	sub   int
	layer int

	bitrate int
	// marginal utility per additional bit over the currently held rung
	density float64
}

// Allocate assigns a layer to every subscription within the budget.
//
// The method is a marginal-utility greedy: start everyone at nothing, then
// repeatedly buy the single upgrade that yields the most additional utility
// per additional bit, until the budget is exhausted.
//
// This is the fractional-knapsack heuristic. For the continuous relaxation
// it is provably optimal; for the discrete problem we actually have it is
// not, but the gap is bounded by the largest single item, and every
// alternative worth considering is worse in the ways that matter here.
// Dynamic programming over a bitrate axis would be exact but needs the axis
// quantised, runs per receiver per allocation cycle, and buys an
// improvement no human eye can resolve. Optimality is not the objective;
// spending each bit where it is most visible is.
func Allocate(subs []Subscription, b Budget) []Allocation {
	out := make([]Allocation, len(subs))
	held := make([]int, len(subs)) // index into sorted layers, -1 = nothing
	sorted := make([][]Layer, len(subs))

	for i, s := range subs {
		out[i] = Allocation{TrackID: s.TrackID, Reason: "not visible"}
		held[i] = -1
		ls := append([]Layer(nil), s.Layers...)
		sort.Slice(ls, func(a, c int) bool { return ls[a].Bitrate < ls[c].Bitrate })
		sorted[i] = ls
	}

	spent := 0
	budget := b.allocatable()

	// Floor pass. Every visible subscription gets its cheapest rung before
	// any subscription gets a second one. A gallery where the speaker is
	// crisp and eleven tiles are black is a worse product than one where
	// everybody is visible, regardless of what the utility sum says, and
	// this is the one place a hard rule beats the scoring function.
	for i, s := range subs {
		if !s.Visible || len(sorted[i]) == 0 {
			continue
		}
		l := sorted[i][0]
		if spent+l.Bitrate > budget {
			out[i].Reason = "starved: budget exhausted at floor pass"
			continue
		}
		held[i] = 0
		spent += l.Bitrate
		out[i] = Allocation{
			TrackID: s.TrackID, Layer: l, Selected: true,
			Bitrate: l.Bitrate, Reason: "floor",
		}
	}

	// Upgrade pass.
	for {
		best := candidate{sub: -1}
		for i, s := range subs {
			if held[i] < 0 || held[i]+1 >= len(sorted[i]) {
				continue
			}
			cur := sorted[i][held[i]]
			next := sorted[i][held[i]+1]
			dBits := next.Bitrate - cur.Bitrate
			if dBits <= 0 {
				continue
			}
			if spent+dBits > budget {
				continue
			}
			dUtil := utility(s, next) - utility(s, cur)
			if dUtil <= 0 {
				// The upgrade buys nothing this receiver can perceive.
				// This is the saturation rule doing its work, and it is the
				// single largest source of savings in the whole system.
				continue
			}
			d := dUtil / float64(dBits)
			// The floor judges upgrades that buy only frame rate. One that
			// puts more pixels on a tile that paints them is never refused
			// for being expensive: that is quality traded for bits (rule 3),
			// and it left a two-person call on 360p stretched over an
			// 820x556 tile with bandwidth to spare. The budget still bounds it.
			if d < b.minDensity() && !addsVisiblePixels(s, cur, next) {
				// Perceptible in principle, not worth the bits in practice.
				// Left unspent, this capacity becomes burst headroom.
				continue
			}
			if d > best.density {
				best = candidate{sub: i, layer: held[i] + 1, bitrate: dBits, density: d}
			}
		}
		if best.sub < 0 {
			break
		}
		i := best.sub
		held[i] = best.layer
		spent += best.bitrate
		l := sorted[i][best.layer]
		out[i] = Allocation{
			TrackID: subs[i].TrackID, Layer: l, Selected: true,
			Bitrate: l.Bitrate, Reason: "upgraded on marginal utility",
		}
	}

	for i := range out {
		if !out[i].Selected && subs[i].Visible && out[i].Reason == "not visible" {
			out[i].Reason = "no layers offered"
		}
	}
	return out
}

// TotalBitrate sums an allocation set.
func TotalBitrate(a []Allocation) int {
	n := 0
	for _, x := range a {
		if x.Selected {
			n += x.Bitrate
		}
	}
	return n
}
