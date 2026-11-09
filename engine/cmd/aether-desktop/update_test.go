package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionsCompareNumerically(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0}, {"1.10.0", "1.9.3", 1}, {"1.9.3", "1.10.0", -1}, {"v2.0", "1.99", 1},
		{"1.0.0", "1.0.0.1", -1}, {"1.0.0", "1.0.0-rc1", 1}, {"0.0.1", "0.0.0", 1}, {"dev", "1.0", -1},
		{"2026.1109.1735", "2026.1108.2359", 1}, {"2026.1109.1735", "2026.1109.1735", 0}, // the image's build-time versions
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	m := Manifest{Version: "1.2.4"}
	if !m.newerThan("1.2.3") || m.newerThan("1.2.4") || m.newerThan("1.3.0") || m.newerThan("dev") {
		t.Fatal("newerThan is wrong, or a dev build would update")
	}
}

func TestTheManifestIsCheckedBeforeAnythingIsFetched(t *testing.T) {
	good := Manifest{Version: "1.0.0", File: "aether-desktop.exe", SHA256: strings.Repeat("ab", 32), Size: 1000}
	if err := good.check(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Manifest{
		"path in file":  {Version: "1", File: "../aether-desktop.exe", SHA256: good.SHA256, Size: 1},
		"not an exe":    {Version: "1", File: "aether-desktop.dll", SHA256: good.SHA256, Size: 1},
		"dotfile":       {Version: "1", File: ".exe", SHA256: good.SHA256, Size: 1},
		"short hash":    {Version: "1", File: "a.exe", SHA256: "abcd", Size: 1},
		"not hex":       {Version: "1", File: "a.exe", SHA256: strings.Repeat("zz", 32), Size: 1},
		"too big":       {Version: "1", File: "a.exe", SHA256: good.SHA256, Size: updateMaxBytes + 1},
		"no size":       {Version: "1", File: "a.exe", SHA256: good.SHA256, Size: 0},
		"no version":    {File: "a.exe", SHA256: good.SHA256, Size: 1},
		"version novel": {Version: strings.Repeat("9", 65), File: "a.exe", SHA256: good.SHA256, Size: 1},
	} {
		if err := bad.check(); !errors.Is(err, errManifest) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// A server with an app: the manifest and the file, with knobs to lie.
func appServer(t *testing.T, body []byte, lie func(m *Manifest)) (*httptest.Server, Manifest) {
	t.Helper()
	sum := sha256.Sum256(body)
	m := Manifest{Version: "2.0.0", File: "aether-desktop.exe", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	if lie != nil {
		lie(&m)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/desktop/latest.json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m) })
	mux.HandleFunc("/desktop/aether-desktop.exe", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, m
}

func TestADownloadIsKeptOnlyWhenHashAndSizeMatch(t *testing.T) {
	body := append([]byte("MZ"), []byte(strings.Repeat("program", 100))...)
	ctx := context.Background()
	srv, _ := appServer(t, body, nil)
	m, err := fetchManifest(ctx, srv.Client(), srv.URL)
	if err != nil || m == nil || m.Version != "2.0.0" {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	dest := filepath.Join(t.TempDir(), "aether-desktop.exe.new")
	if err := downloadUpdate(ctx, srv.Client(), srv.URL, m, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(body) {
		t.Fatal("the downloaded file differs")
	}
	// A server whose file does not match what it announced: nothing is kept.
	for name, lie := range map[string]func(*Manifest){
		"wrong hash": func(m *Manifest) { m.SHA256 = strings.Repeat("00", 32) },
		"wrong size": func(m *Manifest) { m.Size = int64(len(body)) - 1 },
	} {
		srv2, _ := appServer(t, body, lie)
		m2, err := fetchManifest(ctx, srv2.Client(), srv2.URL)
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(t.TempDir(), "x.exe.new")
		if err := downloadUpdate(ctx, srv2.Client(), srv2.URL, m2, d); err == nil {
			t.Errorf("%s: the download was kept", name)
		}
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s: a mismatched file was left behind", name)
		}
		if _, err := os.Stat(d + ".part"); err == nil {
			t.Errorf("%s: a partial file was left behind", name)
		}
	}
	// A server with no app at all is not an error.
	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	if m, err := fetchManifest(ctx, none.Client(), none.URL); err != nil || m != nil {
		t.Fatalf("no app: %+v %v", m, err)
	}
}

func TestThePendingUpdateIsSwappedInAndABadFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aether-desktop.exe")
	os.WriteFile(exe, []byte("MZ old"), 0o755)
	if applied, err := applyPendingUpdate(exe); applied || err != nil {
		t.Fatalf("nothing pending: %v %v", applied, err)
	}
	os.WriteFile(exe+updateNewSuffix, []byte("MZ new"), 0o755)
	applied, err := applyPendingUpdate(exe)
	if !applied || err != nil {
		t.Fatalf("apply: %v %v", applied, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "MZ new" {
		t.Fatalf("exe is %q", b)
	}
	if b, _ := os.ReadFile(exe + updateOldSuffix); string(b) != "MZ old" {
		t.Fatal("the old program was not kept for the undo")
	}
	cleanOldUpdate(exe)
	if _, err := os.Stat(exe + updateOldSuffix); err == nil {
		t.Fatal(".old not cleaned")
	}
	// A pending file that is not a program is removed, and nothing changes.
	os.WriteFile(exe+updateNewSuffix, []byte("<html>not a program"), 0o644)
	if applied, err := applyPendingUpdate(exe); applied || err == nil {
		t.Fatalf("a non-program was applied: %v %v", applied, err)
	}
	if _, err := os.Stat(exe + updateNewSuffix); err == nil {
		t.Fatal("the bad pending file was kept")
	}
	if b, _ := os.ReadFile(exe); string(b) != "MZ new" {
		t.Fatal("the program changed")
	}
}
