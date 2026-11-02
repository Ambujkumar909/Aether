//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Getting the person's attention when the window is not where they are
// looking: a Windows notification, a badge on the taskbar and tray icons,
// and a flashing taskbar button for a call. The client decides *when* (it
// knows what is unread and what is a call); the shell owns *how*, because
// only a native process can draw on the taskbar.
//
// Notifications go through the tray icon (NIF_INFO). On Windows 10 and 11
// the shell renders those as ordinary toast notifications in the corner and
// the notification centre, in the app's name with the app's icon, and a
// click on one comes back as NIN_BALLOONUSERCLICK, which restores the
// window. It needs no packaging, no AppUserModelID registration and no
// WinRT, which is why it is the mechanism here.
//
// The badge is two icons drawn at start-up time, not shipped: the taskbar
// overlay (ITaskbarList3::SetOverlayIcon, a red disc with the count) and a
// copy of the tray icon with the same disc in its corner. Both are painted
// pixel by pixel into a 32-bit DIB, so alpha is exact and nothing depends on
// GDI text rendering, which has no alpha.

var (
	gdi32                   = syscall.NewLazyDLL("gdi32.dll")
	ole32                   = syscall.NewLazyDLL("ole32.dll")
	procCreateDIBSection    = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap        = gdi32.NewProc("CreateBitmap")
	procCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	procSelectObject        = gdi32.NewProc("SelectObject")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procDeleteDC            = gdi32.NewProc("DeleteDC")
	procDrawIconEx          = user32.NewProc("DrawIconEx")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procFlashWindowEx       = user32.NewProc("FlashWindowEx")
	procCoInitializeEx      = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance    = ole32.NewProc("CoCreateInstance")
	clsidTaskbarList        = guid{0x56FDF344, 0xFD6D, 0x11d0, [8]byte{0x95, 0x8A, 0x00, 0x60, 0x97, 0xC9, 0xA0, 0x90}}
	iidTaskbarList3         = guid{0xEA1AFB91, 0x9E28, 0x4B86, [8]byte{0x90, 0xE9, 0x9E, 0x9F, 0x8A, 0x5E, 0xEF, 0xAF}}
	taskbar                 uintptr // ITaskbarList3, once created
	badgeOverlay, badgeTray uintptr // the icons currently installed, to destroy on change
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

const (
	niMModify           = 0x00000001
	nifInfo             = 0x00000010
	niifUser            = 0x00000004
	niifLargeIcon       = 0x00000020
	niifRespectQuiet    = 0x00000080
	ninBalloonUserClick = 0x0405
	diNormal            = 0x0003
	biRGB               = 0
	dibRGBColors        = 0
	flashwAll           = 0x00000003
	flashwTimerNoFG     = 0x0000000C
	flashwStop          = 0
	coinitApartment     = 0x2
	clsctxInprocServer  = 0x1
	badgeSize           = 16
)

// --- notifications -------------------------------------------------------

// Notify shows a Windows notification from the tray icon. Title and body are
// clipped to what NOTIFYICONDATA holds (63 and 255 UTF-16 units).
func (t *Tray) Notify(title, body string) {
	if t == nil {
		return
	}
	d := t.data
	d.UFlags = nifInfo
	d.DwInfoFlags = niifUser | niifLargeIcon | niifRespectQuiet
	d.HBalloonIcon = t.big
	d.SzInfo = [256]uint16{}
	d.SzInfoTitle = [64]uint16{}
	copy(d.SzInfoTitle[:63], utf16Of(title))
	copy(d.SzInfo[:255], utf16Of(body))
	procShellNotify.Call(niMModify, uintptr(unsafe.Pointer(&d)))
}

func utf16Of(s string) []uint16 {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return nil
	}
	return u[:len(u)-1] // without the terminator; the array is zeroed
}

// --- badge ---------------------------------------------------------------

