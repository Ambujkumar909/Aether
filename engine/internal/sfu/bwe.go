package sfu

import (
	"math"
	"time"
)

// Congestion control, and specifically the recovery half of it, is where
// real-time video is won or lost.
//
// The industry standard is Google Congestion Control: estimate the trend in
// one-way delay, classify the link as overusing / normal / underusing, and
// drive a three-state rate controller. It is a good design and its detection
// half is excellent. Its recovery half is not. On the additive-increase path
// GCC climbs by roughly 5% of the current estimate, or 4 kbps, per feedback
// interval, whichever is larger. After a transient -- a microwave oven, a
// neighbour starting a download, a wifi roam, a single dropped burst -- the
// estimate collapses in one multiplicative step and then takes many seconds
// to climb back.
//
// Those seconds are not neutral. They are the seconds during which the user
// is looking at a degraded picture on a link that has been perfectly healthy
// the entire time. Every video product exhibits this, and users experience
// it as "it never really recovered after that one glitch".
//
// The fix implemented here is capacity memory. The controller remembers the
// highest rate the path has recently sustained without overuse. Below that
// remembered ceiling it treats the capacity as previously proven and ramps
// back multiplicatively, in a handful of feedback intervals rather than a
// hundred. Only above the ceiling does it revert to cautious additive
// probing, because only there is it making a genuinely new claim about the
// path.
//
// The ceiling decays, so the memory does not outlive its evidence, and a
// real capacity reduction is learned rather than fought.

// State is the rate controller's classification of the path.
type State uint8

const (
	StateIncrease State = iota
	StateHold
	StateDecrease
)

func (s State) String() string {
	switch s {
	case StateHold:
		return "hold"
	case StateDecrease:
		return "decrease"
	default:
		return "increase"
	}
}

// Signal is the delay-trend classification derived from feedback.
type Signal uint8

const (
	SignalNormal Signal = iota
	SignalOveruse
	SignalUnderuse
)

// Config tunes the controller. The defaults are the values the tests in this
// package pin, and changing them should mean changing those tests too.
type Config struct {
	// MinBps is the floor below which we stop reducing and start dropping
	// layers instead. Continuing to shrink the encoder below this produces
	// a picture nobody wants at a bitrate that still does not fit.
	MinBps int
	// MaxBps is an upper bound for sanity only. It is deliberately very
	// large: this system does not impose a bitrate ceiling on a link that
	// can carry the bits, because a motion burst that gets clamped is a
	// smeared frame the user can see.
	MaxBps int

	// DecreaseFactor is applied on detected overuse. 0.85 is gentler than
	// GCC's traditional 0.85-of-measured-throughput because our detector
	// has fewer false positives, so we can afford to give up less ground.
	DecreaseFactor float64

	// RecoveryFactor is the multiplicative step used while climbing back
	// toward a remembered ceiling. This is the whole point of the design.
	RecoveryFactor float64

	// ProbeFactor is the cautious multiplicative step used above the
	// remembered ceiling, where we are making a new claim.
	ProbeFactor float64

	// CeilingHalfLife is how long a remembered capacity survives without
	// being reconfirmed.
	CeilingHalfLife time.Duration

	// OveruseWindow is how close together two overuse events must be for
	// the second to count as corroboration rather than an unrelated blip.
	OveruseWindow time.Duration
	// OveruseBeforeForget is how many corroborating events it takes to
	// write down the remembered capacity.
	OveruseBeforeForget int

	// BurstBucketMs is how much above the steady estimate a short burst may
	// draw, expressed as milliseconds of the current rate. A scene change
	// or a hand wave lasts a few hundred milliseconds; letting it draw a
	// token bucket rather than clamping it is the difference between sharp
	// motion and a block-smeared frame, and it costs nothing in the average
	// because the bucket refills from unused steady-state capacity.
	BurstBucketMs int
}

func DefaultConfig() Config {
	return Config{
		MinBps:              80_000,
		MaxBps:              100_000_000,
		DecreaseFactor:      0.85,
		RecoveryFactor:      1.35,
		ProbeFactor:         1.06,
		CeilingHalfLife:     20 * time.Second,
		OveruseWindow:       4 * time.Second,
		OveruseBeforeForget: 2,
		BurstBucketMs:       600,
	}
}

// Estimator is the bandwidth estimator and rate controller.
type Estimator struct {
	cfg Config

	rate  float64
	state State

	// ceiling is the remembered proven capacity, and ceilingAt is when it
	// was last confirmed.
	ceiling   float64
	ceilingAt time.Time

	lastUpdate time.Time

	// burst tokens, in bits.
	tokens    float64
	lastToken time.Time

	// Trend filtering state.
	trend      float64
	trendInit  bool
	overuseRun int

	lastOveruse   time.Time
	overuseEvents int

	// peerID binds this estimator to a burst bucket that survives its own
	// connection. Empty means the bucket is connection-scoped, which is
	// correct only for tests and for genuinely one-shot measurements.
	peerID string
}

