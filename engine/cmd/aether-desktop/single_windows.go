//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// One window, however many times somebody double-clicks.
//
// Found by launching it twice: two windows opened, each with its own WebView2
// profile lock and its own idea of the session. Every communications client on
// this desktop answers a second launch by raising the window that already
// exists, and an app that does not is one somebody ends up with four copies of
// by Friday.

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	user32         = syscall.NewLazyDLL("user32.dll")
	procCreateMutx = kernel32.NewProc("CreateMutexW")
	procFindWindow = user32.NewProc("FindWindowW")
	procSetForeg   = user32.NewProc("SetForegroundWindow")
	procShowWindow = user32.NewProc("ShowWindow")
	procIsIconic   = user32.NewProc("IsIconic")
)

const (
	errAlreadyExists = 183
	swRestore        = 9
	swShow           = 5
)

// claimSingleInstance returns false if another copy is already running, having
// first raised its window.
//
// The mutex is held for the life of the process and released by Windows when
// it exits, including when it crashes -- which is the property that matters,
// because a lock file left behind by a crash is a program that will not start
// again until somebody deletes a file they do not know about.
func claimSingleInstance(name string) bool {
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		// Cannot form the name: proceed rather than refuse to start.
		return true
	}
	_, _, lastErr := procCreateMutx.Call(0, 0, uintptr(unsafe.Pointer(n)))
	if errno, ok := lastErr.(syscall.Errno); ok && errno == errAlreadyExists {
		raiseExistingWindow(appName)
		return false
	}
	return true
}

// raiseExistingWindow brings the running copy to the front, restoring it if it
// was minimised. Best effort: failing to raise it is not a reason to open a
// second one.
func raiseExistingWindow(title string) {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return
	}
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	} else {
		procShowWindow.Call(hwnd, swShow)
	}
	procSetForeg.Call(hwnd)
}
