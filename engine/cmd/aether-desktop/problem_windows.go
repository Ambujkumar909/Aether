//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// showProblem puts an unreachable server in front of somebody, in a window,
// with the address in it.
//
// The alternative is what the app did before: a WebView2 "refused to connect"
// page, which is the browser's failure message shown inside something that is
// not a browser, with no address bar and nothing to act on.
func showProblem(title, body string) {
	mb := user32.NewProc("MessageBoxW")
	t, err1 := syscall.UTF16PtrFromString(title)
	b, err2 := syscall.UTF16PtrFromString(body)
	if err1 != nil || err2 != nil {
		return
	}
	const mbIconWarning = 0x30
	mb.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), mbIconWarning)
}
