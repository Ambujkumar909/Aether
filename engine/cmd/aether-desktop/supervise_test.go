package main

import (
	"strings"
	"testing"
	"time"
)

// The restart policy, with numbers: a clean exit stops; a configuration
// error stops; a crash restarts, up to the bound inside the window; crashes
// outside the window are forgotten.
func TestSupervisorRestartsCrashesAndOnlyCrashes(t *testing.T) {
	var notes []string
	note := func(s string) { notes = append(notes, s) }
	clock := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	sleep := func(d time.Duration) { clock = clock.Add(d) }

	// Clean exit: once, done.
	runs := 0
	if code := supervisePolicy(func(int) int { runs++; return 0 }, now, sleep, note); code != 0 || runs != 1 {
		t.Fatalf("clean exit: code %d runs %d", code, runs)
	}
	// Configuration error (1): no restart.
	runs = 0
	if code := supervisePolicy(func(int) int { runs++; return 1 }, now, sleep, note); code != 1 || runs != 1 {
		t.Fatalf("exit 1: code %d runs %d", code, runs)
	}
	// A panic (2) twice, then the app runs and exits cleanly.
	runs = 0
	notes = nil
	code := supervisePolicy(func(attempt int) int {
		runs++
		if attempt <= 2 {
			return 2
		}
		return 0
	}, now, sleep, note)
	if code != 0 || runs != 3 {
		t.Fatalf("two crashes then clean: code %d runs %d", code, runs)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "restarting") {
		t.Fatalf("notes: %v", notes)
	}
	// A native fault (negative / large exception code) restarts too.
	runs = 0
	code = supervisePolicy(func(attempt int) int {
		runs++
		if attempt == 1 {
			return -1073741819 // 0xC0000005 as int32
		}
		return 0
	}, now, sleep, note)
	if code != 0 || runs != 2 {
		t.Fatalf("native fault then clean: code %d runs %d", code, runs)
	}
	// Crashing on every start: bounded to maxRestarts, then it gives up.
	runs = 0
	notes = nil
	code = supervisePolicy(func(int) int { runs++; return 2 }, now, sleep, note)
	if code != 2 || runs != maxRestarts+1 {
		t.Fatalf("crash loop: code %d runs %d", code, runs)
	}
	if !strings.Contains(notes[len(notes)-1], "not restarting again") {
		t.Fatalf("last note: %q", notes[len(notes)-1])
	}
	// Crashes far apart do not accumulate: one every hour restarts forever
	// within reason (here: five, all restarted).
	runs = 0
	code = supervisePolicy(func(attempt int) int {
		runs++
		clock = clock.Add(time.Hour)
		if attempt <= 5 {
			return 2
		}
		return 0
	}, now, sleep, note)
	if code != 0 || runs != 6 {
		t.Fatalf("spaced crashes: code %d runs %d", code, runs)
	}
}

func TestChildMarkerIsStrippedFromArgs(t *testing.T) {
	args, child := isChild([]string{"-server", "https://x", "-child", "-v"})
	if !child || strings.Join(args, " ") != "-server https://x -v" {
		t.Fatalf("child %v args %v", child, args)
	}
	args, child = isChild([]string{"-server", "https://x"})
	if child || len(args) != 2 {
		t.Fatalf("not a child: %v %v", child, args)
	}
	for _, c := range []int{2, -1, 256, -1073741819} {
		if !restartable(c) {
			t.Errorf("%d should restart", c)
		}
	}
	for _, c := range []int{0, 1, 3, 255} {
		if restartable(c) {
			t.Errorf("%d should not restart", c)
		}
	}
}
