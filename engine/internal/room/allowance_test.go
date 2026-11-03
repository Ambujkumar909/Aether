package room

import (
	"math"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
)

// Each kind is its own bucket: the burst passes, the next is refused, it
// refills at its rate, one person's flood is not another's, and somebody who
// is not in the room is refused outright.
func TestAllowances(t *testing.T) {
	r := New("a", ledger.NewMeter("t"))
	for _, id := range []string{"p", "q"} {
		if _, err := r.Join(&Participant{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Unix(1_800_000_000, 0)
	for a := Allowance(0); a < numAllowances; a++ {
		burst := int(a.Burst())
		if burst > 1000 {
			// Media is counted in bytes: the burst in one go.
			if !r.AllowedN("p", a, a.Burst(), t0) {
				t.Fatalf("kind %d: a burst of %d refused", a, burst)
			}
		} else {
			for i := range burst {
				if !r.Allowed("p", a, t0) {
					t.Fatalf("kind %d: refused at %d of a burst of %d", a, i, burst)
				}
			}
		}
		if r.Allowed("p", a, t0) {
			t.Fatalf("kind %d: past the burst", a)
		}
		if !r.Allowed("q", a, t0) {
			t.Fatalf("kind %d: q held to p's allowance", a)
		}
		// One token's worth of time later, exactly one more.
		later := t0.Add(time.Duration(math.Ceil(float64(time.Second) / allowanceRates[a][0])))
		if !r.Allowed("p", a, later) || r.Allowed("p", a, later) {
			t.Fatalf("kind %d: the refill is not its rate", a)
		}
	}
	// A long silence refills to the burst, not past it.
	idle := t0.Add(time.Hour)
	for range int(AllowReaction.Burst()) {
		if !r.Allowed("q", AllowReaction, idle) {
			t.Fatal("refused inside the burst after an hour")
		}
	}
	if r.Allowed("q", AllowReaction, idle) {
		t.Fatal("an hour's silence saved up more than the burst")
	}
	// The kinds do not share: p, out of reactions, may still chat.
	if !r.Allowed("p", AllowChat, t0.Add(time.Hour)) {
		t.Fatal("chat refused after reactions ran out")
	}
	if r.Allowed("nobody", AllowReaction, t0) {
		t.Fatal("somebody not in the room")
	}
	// Leaving takes the buckets: back again, a full burst.
	_ = r.Leave("p")
	if _, err := r.Join(&Participant{ID: "p"}); err != nil {
		t.Fatal(err)
	}
	for range int(AllowReaction.Burst()) {
		if !r.Allowed("p", AllowReaction, t0) {
			t.Fatal("a returning person starts with an empty allowance")
		}
	}
}
