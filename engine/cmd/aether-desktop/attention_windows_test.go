//go:build windows

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The attention paths run against real Win32 objects: a message-only tray
// window, a real notification-area icon, the taskbar overlay and the flash.
// They are exercised the way the app does it -- on one locked OS thread,
// with the window hidden -- because that is the state a report of "it
// vanished from the tray" points at, and a fake cannot crash the way the
// shell does.
func TestAttentionPathsSurviveOnAHiddenWindow(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("needs an interactive desktop session")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	tw, err := NewTrayWindow(func() {}, func() {}, func() MenuState { return MenuState{} }, func(uintptr) {})
	if err != nil {
		t.Skipf("no tray window on this session: %v", err)
	}
	defer tw.Close()
	dir := t.TempDir()
	tray, err := NewTray(tw.HWND(), dir, "Aether test icon")
	if err != nil {
		t.Skipf("no notification area on this session: %v", err)
	}
	defer tray.Remove()
	if _, err := os.Stat(filepath.Join(dir, "aether.ico")); err != nil {
		t.Fatalf("the icon file was not written: %v", err)
	}

	hwnd := tw.HWND()
	// Everything the client can ask for while the window is out of sight,
	// several times over, in both orders.
	for i := 0; i < 3; i++ {
		tray.Notify("Aether test", "a message while hidden")
		tray.SetBadge(hwnd, 3)
		tray.SetBadge(hwnd, 12)
		FlashTaskbar(hwnd)
		tray.SetBadge(hwnd, 0)
		StopFlashing(hwnd)
	}
	// And a nil tray -- the bindings can fire before the tray exists.
	var none *Tray
	none.Notify("x", "y")
	none.SetBadge(hwnd, 1)
	none.SetBadge(hwnd, 0)
	// The overlay path against a window that does not exist must not crash
	// either.
	setOverlay(0, 0, "")
	_ = log
}
