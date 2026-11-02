//go:build windows

package main

import (
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Two window messages the library gets wrong for an application like this,
// corrected by subclassing the window rather than replacing its procedure.
//
// SetWindowSubclass is the supported way to see a window's messages first
// and hand the rest on (DefSubclassProc). It is not what an earlier attempt
// did -- swapping the class procedure and handling WM_NCCALCSIZE by hand --
// which deadlocked on re-entry; neither message here re-enters anything.
//
// WM_CLOSE. The library destroys the window, which ends the message loop,
// which ends the program. A communications client does not end when its
// window closes: calls and messages still have to arrive, so close hides the
// window to the notification area and the process stays. Quit, from the
// tray menu, sets the flag that lets the close through.
//
// WM_GETMINMAXINFO. With the caption stripped, a maximised window is sized by
// Windows to the whole monitor and the taskbar hides behind it, as it does
// behind a full-screen game. Setting the maximised size and position to the
// monitor's work area here puts the taskbar back.

var (
	comctl32                = syscall.NewLazyDLL("comctl32.dll")
	procSetWindowSubclass   = comctl32.NewProc("SetWindowSubclass")
	procDefSubclassProc     = comctl32.NewProc("DefSubclassProc")
	procRemoveWindowSubcls  = comctl32.NewProc("RemoveWindowSubclass")
	shellSubclassID         = uintptr(0xAE7)
	shellSubclassCallback   uintptr
	closeMeansQuit          atomic.Bool
	closeHidesToTrayEnabled atomic.Bool
)

const wmGetMinMaxInfo = 0x0024

type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

// InstallShellSubclass hooks the two messages on the main window.
func InstallShellSubclass(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	if shellSubclassCallback == 0 {
		shellSubclassCallback = syscall.NewCallback(shellSubclassProc)
	}
	closeHidesToTrayEnabled.Store(true)
	r, _, _ := procSetWindowSubclass.Call(hwnd, shellSubclassCallback, shellSubclassID, 0)
	return r != 0
}

// LetCloseQuit makes the next WM_CLOSE end the program rather than hide it.
func LetCloseQuit() { closeMeansQuit.Store(true) }

func shellSubclassProc(hwnd, msg, wp, lp, id, ref uintptr) uintptr {
	switch msg {
	case wmClose:
		if closeHidesToTrayEnabled.Load() && !closeMeansQuit.Load() {
			HideToTray(hwnd)
			return 0
		}
	case wmGetMinMaxInfo:
		// The library's own handling first (it sets the minimum), then the
		// maximised geometry on top of it.
		r, _, _ := procDefSubclassProc.Call(hwnd, msg, wp, lp)
		if lp != 0 {
			if mi, ok := workArea(hwnd); ok {
				// lp is a pointer Windows owns for the duration of the
				// message; the conversion is the documented shape of
				// WM_GETMINMAXINFO.
				applyWorkArea((*minMaxInfo)(*(*unsafe.Pointer)(unsafe.Pointer(&lp))), mi, frameBorder(hwnd))
			}
		}
		return r
	}
	r, _, _ := procDefSubclassProc.Call(hwnd, msg, wp, lp)
	return r
}

// applyWorkArea sets the maximised geometry so that the CLIENT area is
// exactly the work area: the window's own resize frame (border pixels wide
// on every side) hangs off the screen, which is where Windows puts it for
// every maximised window. A first version set the outer rectangle to the
// work area instead, and the frame stayed on screen as a thin gap along the
// edges and at the corners. Position is relative to the monitor's origin,
// as WM_GETMINMAXINFO expects. Pure, so it can be tested with numbers.
func applyWorkArea(mmi *minMaxInfo, mi monitorInfo, border int32) {
	mmi.MaxPosition = point{X: mi.RcWork.Left - mi.RcMonitor.Left - border, Y: mi.RcWork.Top - mi.RcMonitor.Top - border}
	mmi.MaxSize = point{X: mi.RcWork.Right - mi.RcWork.Left + 2*border, Y: mi.RcWork.Bottom - mi.RcWork.Top + 2*border}
}

var procGetSystemMetricsForDpi = user32.NewProc("GetSystemMetricsForDpi")

const (
	smCxSizeFrame    = 32
	smCxPaddedBorder = 92
)

var procGetClientRect = user32.NewProc("GetClientRect")

// frameBorder is the width of this window's own frame, measured: half the
// difference between its outer and client widths. Measured rather than
// computed from system metrics because the metrics describe a window with
// a caption, and this one has none -- the computed value was one pixel too
// wide, which left the client area one pixel off the work area. Falls back
// to the metrics when the window cannot be measured.
func frameBorder(hwnd uintptr) int32 {
	var wr, cr rect
	okW, _, _ := procGetWindowRectFn.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
	okC, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	if okW != 0 && okC != 0 {
		if d := (wr.Right - wr.Left) - (cr.Right - cr.Left); d >= 0 && d < 64 {
			return d / 2
		}
	}
	dpi, _, _ := procGetDpiForWindow.Call(hwnd)
	if dpi == 0 {
		dpi = 96
	}
	a, _, _ := procGetSystemMetricsForDpi.Call(smCxSizeFrame, dpi)
	b, _, _ := procGetSystemMetricsForDpi.Call(smCxPaddedBorder, dpi)
	return int32(a + b)
}
