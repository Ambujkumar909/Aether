//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// The tray, and the taskbar icon.
//
// Two reports, one cause: the window carried no icon of its own, so Windows
// drew the default one in the taskbar and the title bar; and minimising put
// the app in the taskbar rather than out of the way, which is not how a
// communications client behaves. Teams, and everything like it, lives in the
// notification area and comes back when you click it.
//
// The tray is built on a message-only window of our own rather than by
// subclassing the WebView2 window. Replacing another library's window
// procedure to intercept a minimise is the kind of change that works until the
// library reorders something, and the failure is a window that stops
// responding to the mouse.

var (
	shell32           = syscall.NewLazyDLL("shell32.dll")
	procShellNotify   = shell32.NewProc("Shell_NotifyIconW")
	procLoadImage     = user32.NewProc("LoadImageW")
	procSendMessage   = user32.NewProc("SendMessageW")
	procIsWindowVis   = user32.NewProc("IsWindowVisible")
	procSetForeground = user32.NewProc("SetForegroundWindow")
)

const (
	imageIcon      = 1
	lrLoadFromFile = 0x00000010
	lrDefaultSize  = 0x00000040
	wmSetIcon      = 0x0080
	iconSmall      = 0
	iconBig        = 1
	swHide         = 0
	niMAdd         = 0x00000000
	niMDelete      = 0x00000002
	nifMessage     = 0x00000001
	nifIcon        = 0x00000002
	nifTip         = 0x00000004
)

// installWindowIcon gives the window, the taskbar and alt-tab the brand mark.
//
// Loaded from a file rather than compiled in as a resource, because embedding
// one needs a resource compiler in the build and this needs none: the icon
// ships in the binary as bytes, is written beside the settings once, and is
// loaded from there. Multi-resolution, so Windows picks the right size for
// each place it draws rather than resampling one into mush.
func installWindowIcon(hwnd uintptr, dir string) error {
	if hwnd == 0 {
		return nil
	}
	path := filepath.Join(dir, "aether.ico")
	if err := os.WriteFile(path, brandIcon, 0o644); err != nil {
		return err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	for _, size := range []struct{ which, cx, cy uintptr }{
		{iconBig, 32, 32},
		{iconSmall, 16, 16},
	} {
		h, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), imageIcon,
			size.cx, size.cy, lrLoadFromFile)
		if h != 0 {
			procSendMessage.Call(hwnd, wmSetIcon, size.which, h)
		}
	}
	return nil
}

// notifyIconData is NOTIFYICONDATAW, to the fields this uses.
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// Tray is the notification-area presence.
type Tray struct {
	hwnd uintptr
	icon uintptr
	big  uintptr // 32px, for the notification's picture
	data notifyIconData
}

// NewTray puts the mark in the notification area, attached to the app window.
func NewTray(hwnd uintptr, dir, tip string) (*Tray, error) {
	path := filepath.Join(dir, "aether.ico")
	if err := os.WriteFile(path, brandIcon, 0o644); err != nil {
		return nil, err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	icon, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), imageIcon,
		16, 16, lrLoadFromFile)
	big, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), imageIcon,
		32, 32, lrLoadFromFile)

	t := &Tray{hwnd: hwnd, icon: icon, big: big}
	t.data = notifyIconData{
		CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:   hwnd,
		UID:    1,
		// nifMessage is what makes the icon clickable. Without it the icon
		// appears and does nothing, which -- once minimising hides the window
		// -- means the app has gone somewhere the person cannot get it back
		// from.
		UFlags:           nifIcon | nifTip | nifMessage,
		UCallbackMessage: wmTrayCallback,
		HIcon:            icon,
	}
	tipUTF, _ := syscall.UTF16FromString(tip)
	copy(t.data.SzTip[:], tipUTF)

	r, _, _ := procShellNotify.Call(niMAdd, uintptr(unsafe.Pointer(&t.data)))
	if r == 0 {
		return nil, syscall.EINVAL
	}
	return t, nil
}

// Remove takes the icon out of the notification area. Called on the way out,
// because an icon left behind is the ghost every user has seen and nobody can
// explain.
func (t *Tray) Remove() {
	if t == nil {
		return
	}
	procShellNotify.Call(niMDelete, uintptr(unsafe.Pointer(&t.data)))
}

// HideToTray takes the window off the taskbar.
func HideToTray(hwnd uintptr) {
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

// RestoreFromTray brings it back and puts it in front.
func RestoreFromTray(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, swRestore)
	procSetForeground.Call(hwnd)
}

// WindowVisible reports whether the window is on screen at all.
func WindowVisible(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	v, _, _ := procIsWindowVis.Call(hwnd)
	return v != 0
}

// Minimised reports whether the window is minimised.
func Minimised(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	v, _, _ := procIsIconic.Call(hwnd)
	return v != 0
}
