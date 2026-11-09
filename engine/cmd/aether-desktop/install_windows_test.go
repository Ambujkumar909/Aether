//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// The registration and the shortcut, written under a test key and a
// temporary folder: real Windows plumbing, nothing left on the machine.
func TestTheInstallerRegistersAndMakesAShortcut(t *testing.T) {
	saved := uninstallKey
	uninstallKey = `Software\AetherTest\Uninstall\Aether`
	t.Cleanup(func() {
		registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
		registry.DeleteKey(registry.CURRENT_USER, `Software\AetherTest\Uninstall`)
		registry.DeleteKey(registry.CURRENT_USER, `Software\AetherTest`)
		uninstallKey = saved
	})
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	if err := os.WriteFile(exe, []byte("MZ test program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeUninstallEntry(exe, dir); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, uninstallKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("DisplayName")
	unins, _, _ := k.GetStringValue("UninstallString")
	loc, _, _ := k.GetStringValue("InstallLocation")
	if name != appName || unins != `"`+exe+`" -uninstall` || loc != dir {
		t.Fatalf("registration: %q %q %q", name, unins, loc)
	}
	if v, _, err := k.GetIntegerValue("NoModify"); err != nil || v != 1 {
		t.Fatalf("NoModify %d %v", v, err)
	}

	lnk := filepath.Join(dir, "menu", "Aether.lnk")
	if err := createShortcut(lnk, exe, "test"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(lnk)
	if err != nil || st.Size() < 100 {
		t.Fatalf("shortcut: %v (%d bytes)", err, st.Size())
	}
	// A .lnk starts with its header size (0x4C) and the ShellLink CLSID.
	b, _ := os.ReadFile(lnk)
	if len(b) < 20 || b[0] != 0x4C || b[4] != 0x01 || b[5] != 0x14 || b[6] != 0x02 {
		t.Fatalf("not a shell link: % x", b[:8])
	}

	// copyFile replaces atomically and leaves no partial file.
	dst := filepath.Join(dir, "copy", exeName)
	os.MkdirAll(filepath.Dir(dst), 0o700)
	if err := copyFile(exe, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "MZ test program" {
		t.Fatal("copy differs")
	}
	if _, err := os.Stat(dst + ".installing"); err == nil {
		t.Fatal("partial file left")
	}
}

// A swap that fails halfway is undone. Windows will not rename a file another
// process holds open, which is the failure this guards against, so the test
// is Windows-only: on Linux the rename succeeds and nothing is there to undo.
func TestAFailedSwapIsUndone(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, exeName)
	os.WriteFile(exe, []byte("MZ new"), 0o755)
	// The .new is held open, so the second rename cannot happen.
	os.WriteFile(exe+updateNewSuffix, []byte("MZ newer"), 0o755)
	held, err := os.Open(exe + updateNewSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if applied, err := applyPendingUpdate(exe); applied || err == nil {
		t.Fatalf("a swap against a held file: %v %v", applied, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "MZ new" {
		t.Fatalf("the failed swap left %q in place", b)
	}
	if _, err := os.Stat(exe + updateOldSuffix); err == nil {
		t.Fatal("the undo left the .old behind")
	}
}
