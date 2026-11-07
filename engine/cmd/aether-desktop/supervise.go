//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// The supervisor.
//
// A communications client that is not running cannot ring. When the app
// vanished from the notification area without being quit, nothing was left
// behind to say why: the process was gone and its output with it. So the
// executable now runs in two roles. The parent is a supervisor: it starts
// itself again as the child (with -child), sends the child's output --
// including a Go panic's stack -- to a log file beside the settings, and if
// the child dies of anything but a clean exit, starts it again. The child is
// the application as it always was.
//
// The policy is deliberately narrow. A crash restarts (a Go panic exits 2; a
// native fault exits with a Windows exception code, which comes back as a
// large or negative number). A clean exit, a configuration error (1), or a
// second copy that found the first already running does not: looping on a
// bad address would be worse than stopping. And the restarts are bounded to
// maxRestarts in restartWindow, so a crash on start-up does not become a
// process that reappears forever.

const (
	childFlag     = "-child"
	maxRestarts   = 3
	restartWindow = 10 * time.Minute
	restartDelay  = 1500 * time.Millisecond
	logKeepBytes  = 5 << 20
)

// restartable says whether an exit code is one the supervisor should answer
// with a restart.
func restartable(code int) bool {
	if code == 2 {
		return true // Go runtime: panic, fatal error
	}
	return code < 0 || code > 255 // a Windows exception status, e.g. 0xC0000005
}

// supervisePolicy runs the child through fn until it exits cleanly, fails in
// a way that is not a crash, or crashes more than maxRestarts times inside
// restartWindow. It is separated from the process plumbing so the policy can
// be tested with numbers. now is injected for the same reason.
func supervisePolicy(fn func(attempt int) int, now func() time.Time, sleep func(time.Duration), note func(string)) int {
	var crashes []time.Time
	for attempt := 1; ; attempt++ {
		code := fn(attempt)
		if code == 0 || !restartable(code) {
			return code
		}
		t := now()
		crashes = append(crashes, t)
		// Only crashes inside the window count.
		kept := crashes[:0]
		for _, c := range crashes {
			if t.Sub(c) <= restartWindow {
				kept = append(kept, c)
			}
		}
		crashes = kept
		if len(crashes) > maxRestarts {
			note(fmt.Sprintf("crashed %d times in %s; not restarting again (last exit code %d)", len(crashes), restartWindow, code))
			return code
		}
		note(fmt.Sprintf("crashed (exit code %d); restarting in %s (%d of %d)", code, restartDelay, len(crashes), maxRestarts))
		sleep(restartDelay)
	}
}

// openLog opens the persistent log beside the settings, trimming it when it
// has grown past logKeepBytes by keeping the newer half.
func openLog(dir string) (*os.File, error) {
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(logs, "desktop.log")
	if st, err := os.Stat(path); err == nil && st.Size() > logKeepBytes {
		if raw, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(path, raw[len(raw)/2:], 0o600)
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// supervise is the parent role: run the child until the policy says stop,
// and return the exit code to hand to the OS.
func supervise(dir string, args []string) int {
	logf, err := openLog(dir)
	if err != nil {
		// No log is not a reason not to run; the child goes to stderr.
		logf = nil
	}
	out := io.Writer(os.Stderr)
	if logf != nil {
		defer logf.Close()
		out = io.MultiWriter(os.Stderr, logf)
	}
	note := func(s string) {
		fmt.Fprintf(out, "time=%s level=INFO msg=%q role=supervisor\n", time.Now().Format(time.RFC3339), s)
	}
	exe, err := os.Executable()
	if err != nil {
		note("cannot find own executable: " + err.Error())
		return 1
	}
	return supervisePolicy(func(attempt int) int {
		note(fmt.Sprintf("starting %s (attempt %d)", filepath.Base(exe), attempt))
		cmd := exec.Command(exe, append(append([]string{}, args...), childFlag)...)
		cmd.Stdout = out
		cmd.Stderr = out
		cmd.Stdin = nil
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			note("could not start the child: " + err.Error())
			return 1
		}
		return 0
	}, time.Now, time.Sleep, note)
}

// isChild reports whether this process is the application role, and strips
// the marker from the arguments so the flags parse as before.
func isChild(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	child := false
	for _, a := range args {
		if a == childFlag {
			child = true
			continue
		}
		out = append(out, a)
	}
	return out, child
}
