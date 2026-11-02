//go:build windows

package main

import "unsafe"

// One bar, not two.
//
// The system caption and the application's own bar stacked into two bands at
// the top of every window, and the report was exactly that. The first attempt
// at fixing it replaced the window procedure and handled WM_NCCALCSIZE by hand
// to remove the whole non-client frame. It removed the resize borders with it,
// deadlocked on a re-entrant message, and left the web view laid out for a
// frame that no longer existed. Three regressions, then a revert.
//
// The correct change is much smaller. WS_CAPTION is one style bit. Clearing
// it, and only it, removes the caption strip while leaving WS_THICKFRAME,
// WS_MINIMIZEBOX and WS_MAXIMIZEBOX in place -- so Windows keeps drawing the
// resize borders, keeps handling the edges, keeps snapping and maximising to
// the work area, and keeps every keyboard shortcut. No window procedure is
// replaced. The frame change is announced with SWP_FRAMECHANGED, which raises
// the ordinary WM_SIZE the view library already listens for, so the page
// re-fits on its own.
//
// What the page then has to supply is exactly what the caption used to: a
// place to drag from, and three buttons. Dragging is handed back to Windows
// with WM_NCLBUTTONDOWN/HTCAPTION from a mousedown, which is the same message
// the real caption would have sent.

const (
	gwlStyle = ^uintptr(15) // GWL_STYLE is -16

	wsCaption = 0x00C00000
	wsSysMenu = 0x00080000

	swpFrameChanged = 0x0020
	swpNoZOrder     = 0x0004

	wmNCLButtonDown = 0x00A1
	wmSysCommand    = 0x0112
	wmClose         = 0x0010
	htCaption       = 2

	scMinimize = 0xF020
	scMaximize = 0xF030
	scRestore  = 0xF120
)

var (
	procReleaseCapture = user32.NewProc("ReleaseCapture")
	procIsZoomed       = user32.NewProc("IsZoomed")
	procPostMessageW2  = user32.NewProc("PostMessageW")
)

// StripCaption removes the system title bar and nothing else.
//
// Best effort: if the style cannot be read the window keeps its caption,
// which is worse looking and entirely functional.
func StripCaption(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	if style == 0 {
		return false
	}
	// WS_SYSMENU goes too: without a caption there is nowhere for its icon
	// to live, and leaving it makes Windows reserve room for one.
	style &^= wsCaption | wsSysMenu
	if r, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlStyle, style); r == 0 {
		return false
	}
	// Announce the frame change. This raises WM_SIZE, which is what makes the
	// web view re-fit the client area it now has.
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
	return true
}

// StartWindowDrag hands a drag to Windows, from a mousedown in the page.
//
// This is why nothing is hit-tested in the shell: the page knows which of its
// elements are interactive and the shell does not, so the page decides what
// is a handle and calls this only for those.
func StartWindowDrag(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procReleaseCapture.Call()
	procSendMessage.Call(hwnd, wmNCLButtonDown, htCaption, 0)
}

func MinimiseWindow(hwnd uintptr) {
	if hwnd != 0 {
		procPostMessageW2.Call(hwnd, wmSysCommand, scMinimize, 0)
	}
}

func ToggleMaximise(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	if WindowIsMaximised(hwnd) {
		procPostMessageW2.Call(hwnd, wmSysCommand, scRestore, 0)
		return
	}
	procPostMessageW2.Call(hwnd, wmSysCommand, scMaximize, 0)
}

func CloseWindow(hwnd uintptr) {
	if hwnd != 0 {
		procPostMessageW2.Call(hwnd, wmClose, 0, 0)
	}
}

func WindowIsMaximised(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	z, _, _ := procIsZoomed.Call(hwnd)
	return z != 0
}

var _ = unsafe.Pointer(nil)