// SetBadge puts the unread count on the taskbar button and the tray icon,
// or clears both for zero.
func (t *Tray) SetBadge(hwnd uintptr, n int) {
	if badgeOverlay != 0 {
		procDestroyIcon.Call(badgeOverlay)
		badgeOverlay = 0
	}
	if badgeTray != 0 {
		procDestroyIcon.Call(badgeTray)
		badgeTray = 0
	}
	if n <= 0 {
		setOverlay(hwnd, 0, "")
		if t != nil {
			t.setIcon(t.icon)
		}
		return
	}
	badgeOverlay = iconFromPixels(paintBadgePixels(nil, badgeSize, n))
	setOverlay(hwnd, badgeOverlay, badgeText(n)+" unread")
	if t != nil {
		if base := iconPixels(t.icon, badgeSize); base != nil {
			badgeTray = iconFromPixels(paintBadgePixels(base, badgeSize, n))
			if badgeTray != 0 {
				t.setIcon(badgeTray)
			}
		}
	}
}

func (t *Tray) setIcon(h uintptr) {
	d := t.data
	d.UFlags = nifIcon
	d.HIcon = h
	procShellNotify.Call(niMModify, uintptr(unsafe.Pointer(&d)))
}

// badgeText is what the disc shows: the count to nine, then a dot (drawn
// as an empty string), because two digits do not fit in sixteen pixels and
// "many" is what a person needs to know.
func badgeText(n int) string {
	if n <= 0 {
		return ""
	}
	if n > 9 {
		return "9+"
	}
	return string(rune('0' + n))
}

// A 3x5 digit font, one bit per pixel, rows top to bottom.
var digitFont = [10][5]uint8{
	{0b111, 0b101, 0b101, 0b101, 0b111}, // 0
	{0b010, 0b110, 0b010, 0b010, 0b111}, // 1
	{0b111, 0b001, 0b111, 0b100, 0b111}, // 2
	{0b111, 0b001, 0b111, 0b001, 0b111}, // 3
	{0b101, 0b101, 0b111, 0b001, 0b001}, // 4
	{0b111, 0b100, 0b111, 0b001, 0b111}, // 5
	{0b111, 0b100, 0b111, 0b101, 0b111}, // 6
	{0b111, 0b001, 0b001, 0b001, 0b001}, // 7
	{0b111, 0b101, 0b111, 0b101, 0b111}, // 8
	{0b111, 0b101, 0b111, 0b001, 0b111}, // 9
}

// paintBadgePixels draws the badge into a size x size premultiplied BGRA
// buffer. With base nil the whole square is the disc (the taskbar overlay);
// with a base icon's pixels the disc goes in the bottom-right corner, over
// the icon (the tray). Pure, so it can be tested with numbers.
func paintBadgePixels(base []byte, size, n int) []byte {
	px := make([]byte, size*size*4)
	copy(px, base)
	var cx, cy, r float64
	if base == nil {
		cx, cy, r = float64(size)/2, float64(size)/2, float64(size)/2
	} else {
		r = float64(size) * 0.34
		cx, cy = float64(size)-r, float64(size)-r
	}
	set := func(x, y int, b, g, rr byte) {
		if x < 0 || y < 0 || x >= size || y >= size {
			return
		}
		i := (y*size + x) * 4
		px[i], px[i+1], px[i+2], px[i+3] = b, g, rr, 0xff
	}
	// The disc, with a one-pixel lighter rim on the tray version so it reads
	// against the mark.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				set(x, y, 0x3c, 0x3c, 0xe0) // red, BGR
			}
		}
	}
	// The digit, white, only when the disc is the whole square.
	if base == nil && n >= 1 && n <= 9 {
		scale := 2
		if size < 16 {
			scale = 1
		}
		gw, gh := 3*scale, 5*scale
		ox, oy := (size-gw)/2, (size-gh)/2
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if digitFont[n][row]&(1<<(2-col)) == 0 {
					continue
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						set(ox+col*scale+sx, oy+row*scale+sy, 0xff, 0xff, 0xff)
					}
				}
			}
		}
	}
	return px
}

