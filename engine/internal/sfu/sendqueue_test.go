package sfu

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// A receiver that is not keeping up must cost the publisher nothing: handing
// a packet over never waits, a full queue drops and counts, and the close
// paths (double, concurrent, racing a push, push after) are all harmless.
func TestSendQueueNeverBlocksThePublisher(t *testing.T) {
	q := &sendQueue{ch: make(chan queued, sendQueueLen), done: make(chan struct{})} // no writer: stuck
	d := &Downstream{}
	start := time.Now()
	accepted := 0
	for range sendQueueLen * 4 {
		if q.push(nil, d, rtp.Packet{}) {
			accepted++
		}
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("handing packets to a stuck receiver took %v", took)
	}
	if accepted != sendQueueLen || q.dropped.Load() != sendQueueLen*3 {
		t.Fatalf("accepted %d, dropped %d; want %d and %d", accepted, q.dropped.Load(), sendQueueLen, sendQueueLen*3)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); q.close() }()
		go func() { defer wg.Done(); q.push(nil, d, rtp.Packet{}) }()
	}
	wg.Wait()
	q.close()
	if q.push(nil, d, rtp.Packet{}) {
		t.Fatal("a closed queue accepted a packet")
	}
	var never *sendQueue
	never.close() // a peer built without one
}

// Closing stops the writer goroutine.
func TestSendQueueWriterStopsOnClose(t *testing.T) {
	before := runtime.NumGoroutine()
	qs := make([]*sendQueue, 50)
	for i := range qs {
		qs[i] = newSendQueue()
	}
	for _, q := range qs {
		q.close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines after closing, %d before", n, before)
	}
}
