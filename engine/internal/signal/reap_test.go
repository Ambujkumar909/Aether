package signal

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
)

// An empty room is reaped with its media router; an occupied one keeps
// both. (The router used to outlive its room for the life of the process:
// nothing called Drop.)
func TestReapingARoomDropsItsRouter(t *testing.T) {
	meter := ledger.NewMeter("t")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := NewGateway(room.NewRegistry(meter), sfu.NewRegistry(meter, quiet, nil), meter, quiet)

	empty := g.Rooms.GetOrCreate("empty")
	g.Media.GetOrCreate("empty")
	busy := g.Rooms.GetOrCreate("busy")
	g.Media.GetOrCreate("busy")
	if _, err := busy.Join(&room.Participant{ID: "p", Send: func([]byte) {}}); err != nil {
		t.Fatal(err)
	}
	empty.Created = time.Now().Add(-time.Hour)
	busy.Created = time.Now().Add(-time.Hour)

	if n := g.Rooms.Reap(time.Minute, g.dropMedia); n != 1 {
		t.Fatalf("reaped %d rooms, want 1", n)
	}
	if _, ok := g.Media.Get("empty"); ok {
		t.Fatal("the empty room's router outlived it")
	}
	if _, ok := g.Media.Get("busy"); !ok {
		t.Fatal("an occupied room lost its router")
	}
	if _, ok := g.Rooms.Get("busy"); !ok {
		t.Fatal("an occupied room was reaped")
	}
	// The name is free again: a new meeting there gets a fresh router.
	if r := g.Media.GetOrCreate("empty"); r == nil {
		t.Fatal("no router for a new meeting with a reaped room's name")
	}
}
