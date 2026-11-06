package sfu

import (
	"testing"
	"time"
)

// Receiver reports move the estimate: a clean link climbs past the starting
// 1.5 Mbit/s (it used to stay there for good), real loss backs it off,
// moderate loss holds it, the worst report in a window counts, and the climb
// follows elapsed time rather than how often reports come.
func TestReceiverReportsFeedTheEstimate(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	fresh := func() *Peer {
		p := &Peer{Estimator: NewEstimatorAt(DefaultConfig(), 1_500_000, at(0))}
		p.observeLoss(0, at(0)) // the first report only starts the clock
		return p
	}

	p := fresh()
	if p.EstimateRate() != 1_500_000 {
		t.Fatalf("moved on the first report: %d", p.EstimateRate())
	}
	// One clean second, reported ten times.
	for ms := 100; ms <= 1000; ms += 100 {
		p.observeLoss(0, at(ms))
	}
	clean := p.EstimateRate()
	if clean <= 2_000_000 || clean >= DefaultConfig().MaxBps {
		t.Fatalf("one clean second: %d bit/s; it must climb, and not to the cap", clean)
	}
	// The same second reported twice climbs the same.
	q := fresh()
	q.observeLoss(0, at(500))
	q.observeLoss(0, at(1000))
	if r := q.EstimateRate(); r < clean*8/10 || r > clean*12/10 {
		t.Fatalf("two reports a second gave %d; ten gave %d", r, clean)
	}
	// Moderate loss holds.
	p.observeLoss(0.05, at(1600))
	if p.EstimateRate() != clean {
		t.Fatalf("5%% loss moved the rate: %d -> %d", clean, p.EstimateRate())
	}
	// Moderate loss followed by a clean report in the same window still
	// holds: the worst report in the window decides.
	p.observeLoss(0.05, at(1800))
	p.observeLoss(0, at(2100))
	if p.EstimateRate() != clean {
		t.Fatalf("a clean report hid the window's loss: %d -> %d", clean, p.EstimateRate())
	}
	// A long silence and then a clean report is one second's climb, not
	// the silence's: a gap is no evidence of capacity.
	g := fresh()
	g.observeLoss(0, at(60_000))
	if r := g.EstimateRate(); r > clean*12/10 {
		t.Fatalf("a minute without reports then a clean one: %d, more than a clean second's %d", r, clean)
	}
	// Real loss backs off.
	p.observeLoss(0.20, at(2700))
	if r := p.EstimateRate(); r >= clean {
		t.Fatalf("20%% loss did not back off: %d -> %d", clean, r)
	}
}
