package sfu

import "testing"

// ladder is a conventional three-rung encoding of a camera track.
func ladder() []Layer {
	return []Layer{
		{Spatial: 0, Width: 320, Height: 180, FPS: 15, Bitrate: 85_000},
		{Spatial: 1, Width: 640, Height: 360, FPS: 30, Bitrate: 350_000},
		{Spatial: 2, Width: 1280, Height: 720, FPS: 30, Bitrate: 750_000},
	}
}

func sub(id string, w, h int, visible bool, rank int) Subscription {
	return Subscription{
		TrackID: id, Layers: ladder(),
		ViewportW: w, ViewportH: h, Visible: visible, SpeakerRank: rank,
	}
}

func find(t *testing.T, as []Allocation, id string) Allocation {
	t.Helper()
	for _, a := range as {
		if a.TrackID == id {
			return a
		}
	}
	t.Fatalf("no allocation for %s", id)
	return Allocation{}
}

// The core claim of the whole system: bits are never spent on detail the
// receiver cannot paint, even when bandwidth is effectively unlimited.
func TestNeverExceedsViewportEvenWithInfiniteBandwidth(t *testing.T) {
	subs := []Subscription{sub("thumb", 160, 90, true, 5)}
	got := find(t, Allocate(subs, Budget{EstimateBps: 100_000_000}), "thumb")

	if got.Layer.Height > 180 {
		t.Fatalf("allocated %dp to a 90px tall tile with unlimited budget; "+
			"this is the exact waste the allocator exists to prevent", got.Layer.Height)
	}
	t.Logf("160x90 tile, 100 Mbps available -> %dx%d at %d bps",
		got.Layer.Width, got.Layer.Height, got.Bitrate)
}

// An invisible tile costs zero, not "a low layer".
func TestInvisibleSubscriptionGetsNothing(t *testing.T) {
	subs := []Subscription{
		sub("visible", 1280, 720, true, 0),
		sub("scrolled-away", 640, 360, false, 1),
	}
	as := Allocate(subs, Budget{EstimateBps: 10_000_000})
	if a := find(t, as, "scrolled-away"); a.Selected {
		t.Fatalf("forwarded %d bps to an invisible tile", a.Bitrate)
	}
	if a := find(t, as, "visible"); !a.Selected {
		t.Fatal("visible tile got nothing")
	}
}

// The budget is a hard constraint, and the headroom reservation is real.
func TestNeverExceedsBudget(t *testing.T) {
	var subs []Subscription
	for i := 0; i < 12; i++ {
		subs = append(subs, sub(string(rune('a'+i)), 640, 360, true, i))
	}
	for _, est := range []int{300_000, 1_000_000, 2_500_000, 6_000_000} {
		b := Budget{EstimateBps: est, HeadroomRatio: 0.15}
		total := TotalBitrate(Allocate(subs, b))
		if total > b.allocatable() {
			t.Fatalf("estimate %d: allocated %d, over the %d ceiling", est, total, b.allocatable())
		}
		t.Logf("estimate %8d -> allocated %8d (ceiling %d)", est, total, b.allocatable())
	}
}

// Under pressure the speaker keeps quality and the periphery degrades.
// The alternative -- degrading everyone equally -- is what products do now,
// and it makes the one feed anybody is looking at worse for no reason.
func TestSpeakerIsProtectedUnderPressure(t *testing.T) {
	subs := []Subscription{
		sub("speaker", 1280, 720, true, 0),
		sub("other-1", 1280, 720, true, 3),
		sub("other-2", 1280, 720, true, 4),
		sub("other-3", 1280, 720, true, 5),
	}
	as := Allocate(subs, Budget{EstimateBps: 1_400_000, HeadroomRatio: 0.15})
	speaker := find(t, as, "speaker")
	for _, id := range []string{"other-1", "other-2", "other-3"} {
		o := find(t, as, id)
		if o.Layer.Pixels() > speaker.Layer.Pixels() {
			t.Fatalf("%s (%dp) outranked the speaker (%dp)",
				id, o.Layer.Height, speaker.Layer.Height)
		}
	}
	t.Logf("at 1.4 Mbps: speaker=%dp others=%dp/%dp/%dp",
		speaker.Layer.Height,
		find(t, as, "other-1").Layer.Height,
		find(t, as, "other-2").Layer.Height,
		find(t, as, "other-3").Layer.Height)
}

// An explicit user action must beat every heuristic in the file.
func TestPinnedBeatsDominantSpeaker(t *testing.T) {
	subs := []Subscription{
		sub("speaker", 1280, 720, true, 0),
		func() Subscription { s := sub("pinned", 1280, 720, true, 7); s.Pinned = true; return s }(),
	}
	as := Allocate(subs, Budget{EstimateBps: 1_000_000, HeadroomRatio: 0.15})
	p := find(t, as, "pinned")
	s := find(t, as, "speaker")
	if p.Layer.Pixels() < s.Layer.Pixels() {
		t.Fatalf("pinned feed (%dp) lost to the speaker (%dp); explicit intent must win",
			p.Layer.Height, s.Layer.Height)
	}
}