func NewEstimator(cfg Config, start int) *Estimator {
	return NewEstimatorAt(cfg, start, time.Now())
}

// NewEstimatorAt builds an estimator anchored to an explicit clock, so that
// controller behaviour can be simulated deterministically rather than
// verified by sleeping, which is how control loops end up with tests that
// pass on a fast machine and fail in CI.
func NewEstimatorAt(cfg Config, start int, now time.Time) *Estimator {
	return &Estimator{
		cfg: cfg, rate: float64(start),
		ceiling: float64(start), ceilingAt: now,
		lastUpdate: now, lastToken: now,
	}
}

// Rate is the current steady-state estimate in bits per second.
func (e *Estimator) Rate() int { return int(e.rate) }

// State reports the controller state, for telemetry and for the UI to be
// honest with the user about what the network is doing.
func (e *Estimator) State() State { return e.state }

// Ceiling is the remembered proven capacity.
func (e *Estimator) Ceiling() int { return int(e.ceiling) }

// Observe feeds one delay-gradient measurement into the trend filter and
// returns the classification.
//
// delayGradientMs is the change in one-way delay across a packet group,
// which transport-wide congestion control feedback gives us directly. A
// positive sustained gradient means a queue is building somewhere, which is
// the earliest available evidence of congestion -- long before loss.
func (e *Estimator) Observe(delayGradientMs float64, thresholdMs float64) Signal {
	// Exponential trend filter. A single sample is noise; the trend is
	// signal. The coefficient is deliberately slow enough that one late
	// packet cannot trigger a decrease, because a false positive here costs
	// the user visible quality for seconds.
	const alpha = 0.15
	if !e.trendInit {
		e.trend = delayGradientMs
		e.trendInit = true
	} else {
		e.trend = (1-alpha)*e.trend + alpha*delayGradientMs
	}

	if thresholdMs <= 0 {
		thresholdMs = 2.0
	}

	switch {
	case e.trend > thresholdMs:
		e.overuseRun++
		// Require corroboration. One noisy group is not congestion, and
		// reacting to it is how estimators end up sawtoothing on a link
		// that was never actually congested.
		if e.overuseRun >= 2 {
			return SignalOveruse
		}
		return SignalNormal
	case e.trend < -thresholdMs:
		e.overuseRun = 0
		return SignalUnderuse
	default:
		e.overuseRun = 0
		return SignalNormal
	}
}

// Update advances the rate controller. measuredBps is the throughput
// actually achieved over the last interval, used to anchor a decrease to
// reality rather than to a guess.
func (e *Estimator) Update(now time.Time, sig Signal, measuredBps int) int {
	dt := now.Sub(e.lastUpdate)
	if dt <= 0 {
		dt = time.Millisecond
	}
	e.lastUpdate = now
	e.decayCeiling(now)

	switch sig {
	case SignalOveruse:
		e.state = StateDecrease
		// Anchor to what the path actually delivered, not to what we asked
		// for. If we were sending 3 Mbps into a 1 Mbps path, backing off to
		// 85% of 3 Mbps accomplishes nothing.
		target := e.rate
		if measuredBps > 0 && float64(measuredBps) < target {
			target = float64(measuredBps)
		}
		e.rate = target * e.cfg.DecreaseFactor

		// Whether to forget the remembered capacity is the single most
		// consequential decision in this file.
		//
		// Slamming the ceiling down on every overuse event is the obvious
		// implementation and it is wrong: it destroys the memory precisely
		// when the memory is most valuable, and it reduces this controller
		// to the additive crawl it exists to replace. One overuse event is
		// far more often a transient -- a wifi retransmit burst, a competing
		// flow starting and stopping, a scheduler hiccup -- than a genuine
		// change in path capacity.
		//
		// So a single event costs rate but not memory. Only when overuse
		// recurs, meaning we ramped back to the remembered ceiling and hit
		// the wall again, do we accept that the path really did change and
		// write the ceiling down. That self-corrects within two cycles,
		// which is fast enough that a genuine capacity drop is learned
		// quickly, while a lone glitch costs nothing lasting.
		if now.Sub(e.lastOveruse) < e.cfg.OveruseWindow {
			e.overuseEvents++
		} else {
			e.overuseEvents = 1
		}
		e.lastOveruse = now

		if e.overuseEvents >= e.cfg.OveruseBeforeForget && e.ceiling > e.rate {
			e.ceiling = e.rate * 1.05
			e.ceilingAt = now
		}

	case SignalUnderuse:
		// The queue is draining: the path has more room than we are using.
		// Hold rather than climb, because underuse frequently means our own
		// encoder simply did not produce as much as it was allowed to, and
		// raising the estimate on that basis is how estimators drift up
		// into a link they never tested.
		e.state = StateHold

	default:
		e.state = StateIncrease
		if e.rate < e.ceiling {
			// Below proven capacity. This link has already carried this
			// much without complaint, and the only reason we are not there
			// is a transient that has since cleared. Ramp, do not crawl.
			//
			// The step is scaled by elapsed time so the behaviour does not
			// depend on feedback frequency.
			step := math.Pow(e.cfg.RecoveryFactor, dt.Seconds()*10)
			e.rate *= step
			if e.rate > e.ceiling {
				e.rate = e.ceiling
			}
		} else {
			// Above proven capacity: a new claim, made cautiously.
			step := math.Pow(e.cfg.ProbeFactor, dt.Seconds()*10)
			e.rate *= step
			e.ceiling = e.rate
			e.ceilingAt = now
		}
	}

	if e.rate < float64(e.cfg.MinBps) {
		e.rate = float64(e.cfg.MinBps)
	}
	if e.rate > float64(e.cfg.MaxBps) {
		e.rate = float64(e.cfg.MaxBps)
	}
	if e.ceiling < e.rate {
		e.ceiling = e.rate
		e.ceilingAt = now
	}
	return int(e.rate)
}

