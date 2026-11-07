package sfu

import (
	"sync"
	"testing"

	"github.com/aether-rtc/aether/internal/ledger"
)

// Every join installs the meeting-controls hook while media already flows
// through it. As a plain field that was a data race (two joins at once,
// found by -race on Linux); run under -race this fails against it.
func TestPermitMayBeSetWhileMediaFlows(t *testing.T) {
	r := NewRouter("race", ledger.NewMeter("race"), quietLog(), nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.SetPermit(func(string, string) bool { return true }) }()
		go func() { defer wg.Done(); _ = r.permits("p", "video") }()
	}
	wg.Wait()
	r.SetPermit(func(_, kind string) bool { return kind == "audio" })
	if r.permits("p", "video") || !r.permits("p", "audio") {
		t.Fatal("the installed hook is not the one consulted")
	}
}