// --- GDI plumbing ----------------------------------------------------------

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

// dib32 makes a top-down 32-bit DIB section and returns it with its pixels.
func dib32(size int) (uintptr, []byte) {
	hdr := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32, Compression: biRGB}
	var bits uintptr
	h, _, _ := procCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&hdr)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if h == 0 || bits == 0 {
		return 0, nil
	}
	return h, unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&bits))), size*size*4)
}

// iconPixels renders an icon into a buffer, alpha intact.
func iconPixels(icon uintptr, size int) []byte {
	if icon == 0 {
		return nil
	}
	dib, px := dib32(size)
	if dib == 0 {
		return nil
	}
	defer procDeleteObject.Call(dib)
	dc, _, _ := procCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil
	}
	defer procDeleteDC.Call(dc)
	old, _, _ := procSelectObject.Call(dc, dib)
	procDrawIconEx.Call(dc, 0, 0, icon, uintptr(size), uintptr(size), 0, 0, diNormal)
	procSelectObject.Call(dc, old)
	out := make([]byte, len(px))
	copy(out, px)
	return out
}

// iconFromPixels makes an HICON from premultiplied BGRA pixels.
func iconFromPixels(px []byte) uintptr {
	size := badgeSize
	dib, dst := dib32(size)
	if dib == 0 {
		return 0
	}
	defer procDeleteObject.Call(dib)
	copy(dst, px)
	mask, _, _ := procCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	if mask == 0 {
		return 0
	}
	defer procDeleteObject.Call(mask)
	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: dib}
	h, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return h
}

// --- taskbar overlay -------------------------------------------------------

func taskbarList() uintptr {
	if taskbar != 0 {
		return taskbar
	}
	procCoInitializeEx.Call(0, coinitApartment) // S_FALSE / RPC_E_CHANGED_MODE are fine
	var p uintptr
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidTaskbarList)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidTaskbarList3)), uintptr(unsafe.Pointer(&p)))
	if int32(hr) < 0 || p == 0 {
		return 0
	}
	// HrInit is slot 3 of the vtable (after IUnknown).
	syscall.SyscallN(vtableOf(p)[3], p)
	taskbar = p
	return p
}

// comObject is the first word of any COM object: its vtable pointer.
type comObject struct {
	vtbl *[21]uintptr
}

// vtableOf reads the vtable behind an interface pointer. Two dereferences:
// the pointer names the object, the object's first word names the table.
// (A first version read the table from the object itself and called into
// address zero.)
func vtableOf(p uintptr) *[21]uintptr {
	obj := (*comObject)(*(*unsafe.Pointer)(unsafe.Pointer(&p)))
	return obj.vtbl
}

// setOverlay is ITaskbarList3::SetOverlayIcon, vtable slot 18.
func setOverlay(hwnd, icon uintptr, desc string) {
	p := taskbarList()
	if p == 0 || hwnd == 0 {
		return
	}
	var d *uint16
	if desc != "" {
		d, _ = syscall.UTF16PtrFromString(desc)
	}
	syscall.SyscallN(vtableOf(p)[18], p, hwnd, icon, uintptr(unsafe.Pointer(d)))
}

// --- flashing ----------------------------------------------------------------

type flashWInfo struct {
	CbSize    uint32
	Hwnd      uintptr
	DwFlags   uint32
	UCount    uint32
	DwTimeout uint32
}

// FlashTaskbar flashes the taskbar button until the window comes to the
// front, the way an incoming call does everywhere else.
func FlashTaskbar(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	fi := flashWInfo{CbSize: uint32(unsafe.Sizeof(flashWInfo{})), Hwnd: hwnd, DwFlags: flashwAll | flashwTimerNoFG}
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

func StopFlashing(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	fi := flashWInfo{CbSize: uint32(unsafe.Sizeof(flashWInfo{})), Hwnd: hwnd, DwFlags: flashwStop}
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}
