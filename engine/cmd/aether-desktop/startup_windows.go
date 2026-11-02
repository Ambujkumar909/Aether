//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Start with Windows, and keep the window on top.
//
// Both are settings a communications client is expected to have, and both are
// one Windows call away -- but only one of them belongs in a file we own.
// "Start at login" is a registry entry the shell reads, so the registry is the
// truth and our settings file would only be a stale copy of it; it is read
// back rather than remembered.

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKey = advapi32.NewProc("RegOpenKeyExW")
	procRegSetVal  = advapi32.NewProc("RegSetValueExW")
	procRegQueryV  = advapi32.NewProc("RegQueryValueExW")
	procRegDelVal  = advapi32.NewProc("RegDeleteValueW")
	procRegClose   = advapi32.NewProc("RegCloseKey")
)

const (
	hkeyCurrentUser = 0x80000001
	keyRead         = 0x20019
	keyWrite        = 0x20006
	regSZ           = 1
)

func openRunKey(access uintptr) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(runKeyPath)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	r, _, e := procRegOpenKey.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(p)),
		0, access, uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return 0, e
	}
	return h, nil
}

// StartsAtLogin reads the registry rather than a remembered flag, so a value
// somebody removed by hand is reported honestly.
func StartsAtLogin() bool {
	h, err := openRunKey(keyRead)
	if err != nil {
		return false
	}
	defer procRegClose.Call(uintptr(h))

	name, err := syscall.UTF16PtrFromString(appName)
	if err != nil {
		return false
	}
	var kind, size uint32
	r, _, _ := procRegQueryV.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0,
		uintptr(unsafe.Pointer(&kind)), 0, uintptr(unsafe.Pointer(&size)))
	return r == 0 && size > 0
}

// SetStartAtLogin adds or removes the entry.
func SetStartAtLogin(on bool) error {
	h, err := openRunKey(keyWrite | keyRead)
	if err != nil {
		return err
	}
	defer procRegClose.Call(uintptr(h))

	name, err := syscall.UTF16PtrFromString(appName)
	if err != nil {
		return err
	}
	if !on {
		procRegDelVal.Call(uintptr(h), uintptr(unsafe.Pointer(name)))
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Quoted, because a path with a space in it -- which is every path under
	// Program Files -- is otherwise read as a command and its arguments.
	value := `"` + exe + `"`
	v, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	r, _, e := procRegSetVal.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, regSZ,
		uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2))
	if r != 0 {
		return e
	}
	return nil
}

const (
	hwndTopmost   = ^uintptr(0) // (HWND)-1
	hwndNoTopmost = ^uintptr(1) // (HWND)-2
	swpNoMove     = 0x0002
	swpNoSize     = 0x0001
	swpNoActivate = 0x0010
)

// SetAlwaysOnTop pins the window above other windows, or releases it.
//
// Teams calls this "pin the window on top", and it exists for one situation:
// following a meeting while working in something else. A floating window that
// cannot be pinned means alt-tabbing every few seconds.
func SetAlwaysOnTop(hwnd uintptr, on bool) {
	if hwnd == 0 {
		return
	}
	after := hwndNoTopmost
	if on {
		after = hwndTopmost
	}
	procSetWindowPos.Call(hwnd, after, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoActivate)
}
