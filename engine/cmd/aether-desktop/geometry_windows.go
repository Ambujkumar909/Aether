//go:build windows

package main

import "unsafe"

// The few Win32 shapes the window helpers need, kept here now that the custom
// caption that first introduced them has gone.

type rect struct{ Left, Top, Right, Bottom int32 }

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

const monitorDefaultToNearest = 0x00000002

var (
	procGetWindowRectFn = user32.NewProc("GetWindowRect")
	procMonitorFromWin  = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfo  = user32.NewProc("GetMonitorInfoW")
)

func workArea(hwnd uintptr) (monitorInfo, bool) {
	var mi monitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	mon, _, _ := procMonitorFromWin.Call(hwnd, monitorDefaultToNearest)
	if mon == 0 {
		return mi, false
	}
	r, _, _ := procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi)))
	return mi, r != 0
}

// FitToScreen clamps a wanted window size, in physical pixels, to what the
// display can actually show.
//
// This is why the window opened with a third of it off the edge. The wanted
// size is expressed in CSS pixels and multiplied by the display scale, and on
// a 200% laptop screen a perfectly reasonable 1280x840 becomes 2560x1680 --
// taller than the work area. Windows does not refuse it; it simply puts a
// window there that does not fit, and centring a window that is too big only
// moves the missing part from one edge to both.
//
// A margin is left so the window reads as a window rather than as something
// that failed to maximise.
func FitToScreen(hwnd uintptr, wantW, wantH int) (int, int) {
	mi, ok := workArea(hwnd)
	if !ok {
		return wantW, wantH
	}
	maxW := int(mi.RcWork.Right-mi.RcWork.Left) - 80
	maxH := int(mi.RcWork.Bottom-mi.RcWork.Top) - 80
	if maxW > 0 && wantW > maxW {
		wantW = maxW
	}
	if maxH > 0 && wantH > maxH {
		wantH = maxH
	}
	return wantW, wantH
}

// RestoreWindow takes the window out of a maximised or minimised state, so a
// size can actually be applied to it.
func RestoreWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	const swRestore = 9
	procShowWindow.Call(hwnd, swRestore)
}
