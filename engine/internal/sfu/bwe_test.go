package sfu

import (
	"math"
	"testing"
	"time"
)

const feedbackInterval = 100 * time.Millisecond

// gccReference models the recovery behaviour of a standard Google
// Congestion Control rate controller: while increasing, the estimate climbs
// multiplicatively at about 8% per second.
//
// This is a fair rather than a flattering model of the incumbent. The
// additive-increase regime GCC uses near convergence is considerably slower
// still, so the real gap in the field is wider than this test reports.
func gccReference(from, to float64) time.Duration {
	if from >= to {
		return 0
	}
	secs := math.Log(to/from) / math.Log(1.08)
	return time.Duration(secs * float64(time.Second))
}

// The headline claim of the design: a transient costs rate, but not seconds.
func TestRecoversFromTransientInUnderOneSecond(t *testing.T) {
	base := time.Unix(0, 0)
	const proven = 3_000_000
	e := NewEstimatorAt(DefaultConfig(), proven, base)

	now := base
	// Establish the path as healthy so the ceiling is genuinely earned.
	for i := 0; i < 20; i++ {
		now = now.Add(feedbackInterval)
		e.Update(now, SignalNormal, proven)
	}
	ceilingBefore := e.Ceiling()

	// One transient. Throughput briefly collapses to 1.2 Mbps.
	now = now.Add(feedbackInterval)
	afterDip := e.Update(now, SignalOveruse, 1_200_000)
	if afterDip >= proven {
		t.Fatalf("estimator ignored a real overuse event: %d", afterDip)
	}
	if e.Ceiling() < ceilingBefore {
		t.Fatalf("a single transient erased the capacity memory: ceiling %d -> %d",
			ceilingBefore, e.Ceiling())
	}

	// The path is fine again. Count how long to get back to 90% of proven.
	target := int(0.9 * proven)
	start := now
	var recovered time.Duration
	for i := 0; i < 200; i++ {
		now = now.Add(feedbackInterval)
		if e.Update(now, SignalNormal, proven) >= target {
			recovered = now.Sub(start)
			break
		}
	}
	if recovered == 0 {
		t.Fatalf("never recovered; stuck at %d", e.Rate())
	}

	ref := gccReference(float64(afterDip), float64(target))
	t.Logf("dip to %d bps; aether recovered to %d bps in %v, GCC-style would take %v (%.0fx)",
		afterDip, target, recovered, ref, float64(ref)/float64(recovered))

	if recovered > time.Second {
		t.Fatalf("recovery took %v, want under 1s", recovered)
	}
	if ref < recovered {
		t.Fatalf("reference recovered faster (%v) than ours (%v)", ref, recovered)
	}
}

// Memory must not become stubbornness: a real capacity reduction has to be
// learned, and quickly.
func TestLearnsGenuineCapacityReduction(t *testing.T) {
	base := time.Unix(0, 0)
	e := NewEstimatorAt(DefaultConfig(), 3_000_000, base)
	now := base
	for i := 0; i < 20; i++ {
		now = now.Add(feedbackInterval)
		e.Update(now, SignalNormal, 3_000_000)
	}

	// The path really is 1 Mbps now. Every attempt to exceed it overuses.
	const realCapacity = 1_000_000
	for i := 0; i < 60; i++ {
		now = now.Add(feedbackInterval)
		sig := SignalNormal
		if e.Rate() > realCapacity {
			sig = SignalOveruse
		}
		e.Update(now, sig, realCapacity)
	}

	if e.Ceiling() > 1_600_000 {
		t.Fatalf("ceiling stayed at %d against a real 1 Mbps path; memory became stubbornness",
			e.Ceiling())
	}
	if e.Rate() > 1_400_000 {
		t.Fatalf("rate settled at %d on a 1 Mbps path", e.Rate())
	}
	t.Logf("settled: rate=%d ceiling=%d against a real %d path",
		e.Rate(), e.Ceiling(), realCapacity)
}

// One late packet group is not congestion. Reacting to it costs the user
// visible quality for no reason, which is the most common failure mode of
// naive delay-based controllers.
func TestSingleNoisySampleDoesNotSignalOveruse(t *testing.T) {
	e := NewEstimatorAt(DefaultConfig(), 2_000_000, time.Unix(0, 0))
	for i := 0; i < 10; i++ {
		if sig := e.Observe(0.1, 2.0); sig != SignalNormal {
			t.Fatalf("quiet link classified as %v", sig)
		}
	}
	// A single large spike.
	if sig := e.Observe(40, 2.0); sig == SignalOveruse {
		t.Fatal("one spike triggered overuse; corroboration is required")
	}
	// Sustained, and it must be caught.
	var caught bool
	for i := 0; i < 10; i++ {
		if e.Observe(40, 2.0) == SignalOveruse {
			caught = true
			break
		}
	}
	if !caught {
		t.Fatal("sustained delay growth was never classified as overuse")
	}
}

