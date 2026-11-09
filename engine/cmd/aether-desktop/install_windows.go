//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Installing without an installer.
//
// The executable is its own setup: run it with -install (or from a download
// folder: see main) and it copies itself to the per-user programs folder,
// puts a shortcut in the Start menu, registers under "Installed apps" with an
// uninstall command, and starts the installed copy. Per-user and nothing
// elevated: no administrator prompt, no Program Files, no service. -uninstall
// undoes all of it, settings included. The one piece of Windows plumbing is
// the shortcut, which is a COM object (IShellLink) and is driven directly.

const (
	installFolder = "Aether"
	exeName       = "aether-desktop.exe"
)

// Where the registration goes. A variable so a test can register under a
// key of its own instead of the real "Installed apps" list.
var uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Aether`

// installDir is %LOCALAPPDATA%\Programs\Aether.
func installDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("LOCALAPPDATA is not set")
	}
	return filepath.Join(base, "Programs", installFolder), nil
}

// startMenuShortcut is where the Start menu entry goes.
func startMenuShortcut() (string, error) {
	base := os.Getenv("APPDATA")
	if base == "" {
		return "", errors.New("APPDATA is not set")
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", "Aether.lnk"), nil
}

// installCommand handles -install and -uninstall before anything else runs.
// It reports whether it handled the arguments and the exit code to use.
func installCommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "-install", "--install", "/install":
		if err := install(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "Could not install:", err)
			return true, 1
		}
		return true, 0
	case "-uninstall", "--uninstall", "/uninstall":
		if err := uninstall(); err != nil {
			fmt.Fprintln(os.Stderr, "Could not uninstall:", err)
			return true, 1
		}
		return true, 0
	}
	return false, 0
}

// install copies this program into place, registers it, and starts it with
// whatever arguments followed -install (a -server, usually).
func install(rest []string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := installDir()
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, exeName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !strings.EqualFold(src, dst) {
		if err := copyFile(src, dst); err != nil {
			if isInUse(err) {
				return errors.New("Aether is running; quit it from the notification area and run the installer again")
			}
			return err
		}
	}
	if err := writeUninstallEntry(dst, dir); err != nil {
		return fmt.Errorf("registering the app: %w", err)
	}
	lnk, err := startMenuShortcut()
	if err != nil {
		return err
	}
	if err := createShortcut(lnk, dst, "Meetings, calls and chat"); err != nil {
		return fmt.Errorf("start menu shortcut: %w", err)
	}
	fmt.Printf("Installed %s to %s\n", appName, dir)
	cmd := exec.Command(dst, rest...)
	return cmd.Start()
}

// uninstall removes the registration, the shortcut, the settings and, last,
// the program folder -- which cannot be deleted while this very program runs
// from it, so that is left to a shell that waits for this process to end.
func uninstall() error {
	if err := SetStartAtLogin(false); err != nil && !errors.Is(err, registry.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "start at login:", err)
	}
	if lnk, err := startMenuShortcut(); err == nil {
		os.Remove(lnk)
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
	if cfg, err := configDir(); err == nil {
		os.RemoveAll(cfg)
	}
	dir, err := installDir()
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	if strings.EqualFold(filepath.Dir(self), dir) {
		// cmd.exe is part of Windows: wait a moment for this process to end,
		// then remove the folder.
		c := exec.Command("cmd.exe", "/C", "ping -n 3 127.0.0.1 >nul & rd /s /q \""+dir+"\"")
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := c.Start(); err != nil {
			return err
		}
	} else {
		os.RemoveAll(dir)
	}
	fmt.Printf("%s was removed.\n", appName)
	return nil
}

// writeUninstallEntry is what makes the app appear in Settings > Apps, with
// a working Uninstall.
func writeUninstallEntry(exe, dir string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	var size uint32
	if st, err := os.Stat(exe); err == nil {
		size = uint32(st.Size() / 1024)
	}
	for name, v := range map[string]string{
		"DisplayName":     appName,
		"DisplayVersion":  version,
		"Publisher":       appName,
		"InstallLocation": dir,
		"DisplayIcon":     exe + ",0",
		"UninstallString": `"` + exe + `" -uninstall`,
	} {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	for name, v := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": size} {
		if err := k.SetDWordValue(name, v); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".installing"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func isInUse(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == 32 || errno == 5) // sharing violation, access denied
}

// ---------------------------------------------------------------------------
// A Start menu shortcut, through IShellLinkW and IPersistFile.
// ---------------------------------------------------------------------------

var (
	clsidShellLink   = windows.GUID{Data1: 0x00021401, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIShellLinkW   = windows.GUID{Data1: 0x000214F9, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIPersistFile  = windows.GUID{Data1: 0x0000010b, Data2: 0, Data3: 0, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	clsctxInprocServ = uint32(1)
)

// vtable method indexes (IUnknown's three come first).
const (
	shellLinkSetDescription      = 7
	shellLinkSetWorkingDirectory = 9
	shellLinkSetIconLocation     = 17
	shellLinkSetPath             = 20
	persistFileSave              = 6
)

// comObj is a COM interface pointer: the first word points at the vtable.
type comObj struct{ vtbl *[32]uintptr }

func comCall(obj *comObj, index int, args ...uintptr) error {
	hr, _, _ := syscall.SyscallN(obj.vtbl[index], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	if int32(hr) < 0 {
		return fmt.Errorf("COM call %d failed: 0x%08x", index, uint32(hr))
	}
	return nil
}

func comRelease(obj *comObj) { comCall(obj, 2) }

func createShortcut(lnkPath, target, description string) error {
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
		// Already initialised on this thread is fine.
		var e windows.Errno
		if !errors.As(err, &e) || (uint32(e) != 0x80010106 && uint32(e) != 1) {
			return err
		}
	}
	defer windows.CoUninitialize()
	var link *comObj
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, uintptr(clsctxInprocServ),
		uintptr(unsafe.Pointer(&iidIShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if int32(hr) < 0 || link == nil {
		return fmt.Errorf("creating the shortcut object: 0x%08x", uint32(hr))
	}
	defer comRelease(link)
	utf16 := func(s string) uintptr {
		p, _ := syscall.UTF16PtrFromString(s)
		return uintptr(unsafe.Pointer(p))
	}
	if err := comCall(link, shellLinkSetPath, utf16(target)); err != nil {
		return err
	}
	if err := comCall(link, shellLinkSetWorkingDirectory, utf16(filepath.Dir(target))); err != nil {
		return err
	}
	if err := comCall(link, shellLinkSetDescription, utf16(description)); err != nil {
		return err
	}
	if err := comCall(link, shellLinkSetIconLocation, utf16(target), 0); err != nil {
		return err
	}
	var persist *comObj
	if err := comCall(link, 0, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&persist))); err != nil {
		return err
	}
	defer comRelease(persist)
	if err := os.MkdirAll(filepath.Dir(lnkPath), 0o700); err != nil {
		return err
	}
	return comCall(persist, persistFileSave, utf16(lnkPath), 1)
}
