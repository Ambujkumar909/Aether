//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Making the window look like a Windows application rather than a browser in a
// box.
//
// The report was that it "feels like seeing the app running in a virtual
// machine". That is a precise symptom with a precise cause, and it is not
// colour: it is DPI.
//
// A process that has not declared itself DPI-aware is lied to by Windows. It
// is told the screen is 96 DPI, it renders at that size, and Windows then
// bitmap-stretches the result to the real scaling factor -- 125% or 150% on
// most laptops. Every glyph edge and every hairline border goes through a
// resampling filter. The result is soft, slightly washed out, and looks
// exactly like a remote desktop session or a VM console, because those are
// scaled bitmaps too. Nothing about the page is wrong; it is being
// photographed and enlarged.
//
// Declaring per-monitor awareness makes the window render at the real pixel
// density, and the softness goes away.

var (
	dwmapi                 = syscall.NewLazyDLL("dwmapi.dll")
	procSetProcessDpiCtx   = user32.NewProc("SetProcessDpiAwarenessContext")
	procDwmSetWindowAttr   = dwmapi.NewProc("DwmSetWindowAttribute")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procGetDpiForWindow    = user32.NewProc("GetDpiForWindow")
	procSystemParamsInfoW  = user32.NewProc("SystemParametersInfoW")
	procGetWindowLongPtrW  = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	procRedrawWindow       = user32.NewProc("RedrawWindow")
	procInvalidateRectW    = user32.NewProc("InvalidateRect")
	procUpdateWindowHandle = user32.NewProc("UpdateWindow")
)

// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2. Per-monitor rather than
// system-wide, because a laptop docked to an external screen at a different
// scale is the ordinary case and the window has to re-render, not re-stretch,
// when it is dragged across.
const dpiPerMonitorV2 = ^uintptr(3) // -4 as an unsigned word

// DWMWA_USE_IMMERSIVE_DARK_MODE
const dwmUseImmersiveDarkMode = 20

// declareDPIAware must run before any window exists.
//
// Best effort by design. On a Windows old enough to lack the call the window
// is merely scaled, which is how it behaved before this existed -- worth a
// soft window, not worth refusing to start.
func declareDPIAware() bool {
	if procSetProcessDpiCtx.Find() != nil {
		return false
	}
	r, _, _ := procSetProcessDpiCtx.Call(dpiPerMonitorV2)
	return r != 0
}

// matchTitleBarToApp paints the window frame dark when the application is
// dark.
//
// A dark page under a light title bar is the other half of "this looks like a
// browser": every native application on this desktop has a frame that matches
// its content, and the ones that do not are the ones that were ported.
func matchTitleBarToApp(hwnd uintptr, dark bool) {
	if hwnd == 0 || procDwmSetWindowAttr.Find() != nil {
		return
	}
	var on int32
	if dark {
		on = 1
	}
	procDwmSetWindowAttr.Call(
		hwnd,
		uintptr(dwmUseImmersiveDarkMode),
		uintptr(unsafe.Pointer(&on)),
		unsafe.Sizeof(on),
	)
	// The frame is only repainted on the next change, so nudge it.
	procRedrawWindow.Call(hwnd, 0, 0, 0x0001|0x0100) // RDW_INVALIDATE|RDW_UPDATENOW
}

// systemPrefersDark reads the shell's own setting, so the frame follows the
// same preference the page does.
func systemPrefersDark() bool {
	const path = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`
	k, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	var h syscall.Handle
	advapi := syscall.NewLazyDLL("advapi32.dll")
	openKey := advapi.NewProc("RegOpenKeyExW")
	queryValue := advapi.NewProc("RegQueryValueExW")
	closeKey := advapi.NewProc("RegCloseKey")

	// HKEY_CURRENT_USER, KEY_READ
	r, _, _ := openKey.Call(uintptr(0x80000001), uintptr(unsafe.Pointer(k)), 0, 0x20019,
		uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return false
	}
	defer closeKey.Call(uintptr(h))

	name, err := syscall.UTF16PtrFromString("AppsUseLightTheme")
	if err != nil {
		return false
	}
	var data uint32
	size := uint32(4)
	var kind uint32
	r, _, _ = queryValue.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0,
		uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return false
	}
	// The value is "apps use LIGHT theme", so dark is zero.
	return data == 0
}

// scaleForDPI converts a size in CSS pixels to the physical pixels the window
// has to be to give the page that much room.
//
// This is the bug behind "resizing misplaces everything", and it was not a
// layout problem at all. The window library takes its width and height in
// PHYSICAL pixels; the page inside is laid out in CSS pixels, which on a 200%
// display are half as many. Asking for a 1280 pixel window on this laptop gave
// the client 653 CSS pixels to work with -- permanently below its own 940px
// breakpoint -- so it reflowed into the narrow arrangement every single time
// and looked broken at any window size.
//
// Falls back to 1:1 where the call is unavailable, which is the behaviour on a
// display that is not scaled anyway.
func scaleForDPI(hwnd uintptr, cssPixels int) int {
	if hwnd == 0 || procGetDpiForWindow.Find() != nil {
		return cssPixels
	}
	dpi, _, _ := procGetDpiForWindow.Call(hwnd)
	if dpi == 0 {
		return cssPixels
	}
	return cssPixels * int(dpi) / 96
}

// CentreOnScreen puts the window in the middle of the display it is on.
//
// The library centres on the primary monitor's full bounds, which on a screen
// with a taskbar and at this window's size put a third of it off the edge. The
// work area is the right rectangle: it is what "the screen" means to somebody
// looking at one.
func CentreOnScreen(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	var r rect
	procGetWindowRectFn.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	w := r.Right - r.Left
	h := r.Bottom - r.Top
	if w <= 0 || h <= 0 {
		return
	}
	mi, ok := workArea(hwnd)
	if !ok {
		return
	}
	aw := mi.RcWork.Right - mi.RcWork.Left
	ah := mi.RcWork.Bottom - mi.RcWork.Top
	x := mi.RcWork.Left + (aw-w)/2
	y := mi.RcWork.Top + (ah-h)/2
	if x < mi.RcWork.Left {
		x = mi.RcWork.Left
	}
	if y < mi.RcWork.Top {
		y = mi.RcWork.Top
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0,
		swpNoSize|swpNoActivate)
}
