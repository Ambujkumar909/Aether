//go:build windows

package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

// The tray icon's own window.
//
// A notification-area icon delivers its clicks as a window message, so it
// needs a window to deliver them to. Attaching it to the WebView2 window would
// mean replacing that window's procedure to read them, which is the change
// that works until the library reorders something and then leaves a window
// that ignores the mouse.
//
// So the icon gets a window of its own: message-only, never shown, and owned
// entirely by this file. Its whole job is to hear a click and raise the real
// window.
//
// This exists because of a bug that was nearly shipped. Minimising hid the
// window before there was anything listening for a click, so the app went into
// the notification area and could not be brought back -- worse than the
// behaviour being fixed. A tray icon and the way out of the tray are one
// feature, not two.

var (
	procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	procAppendMenuW     = user32.NewProc("AppendMenuW")
	procTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	procDestroyMenu     = user32.NewProc("DestroyMenu")
	procGetCursorPos    = user32.NewProc("GetCursorPos")
	procPostMessageW    = user32.NewProc("PostMessageW")
	procRegisterClass   = user32.NewProc("RegisterClassExW")
	procCreateWindow    = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procGetMessage      = user32.NewProc("GetMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMsg     = user32.NewProc("DispatchMessageW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procPostQuitMsg     = user32.NewProc("PostQuitMessage")
	procGetModuleH      = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmApp           = 0x8000
	wmTrayCallback  = wmApp + 1
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmDestroy       = 0x0002
	hwndMessage     = ^uintptr(2) // HWND_MESSAGE, (HWND)-3
	cwUseDefault    = 0x80000000

	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmRightBtn    = 0x0002
	tpmReturnCmd   = 0x0100
	tpmBottomAlign = 0x0020

	menuOpen    = 1
	menuStartup = 2
	menuServer  = 3
	menuOnTop   = 4
	menuPerms   = 6
	menuQuit    = 5

	mfChecked = 0x00000008
	mfGrayed  = 0x00000001

	wmNull = 0x0000
)

type point struct{ X, Y int32 }

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// TrayWindow is the hidden window the notification icon reports to.
// MenuState is what the menu should show when it is opened. Read fresh each
// time rather than remembered, so a setting changed elsewhere is not stale
// here.
type MenuState struct {
	Server       string
	StartAtLogin bool
	AlwaysOnTop  bool
}

type TrayWindow struct {
	hwnd    uintptr
	onClick func()
	onQuit  func()

	// state reports what to show; act performs a chosen item.
	state func() MenuState
	act   func(id uintptr)
}

// NewTrayWindow creates it and starts pumping its messages.
//
// The message loop is locked to an OS thread: a window belongs to the thread
// that created it, and Go will otherwise move the goroutine somewhere the
// messages are not.
func NewTrayWindow(onClick, onQuit func(), state func() MenuState, act func(uintptr)) (*TrayWindow, error) {
	t := &TrayWindow{onClick: onClick, onQuit: onQuit, state: state, act: act}
	ready := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		className, err := syscall.UTF16PtrFromString("AetherTrayWindow")
		if err != nil {
			ready <- err
			return
		}
		inst, _, _ := procGetModuleH.Call(0)

		wc := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:   syscall.NewCallback(t.wndProc),
			Instance:  inst,
			ClassName: className,
		}
		if r, _, e := procRegisterClass.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			ready <- e
			return
		}

		hwnd, _, e := procCreateWindow.Call(
			0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
			0, 0, 0, 0, 0, hwndMessage, 0, inst, 0)
		if hwnd == 0 {
			ready <- e
			return
		}
		t.hwnd = hwnd
		ready <- nil

		var m msg
		for {
			r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()

	if err := <-ready; err != nil {
		return nil, err
	}
	return t, nil
}

func (t *TrayWindow) wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTrayCallback:
		// The low word of lParam is the mouse message. Left opens, right asks
		// what you want -- which is what everybody tries on a tray icon, and
		// an icon that ignores the right button reads as unfinished.
		switch uint32(lParam) & 0xffff {
		case wmLButtonUp, wmLButtonDblClk, ninBalloonUserClick:
			if t.onClick != nil {
				t.onClick()
			}
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	case wmDestroy:
		procPostQuitMsg.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

// showMenu puts the context menu under the cursor.
//
// Two pieces of protocol that look superstitious and are not. The owner window
// must be brought to the foreground first or the menu will not close when the
// person clicks away from it; and a null message must be posted afterwards, or
// the first click after the menu closes is swallowed. Both are documented
// requirements of TrackPopupMenu and both are wrong in most of the code that
// calls it.
func (t *TrayWindow) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	add := func(flags, id uintptr, label string) {
		p, err := syscall.UTF16PtrFromString(label)
		if err != nil {
			return
		}
		procAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(p)))
	}
	var st MenuState
	if t.state != nil {
		st = t.state()
	}
	check := func(on bool) uintptr {
		if on {
			return mfString | mfChecked
		}
		return mfString
	}

	add(mfString, menuOpen, "Open "+appName)
	add(mfSeparator, 0, "")
	// The server this copy is pointed at, shown rather than hidden in a file.
	// "Which server am I on?" is the first question when something looks
	// wrong, and an app that cannot answer it sends somebody to a support
	// queue to find out.
	if st.Server != "" {
		add(mfString|mfGrayed, menuServer, st.Server)
	}
	add(check(st.AlwaysOnTop), menuOnTop, "Keep window on top")
	add(check(st.StartAtLogin), menuStartup, "Start with Windows")
	add(mfString, menuPerms, "Permissions...")
	add(mfSeparator, 0, "")
	add(mfString, menuQuit, "Quit "+appName)

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	procSetForeground.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmRightBtn|tpmReturnCmd|tpmBottomAlign,
		uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	procPostMessageW.Call(t.hwnd, wmNull, 0, 0)

	switch cmd {
	case menuOpen:
		if t.onClick != nil {
			t.onClick()
		}
	case menuQuit:
		if t.onQuit != nil {
			t.onQuit()
		}
	case menuStartup, menuOnTop, menuPerms:
		if t.act != nil {
			t.act(cmd)
		}
	}
}

// HWND is the window the notification icon should report to.
func (t *TrayWindow) HWND() uintptr {
	if t == nil {
		return 0
	}
	return t.hwnd
}

// Close tears the hidden window down.
func (t *TrayWindow) Close() {
	if t == nil || t.hwnd == 0 {
		return
	}
	procDestroyWindow.Call(t.hwnd)
	t.hwnd = 0
}