// Everyone visible gets something before anyone gets a second rung.
func TestFloorPassKeepsEveryoneVisible(t *testing.T) {
	var subs []Subscription
	for i := 0; i < 8; i++ {
		subs = append(subs, sub(string(rune('a'+i)), 320, 180, true, i))
	}
	as := Allocate(subs, Budget{EstimateBps: 900_000, HeadroomRatio: 0.15})
	for _, a := range as {
		if !a.Selected {
			t.Fatalf("%s went black at 900 kbps for 8 thumbnails: %s", a.TrackID, a.Reason)
		}
	}
}

// Screen share is scored on legibility, not smoothness.
func TestScreenShareFavoursResolution(t *testing.T) {
	share := Subscription{
		TrackID: "screen", Visible: true, IsScreenShare: true,
		ViewportW: 1920, ViewportH: 1080,
		Layers: []Layer{
			{Width: 960, Height: 540, FPS: 30, Bitrate: 600_000},
			{Width: 1920, Height: 1080, FPS: 5, Bitrate: 620_000},
		},
	}
	as := Allocate([]Subscription{share}, Budget{EstimateBps: 900_000, HeadroomRatio: 0.15})
	got := find(t, as, "screen")
	if got.Layer.Height != 1080 {
		t.Fatalf("chose %dp@%.0ffps for a screen share; text legibility should win",
			got.Layer.Height, got.Layer.FPS)
	}
}

// A tile whose viewport exactly matches a rung should not buy the next one,
// because the extra pixels are unpaintable.
func TestUpgradeStopsAtSaturation(t *testing.T) {
	s := sub("exact", 640, 360, true, 0)
	as := Allocate([]Subscription{s}, Budget{EstimateBps: 50_000_000})
	got := find(t, as, "exact")
	if got.Layer.Height > 360 {
		t.Fatalf("bought %dp for a 360p viewport with 50 Mbps free", got.Layer.Height)
	}
}

// Aggregate demonstration: what a 12-person gallery actually costs when the
// allocator refuses to send unpaintable pixels.
func TestGalleryCostIsBoundedByWhatIsPainted(t *testing.T) {
	var subs []Subscription
	// One speaker large, three mid tiles, eight thumbnails below the fold.
	subs = append(subs, sub("speaker", 960, 540, true, 0))
	for i := 0; i < 3; i++ {
		subs = append(subs, sub("mid"+string(rune('a'+i)), 320, 180, true, i+1))
	}
	for i := 0; i < 8; i++ {
		subs = append(subs, sub("fold"+string(rune('a'+i)), 320, 180, false, -1))
	}

	as := Allocate(subs, Budget{EstimateBps: 20_000_000, HeadroomRatio: 0.15})
	total := TotalBitrate(as)

	// The naive baseline: every one of the twelve at the mid rung, which is
	// what a product without viewport awareness forwards.
	naive := 12 * 350_000
	t.Logf("12-person gallery: aether=%d bps  viewport-blind baseline=%d bps  (%.1fx)",
		total, naive, float64(naive)/float64(total))
	if total >= naive {
		t.Fatalf("allocator spent %d, no better than the blind baseline %d", total, naive)
	}
}

// A two-person call: one big tile of the other person, nobody ranked as
// speaker yet, bandwidth to spare. Measured in Chrome before this test: the
// sender produced 1280x720 at ~890 kbit/s, but the utility floor refused the
// 360p->720p upgrade (density ~0.34 per Mbps against a floor of 1.0), so the
// receiver drew 640x360 stretched over an 820x556 tile for the whole call.
// The floor exists to stop spending on imperceptible upgrades; a doubling of
// resolution on a tile that paints it is not one.
func TestVisibleResolutionIsNeverRefusedForWantOfUtility(t *testing.T) {
	measured := []Layer{
		{Spatial: 0, Width: 320, Height: 180, FPS: 15, Bitrate: 130_000},
		{Spatial: 1, Width: 640, Height: 360, FPS: 30, Bitrate: 330_000},
		{Spatial: 2, Width: 1280, Height: 720, FPS: 30, Bitrate: 890_000},
	}
	for _, rank := range []int{-1, 0, 3} {
		s := Subscription{TrackID: "other", Layers: measured, ViewportW: 820, ViewportH: 556, Visible: true, SpeakerRank: rank}
		got := find(t, Allocate([]Subscription{s}, Budget{EstimateBps: 20_000_000}), "other")
		if got.Layer.Height != 720 {
			t.Fatalf("rank %d: an 820x556 tile got %dp with 20 Mbps free (%s)", rank, got.Layer.Height, got.Reason)
		}
	}
	// The floor still does its job where the gain is only frame rate: a
	// thumbnail already at full resolution for its size stays on the
	// cheap rung.
	th := Subscription{TrackID: "thumb", Layers: measured, ViewportW: 320, ViewportH: 180, Visible: true, SpeakerRank: 4}
	if got := find(t, Allocate([]Subscription{th}, Budget{EstimateBps: 20_000_000}), "thumb"); got.Layer.Height != 180 {
		t.Fatalf("a 320x180 thumbnail was upgraded to %dp for frame rate alone", got.Layer.Height)
	}
}
