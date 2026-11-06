package sfu

import "testing"

// A tile that goes off screen and comes back (the allocator selects none, then
// a layer again) must resume like a fresh start: on a keyframe, and with the
// outbound sequence carrying on by one. The publisher kept numbering through
// the pause; carrying its numbering across shows the receiver the whole pause
// as one gap, which it reports as loss. With 50 people that loss drove
// receivers' bandwidth estimates down until their tiles were starved
// (ARCHITECTURE 6bu).
func TestResumeAfterPauseWaitsForKeyframeAndLeavesNoGap(t *testing.T) {
	f := NewForwarder()
	f.Select(LayerFull)
	src := newSource(1000, 111)
	k := &sink{}
	for i := 0; i < 5; i++ {
		for _, p := range src.frame(3) {
			if f.WriteRTP(LayerFull, p, i == 0) == Forward {
				k.add(p)
			}
		}
	}

	f.Select(LayerNone)
	for i := 0; i < 300; i++ { // ten seconds off screen; the publisher carries on
		for _, p := range src.frame(3) {
			if f.WriteRTP(LayerFull, p, i%60 == 0) == Forward {
				t.Fatal("forwarded while paused")
			}
		}
	}

	if !f.Select(LayerFull) {
		t.Fatal("resuming did not ask the publisher for a keyframe")
	}
	if l, waiting := f.Waiting(); !waiting || l != LayerFull {
		t.Fatalf("resume is not reported as waiting on a keyframe (%v, %v)", l, waiting)
	}
	for _, p := range src.frame(3) {
		if f.WriteRTP(LayerFull, p, false) == Forward {
			t.Fatal("resumed on a delta frame: the decoder has no reference for it")
		}
	}
	for i := 0; i < 3; i++ {
		for _, p := range src.frame(3) {
			if f.WriteRTP(LayerFull, p, i == 0) == Forward {
				k.add(p)
			}
		}
	}
	if g := k.gaps(); len(g) != 0 {
		t.Fatalf("receiver saw the pause as a gap of %v packets", g)
	}
	if _, waiting := f.Waiting(); waiting {
		t.Fatal("still waiting after the keyframe arrived")
	}
}

// Audio has no keyframes: it resumes on the next packet, still without a gap.
func TestAudioResumeAfterPauseLeavesNoGap(t *testing.T) {
	f := NewAudioForwarder()
	f.Select(LayerFull)
	src := newSource(500, 222)
	k := &sink{}
	push := func(n int, want Decision) {
		for i := 0; i < n; i++ {
			for _, p := range src.frame(1) {
				d := f.WriteRTP(LayerFull, p, false)
				if d != want {
					t.Fatalf("decision %v, want %v", d, want)
				}
				if d == Forward {
					k.add(p)
				}
			}
		}
	}
	push(50, Forward)
	f.Select(LayerNone)
	push(500, Drop)
	f.Select(LayerFull)
	push(50, Forward)
	if g := k.gaps(); len(g) != 0 {
		t.Fatalf("receiver saw the pause as a gap of %v packets", g)
	}
}
