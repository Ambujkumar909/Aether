package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheDesktopAreaServesOnlyItsOwnFilesAsDownloads(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "latest.json"), []byte(`{"version":"1.2.3"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "aether-desktop.exe"), []byte("MZ..."), 0o644)
	os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.txt"), []byte("x"), 0o644)
	mux := http.NewServeMux()
	mux.Handle("GET /desktop/{file}", desktopHandler(dir))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(p string) *http.Response {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get("/desktop/latest.json"); res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("manifest: %d %v", res.StatusCode, res.Header)
	}
	res := get("/desktop/aether-desktop.exe")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/octet-stream" ||
		res.Header.Get("Content-Disposition") != `attachment; filename="aether-desktop.exe"` || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("exe: %d %v", res.StatusCode, res.Header)
	}
	for _, p := range []string{"/desktop/.secret", "/desktop/..%2Foutside.txt", "/desktop/%2e%2e/outside.txt", "/desktop/nothere.exe", "/desktop/"} {
		if res := get(p); res.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, res.StatusCode)
		}
	}
}
