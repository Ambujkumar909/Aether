package sfu

import (
	"sync"
	"time"
)

// A burst allowance that is created per connection is not an allowance.
//
// This is not a hypothetical. A sibling project measured seven reconnections
// in thirty-two seconds, each one constructing a fresh token bucket and each
// one spending a full fresh burst, which turned a ceiling into a suggestion
// at precisely the moment the network was least healthy. The connection was
// reconnecting *because* the path was bad, and the reconnect handed it a new
// budget to make the path worse with.
//
// The failure is structural rather than accidental: whenever a rate limit
// lives inside the object that a failure destroys and recreates, failure
// resets the limit. So the bucket has to outlive the connection and be keyed
// to the thing that actually persists -- the participant.

// burstPool holds burst buckets keyed by participant identity so they
// survive reconnection.
type burstPool struct {
	mu      sync.Mutex
	buckets map[string]*pooledBucket
}

type pooledBucket struct {
	tokens   float64
	lastSeen time.Time
	lastFill time.Time
}

var pool = &burstPool{buckets: make(map[string]*pooledBucket)}

// BurstMemory is how long a participant's spent burst allowance is
// remembered after their connection goes away. It needs to comfortably
// exceed a reconnect cycle, or the mechanism achieves nothing; it should not
// be so long that someone who genuinely leaves and rejoins an hour later is
// penalised for a burst they spent in a different meeting.
const BurstMemory = 2 * time.Minute

// AttachBurstPool binds an estimator to a persistent bucket for a
// participant. Calling it again after a reconnect restores whatever the
// previous connection had left, rather than granting a fresh burst.
func (e *Estimator) AttachBurstPool(peerID string, now time.Time) {
	if peerID == "" {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.sweepLocked(now)

	b, ok := pool.buckets[peerID]
	if !ok {
		b = &pooledBucket{tokens: 0, lastFill: now}
		pool.buckets[peerID] = b
	}
	b.lastSeen = now

	// Refill for the time the participant was disconnected. Being away is
	// not cheating -- it is genuinely time during which they consumed
	// nothing -- so the bucket refills normally rather than being frozen.
	dt := now.Sub(b.lastFill).Seconds()
	if dt > 0 {
		capacity := e.rate * float64(e.cfg.BurstBucketMs) / 1000
		b.tokens += e.rate * dt
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.lastFill = now
	}

	e.tokens = b.tokens
	e.lastToken = now
	e.peerID = peerID
}

// syncBurstPool writes the estimator's bucket back to the pool.
func (e *Estimator) syncBurstPool(now time.Time) {
	if e.peerID == "" {
		return
	}
	pool.mu.Lock()
	if b, ok := pool.buckets[e.peerID]; ok {
		b.tokens = e.tokens
		b.lastFill = now
		b.lastSeen = now
	}
	pool.mu.Unlock()
}

// sweepLocked drops buckets nobody has touched recently.
func (p *burstPool) sweepLocked(now time.Time) {
	for id, b := range p.buckets {
		if now.Sub(b.lastSeen) > BurstMemory {
			delete(p.buckets, id)
		}
	}
}

// BurstPoolSize reports how many buckets are retained, for telemetry and for
// the test that proves the sweep works.
func BurstPoolSize() int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return len(pool.buckets)
}

// ResetBurstPool clears the pool. Tests only.
func ResetBurstPool() {
	pool.mu.Lock()
	pool.buckets = make(map[string]*pooledBucket)
	pool.mu.Unlock()
}
