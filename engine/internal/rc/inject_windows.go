//go:build windows

package rc

import (
	"errors"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows input injection through SendInput.
//
// SendInput rather than mouse_event/keybd_event, which are deprecated, and
// rather than driver-level injection, which would need a signed driver and
// buys nothing here. SendInput enters the same queue as real hardware, so
// applications cannot tell the difference and none of the usual "this app
// ignores synthetic input" problems apply.
//
// What SendInput deliberately cannot do is worth stating, because it will
// come up as a bug report: it cannot send Ctrl+Alt+Del, and it cannot
// interact with the secure desktop (UAC prompts, the lock screen, the login
// screen). Those are guarded by the Secure Attention Sequence and are
// unreachable from any user-mode process by design. Reaching them needs a
// service running as SYSTEM in session 0 that can call into the input
// desktop, which is a separate component with a much larger attack surface
// and is not something to add casually to a meeting product.

var (
	user32       = windows.NewLazySystemDLL("user32.dll")
	procSendIn   = user32.NewProc("SendInput")
	procGetSysMe = user32.NewProc("GetSystemMetrics")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseeventfMove        = 0x0001
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfXDown       = 0x0080
	mouseeventfXUp         = 0x0100
	mouseeventfWheel       = 0x0800
	mouseeventfHWheel      = 0x1000
	mouseeventfVirtualDesk = 0x4000
	mouseeventfAbsolute    = 0x8000

	xbutton1 = 0x0001
	xbutton2 = 0x0002

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002
	keyeventfUnicode  = 0x0004

	wheelDelta = 120

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// Both variants are exactly 40 bytes on 64-bit Windows, which is what
// SendInput is told through cbSize. The union in the C definition is sized by
// MOUSEINPUT, so the keyboard form carries trailing padding to match; getting
// this wrong makes SendInput reject every call with no useful error.
type mouseInputRec struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extraInfo uintptr
}

type keyboardInputRec struct {
	typ       uint32
	_         uint32
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	_         uint32
	extraInfo uintptr
	_         [8]byte
}

// inputSize is the cbSize SendInput expects.
const inputSize = 40

var ErrInjectionRefused = errors.New("rc: the system refused the injected input")

// WindowsInjector injects into the current input desktop.
type WindowsInjector struct{}

// NewInjector returns the platform injector for this build.
func NewInjector() Injector { return &WindowsInjector{} }

func sendMouse(rec *mouseInputRec) error {
	rec.typ = inputMouse
	n, _, err := procSendIn.Call(1, uintptr(unsafe.Pointer(rec)), inputSize)
	if n != 1 {
		if err != nil && !errors.Is(err, windows.ERROR_SUCCESS) {
			return err
		}
		return ErrInjectionRefused
	}
	return nil
}

func sendKey(rec *keyboardInputRec) error {
	rec.typ = inputKeyboard
	n, _, err := procSendIn.Call(1, uintptr(unsafe.Pointer(rec)), inputSize)
	if n != 1 {
		if err != nil && !errors.Is(err, windows.ERROR_SUCCESS) {
			return err
		}
		return ErrInjectionRefused
	}
	return nil
}

// MouseMove takes coordinates normalised to CoordMax and maps them across the
// whole virtual desktop.
//
// MOUSEEVENTF_VIRTUALDESK matters on any multi-monitor machine. Without it,
// absolute coordinates are interpreted against the primary display only, so
// on a two-monitor setup the operator can reach exactly half the screen they
// are being shown and the cursor pins to the edge for the rest. With it, the
// normalised range covers every monitor, which is what the captured image
// already showed.
func (w *WindowsInjector) MouseMove(x, y uint16) error {
	rec := &mouseInputRec{
		dx:    int32(x),
		dy:    int32(y),
		flags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDesk,
	}
	return sendMouse(rec)
}

func (w *WindowsInjector) MouseButton(b Button, down bool) error {
	rec := &mouseInputRec{}
	switch b {
	case ButtonLeft:
		rec.flags = mouseeventfLeftUp
		if down {
			rec.flags = mouseeventfLeftDown
		}
	case ButtonRight:
		rec.flags = mouseeventfRightUp
		if down {
			rec.flags = mouseeventfRightDown
		}
	case ButtonMiddle:
		rec.flags = mouseeventfMiddleUp
		if down {
			rec.flags = mouseeventfMiddleDown
		}
	case ButtonX1, ButtonX2:
		rec.flags = mouseeventfXUp
		if down {
			rec.flags = mouseeventfXDown
		}
		rec.mouseData = xbutton1
		if b == ButtonX2 {
			rec.mouseData = xbutton2
		}
	default:
		return ErrBadButton
	}
	return sendMouse(rec)
}

// MouseWheel scrolls by notches. Vertical and horizontal are separate
// injections because a single MOUSEINPUT carries one wheel delta.
func (w *WindowsInjector) MouseWheel(dx, dy int16) error {
	if dy != 0 {
		rec := &mouseInputRec{
			flags:     mouseeventfWheel,
			mouseData: uint32(int32(dy) * wheelDelta),
		}
		if err := sendMouse(rec); err != nil {
			return err
		}
	}
	if dx != 0 {
		rec := &mouseInputRec{
			flags:     mouseeventfHWheel,
			mouseData: uint32(int32(dx) * wheelDelta),
		}
		if err := sendMouse(rec); err != nil {
			return err
		}
	}
	return nil
}

// extendedKeys need KEYEVENTF_EXTENDEDKEY or they arrive as their numeric
// keypad twins: the arrow keys become 2/4/6/8, Home becomes 7, Delete becomes
// the decimal point. This is the single most common bug in hand-rolled
// Windows input injection.
var extendedKeys = map[uint16]bool{
	0x21: true, // PRIOR (Page Up)
	0x22: true, // NEXT  (Page Down)
	0x23: true, // END
	0x24: true, // HOME
	0x25: true, // LEFT
	0x26: true, // UP
	0x27: true, // RIGHT
	0x28: true, // DOWN
	0x2D: true, // INSERT
	0x2E: true, // DELETE
	0x5B: true, // LWIN
	0x5C: true, // RWIN
	0x5D: true, // APPS
	0x6F: true, // DIVIDE
	0x90: true, // NUMLOCK
	0xA3: true, // RCONTROL
	0xA5: true, // RMENU (right Alt / AltGr)
	0x0D: false,
}

func (w *WindowsInjector) Key(vk uint16, down bool) error {
	rec := &keyboardInputRec{vk: vk}
	if extendedKeys[vk] {
		rec.flags |= keyeventfExtended
	}
	if !down {
		rec.flags |= keyeventfKeyUp
	}
	return sendKey(rec)
}

// Text types a literal string as Unicode, bypassing the keyboard layout
// entirely.
//
// This is why the protocol carries text as well as virtual key codes. A
// scancode means "the key in this physical position", which produces a
// different character on a different layout: an operator on a Danish keyboard
// sending scancodes to a US-layout machine types the wrong letters, and no
// amount of care at the sending end fixes it. KEYEVENTF_UNICODE says "this
// character" and is layout-independent by construction.
//
// Characters outside the Basic Multilingual Plane are sent as their two UTF-16
// surrogates, which is exactly what Windows expects; emoji arrive correctly
// without special handling.
func (w *WindowsInjector) Text(s string) error {
	for _, r := range s {
		for _, unit := range utf16.Encode([]rune{r}) {
			down := &keyboardInputRec{scan: unit, flags: keyeventfUnicode}
			if err := sendKey(down); err != nil {
				return err
			}
			up := &keyboardInputRec{scan: unit, flags: keyeventfUnicode | keyeventfKeyUp}
			if err := sendKey(up); err != nil {
				return err
			}
		}
	}
	return nil
}

// VirtualScreen reports the bounding box of every monitor, which is the
// coordinate space MouseMove normalises against.
func VirtualScreen() (x, y, w, h int) {
	get := func(i uintptr) int {
		v, _, _ := procGetSysMe.Call(i)
		return int(int32(v))
	}
	return get(smXVirtualScreen), get(smYVirtualScreen),
		get(smCXVirtualScreen), get(smCYVirtualScreen)
}