// The burst bucket is what keeps motion sharp. A sender that has been idle
// must be able to spend well above its steady rate for a few hundred
// milliseconds, and must be throttled once the bucket is drained.
func TestBurstBucketFundsASpikeThenThrottles(t *testing.T) {
	base := time.Unix(0, 0)
	cfg := DefaultConfig()
	e := NewEstimatorAt(cfg, 2_000_000, base)

	// A second of quiet fills the bucket. The grant is capped by measured
	// capacity, not by the size of the ask: bursting far beyond what the
	// path carries would only build a queue and cost more latency than the
	// sharpness is worth.
	now := base.Add(time.Second)
	granted := e.TakeBurst(now, 8_000_000)
	ceiling := int(2_000_000 * MaxBurstRatio)
	if granted != ceiling {
		t.Fatalf("rested burst granted %d, want the physical cap %d", granted, ceiling)
	}
	t.Logf("rested burst: asked 8 Mbps on a 2 Mbps path -> granted %d bps "+
		"(the reserved headroom, spendable instantly)", granted)

	// Sustained demand must drain the bucket and fall back toward steady.
	// At a 25% overshoot the bucket funds roughly 2.4 seconds of continuous
	// bursting before it is spent, which is comfortably longer than any real
	// motion event and short enough that it cannot sustain a standing queue.
	var last int
	for i := 0; i < 30; i++ {
		last = e.TakeBurst(now, 8_000_000)
	}
	if last >= granted {
		t.Fatalf("bucket never drained under sustained demand: still granting %d", last)
	}
	t.Logf("after sustained demand -> granted %d bps (bucket drained to steady)", last)

	// Steady-state requests are always honoured.
	if got := e.TakeBurst(now, 1_500_000); got != 1_500_000 {
		t.Fatalf("a below-rate request was throttled to %d", got)
	}
}

// Over a realistic minute the burst mechanism must not raise the average.
// This is the claim that peak-heavy VBR is free in aggregate.
func TestBurstsDoNotRaiseTheAverage(t *testing.T) {
	base := time.Unix(0, 0)
	e := NewEstimatorAt(DefaultConfig(), 2_000_000, base)

	var totalBits float64
	now := base
	const steps = 600 // 60 seconds at 100 ms
	for i := 0; i < steps; i++ {
		now = now.Add(feedbackInterval)
		e.Update(now, SignalNormal, 2_000_000)

		// A static talking head wants well under the steady rate, with a
		// motion burst roughly every four seconds.
		want := 900_000
		if i%40 == 0 {
			want = 9_000_000
		}
		got := e.TakeBurst(now, want)
		totalBits += float64(got) * feedbackInterval.Seconds()
	}
	avg := totalBits / 60
	t.Logf("60 s with a 9 Mbps burst every 4 s: average %.0f bps", avg)
	if avg > 2_000_000 {
		t.Fatalf("average %.0f bps exceeded the steady estimate; bursts were not free", avg)
	}
}

// A reconnect must not hand out a fresh burst. This is the measured failure
// from a sibling project: seven reconnections in thirty-two seconds, each
// spending a full bucket, on a path that was reconnecting because it was
// already unhealthy.
func TestBurstBudgetSurvivesReconnect(t *testing.T) {
	ResetBurstPool()
	base := time.Unix(0, 0)
	const peer = "participant-7f3a"

	// First connection: rest, then spend the whole bucket.
	e1 := NewEstimatorAt(DefaultConfig(), 2_000_000, base)
	now := base.Add(time.Second)
	e1.AttachBurstPool(peer, now)
	for i := 0; i < 40; i++ {
		e1.TakeBurst(now, 8_000_000)
	}
	drained := e1.TakeBurst(now, 8_000_000)
	if drained > 2_100_000 {
		t.Fatalf("bucket did not drain on the first connection: %d", drained)
	}

	// The connection dies and is immediately re-established.
	e2 := NewEstimatorAt(DefaultConfig(), 2_000_000, now)
	e2.AttachBurstPool(peer, now)
	afterReconnect := e2.TakeBurst(now, 8_000_000)

	if afterReconnect > 2_100_000 {
		t.Fatalf("reconnect granted a fresh burst of %d bps; the ceiling is "+
			"not a ceiling if a failure resets it", afterReconnect)
	}
	t.Logf("drained %d bps, and after an immediate reconnect still %d bps",
		drained, afterReconnect)

	// A participant who has been away long enough is not penalised.
	later := now.Add(30 * time.Second)
	e3 := NewEstimatorAt(DefaultConfig(), 2_000_000, later)
	e3.AttachBurstPool(peer, later)
	if got := e3.TakeBurst(later, 8_000_000); got < 2_400_000 {
		t.Fatalf("bucket did not refill over 30s away: %d", got)
	}

	// And the pool does not grow without bound.
	if n := BurstPoolSize(); n != 1 {
		t.Fatalf("pool holds %d buckets for one participant", n)
	}
	e4 := NewEstimatorAt(DefaultConfig(), 2_000_000, later)
	e4.AttachBurstPool("someone-else", later.Add(BurstMemory+time.Second))
	if n := BurstPoolSize(); n != 1 {
		t.Fatalf("stale buckets were not swept: %d retained", n)
	}
}