// decayCeiling ages the remembered capacity so that evidence expires.
func (e *Estimator) decayCeiling(now time.Time) {
	if e.cfg.CeilingHalfLife <= 0 {
		return
	}
	age := now.Sub(e.ceilingAt)
	if age <= 0 {
		return
	}
	decay := math.Pow(0.5, age.Seconds()/e.cfg.CeilingHalfLife.Seconds())
	floor := e.rate
	e.ceiling = floor + (e.ceiling-floor)*decay
	e.ceilingAt = now
}

// ---------------------------------------------------------------------------
// Burst allowance
// ---------------------------------------------------------------------------

// TakeBurst asks permission to exceed the steady rate for a short spike,
// returning the bitrate the sender may actually use right now.
//
// This is the mechanism that keeps motion sharp. A near-CBR sender facing a
// scene change has exactly one option: raise the quantiser and smear the
// frame. A sender with a burst bucket spends accumulated headroom, keeps the
// frame sharp, and repays it out of the static seconds that follow. The
// fifteen-minute average is unchanged or lower; the picture during the one
// moment anybody was looking is dramatically better.
func (e *Estimator) TakeBurst(now time.Time, wantBps int) int {
	// Refill from unused steady capacity.
	dt := now.Sub(e.lastToken).Seconds()
	if dt < 0 {
		dt = 0
	}
	e.lastToken = now
	capacity := e.rate * float64(e.cfg.BurstBucketMs) / 1000
	e.tokens += e.rate * dt
	if e.tokens > capacity {
		e.tokens = capacity
	}

	if float64(wantBps) <= e.rate {
		return wantBps
	}

	// A burst is bounded by measured capacity, and this bound is not
	// timidity -- it is the difference between a burst that helps and one
	// that is actively self-defeating.
	//
	// The intuition "no ceiling, let motion take whatever it wants" is right
	// about intent and wrong about mechanism. Pushing 8 Mbps into a path
	// that carries 2 Mbps does not deliver 8 Mbps of sharp motion. It
	// delivers 2 Mbps and parks the other 6 in a queue somewhere upstream,
	// and that queue is 1.5 seconds deep by the time the burst ends. The
	// user gets the smeared frame anyway, now with a second and a half of
	// added latency and a delay spike that will trip our own overuse
	// detector and cost quality for seconds afterwards.
	//
	// So the cap is physical, not policy. There is no artificial bitrate
	// limit anywhere in this system: on a link that genuinely carries
	// 50 Mbps, e.rate climbs to 50 Mbps and a burst may take it. What the
	// bucket buys is the right to spend the reserved headroom instantly,
	// the moment a scene changes, instead of waiting for the next allocation
	// cycle to notice. That is the entire quality win, and it is available
	// without ever queueing a packet we cannot deliver.
	maxBurst := e.rate * MaxBurstRatio
	want := float64(wantBps)
	if want > maxBurst {
		want = maxBurst
	}

	// The excess over the steady rate is paid for out of the bucket, priced
	// over a nominal 100 ms spending window.
	const windowSec = 0.1
	excess := (want - e.rate) * windowSec
	if excess <= e.tokens {
		e.tokens -= excess
		e.syncBurstPool(now)
		return int(want)
	}
	affordable := e.rate + e.tokens/windowSec
	e.tokens = 0
	e.syncBurstPool(now)
	return int(affordable)
}

// MaxBurstRatio is how far above the current estimate a burst may reach.
//
// Set by what a path can absorb without building a queue that hurts more
// than the burst helps. Router buffers along a typical consumer path can
// absorb a modest overshoot for a few hundred milliseconds; 25% over the
// estimate is comfortably inside that for the burst durations we care about,
// and it is enough to carry a scene change at full sharpness because the
// steady allocation already sits 15% below the estimate. The burst is
// therefore drawing mostly on capacity we deliberately reserved rather than
// on capacity we are inventing.
const MaxBurstRatio = 1.25

// BurstTokens exposes the bucket for telemetry.
func (e *Estimator) BurstTokens() float64 { return e.tokens }
