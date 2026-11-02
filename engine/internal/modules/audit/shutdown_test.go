package audit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Stopping twice must not panic.
//
// Shutdown is the worst possible place for a panic: it runs while the process
// is already going down, often on an error path, and it takes the rest of the
// orderly teardown with it -- unflushed audit events included, which is
// precisely the data that must not be lost.
//
// The kernel does guard against calling a module's Stop twice, so this is not
// reachable through the normal path today. It is still worth holding, because
// the guard lives in a different package from the thing it protects, and a
// Stop that panics on a second call is a trap for the next caller: a failed
// Start that cleans up after itself, a test, a supervisor that retries.
func TestDoubleStopDoesNotPanic(t *testing.T) {
	svc, _ := testService(t)

	svc.Stop()
	svc.Stop()
	svc.Stop()
}

// Concurrent Stop must also be safe, and must not deadlock.
func TestConcurrentStopIsSafe(t *testing.T) {
	svc, _ := testService(t)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.Stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Stop deadlocked")
	}
}

// Stop must flush what was queued rather than dropping it. An audit log that
// loses its last events during shutdown is not tamper-evident, because the
// gap is indistinguishable from deletion.
func TestStopFlushesQueuedEvents(t *testing.T) {
	svc, _ := testService(t)

	const n = 50
	for i := 0; i < n; i++ {
		svc.Record(context.Background(), Event{
			ActorID: "u1", ActorEmail: "u1@example.com",
			Action: ActionLoginSucceeded, Outcome: OutcomeSuccess,
		})
	}
	svc.Stop()

	events, err := svc.repo.List(context.Background(), Filter{Limit: n * 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < n {
		t.Fatalf("Stop flushed %d of %d queued events; the rest were lost", len(events), n)
	}

	// The chain must still verify after a shutdown flush.
	res, err := svc.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("chain broken at seq %d after shutdown flush", res.BrokenAt)
	}
	t.Logf("%d events flushed on shutdown, chain verifies to seq %d", len(events), res.HeadSeq)
}

// Recording after Stop must not panic or block forever. Something always
// tries: a deferred audit call in a handler that outlives the module.
func TestRecordAfterStopIsSafe(t *testing.T) {
	svc, _ := testService(t)
	svc.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			svc.Record(context.Background(), Event{
				ActorID: "u1", Action: ActionLogout, Outcome: OutcomeSuccess,
			})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Record blocked after Stop")
	}
}
