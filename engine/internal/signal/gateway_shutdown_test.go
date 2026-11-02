package signal

import (
	"context"
	"testing"
	"time"
)

// Server shutdown must not take time proportional to the number of connected
// participants.
//
// Gateway.Stop closes every live connection while holding the gateway lock. A
// graceful websocket close waits for the peer to echo a close frame, so before
// close was made non-blocking this cost up to five seconds PER CONNECTION,
// serially, with the gateway lock held throughout. A node with a hundred
// participants would have taken over eight minutes to shut down, long past any
// orchestrator's termination grace period, at which point it is killed and
// every unflushed audit event goes with it.
func TestGatewayStopIsNotProportionalToConnections(t *testing.T) {
	noLeaks(t)
	g := &Gateway{conns: map[*Conn]struct{}{}, byID: map[string]*Conn{}}

	const n = 8
	for i := 0; i < n; i++ {
		c, _ := newTestConn(t) // connected, but the far end never reads
		g.conns[c] = struct{}{}
	}

	start := time.Now()
	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	t.Logf("shut down %d connected participants in %v", n, elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("stopping with %d participants took %v; shutdown must not scale "+
			"with the number of connections", n, elapsed)
	}
	if len(g.conns) != 0 {
		t.Fatalf("%d connections left registered after Stop", len(g.conns))
	}
}

// Stopping twice must be safe. The kernel guards module Stop, but a gateway
// is also stopped by its own error paths.
func TestGatewayDoubleStopIsSafe(t *testing.T) {
	noLeaks(t)
	g := &Gateway{conns: map[*Conn]struct{}{}, byID: map[string]*Conn{}}
	c, _ := newTestConn(t)
	g.conns[c] = struct{}{}

	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// Every connection must actually be closed by Stop, not merely forgotten.
// A forgotten connection keeps its socket, its coalescing timer and its
// dictionaries alive with nothing left to close them.
func TestGatewayStopClosesEveryConnection(t *testing.T) {
	noLeaks(t)
	g := &Gateway{conns: map[*Conn]struct{}{}, byID: map[string]*Conn{}}

	conns := make([]*Conn, 0, 4)
	for i := 0; i < 4; i++ {
		c, _ := newTestConn(t)
		c.Send([]byte{0x01}) // arm the coalescing timer
		g.conns[c] = struct{}{}
		conns = append(conns, c)
	}

	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, c := range conns {
		c.mu.Lock()
		closed, timer := c.closed, c.timer
		c.mu.Unlock()
		if !closed {
			t.Fatalf("connection %d survived gateway shutdown", i)
		}
		if timer != nil {
			t.Fatalf("connection %d kept its coalescing timer after shutdown", i)
		}
	}
}
