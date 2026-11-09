package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// Updating from the server the app is pointed at.
//
// The server publishes /desktop/latest.json -- the version it shipped with,
// the file, its SHA-256 and size -- and the file beside it (the image builds
// both). The app asks once after start and then every few hours, over the
// same pinned TLS as everything else it says to the server, downloads a
// newer version next to itself as aether-desktop.exe.new, checks the hash
// and the size, and offers "Restart to update" in the tray. The running
// program cannot overwrite itself, so the swap is the supervisor's: the child
// exits with exitUpdateRestart, the parent renames .exe to .old and .new to
// .exe, and starts the child again. A .new that does not verify is deleted;
// a swap that fails is undone. No code signing: the trust is the server's
// TLS identity, the same trust the whole application rests on.

// Manifest is /desktop/latest.json.
type Manifest struct {
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

const (
	manifestPath      = "/desktop/latest.json"
	updateMaxBytes    = 64 << 20
	updateNewSuffix   = ".new"
	updateOldSuffix   = ".old"
	exitUpdateRestart = 3 // the child asking the supervisor to swap and restart
)

var errManifest = errors.New("update: the server's manifest is not usable")

// fetchManifest reads and checks the server's manifest.
func fetchManifest(ctx context.Context, client *http.Client, server string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+manifestPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil // this server offers no app
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: manifest answered %d", res.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&m); err != nil {
		return nil, errManifest
	}
	if err := m.check(); err != nil {
		return nil, err
	}
	return &m, nil
}

// check refuses a manifest that could make the app fetch or run the wrong
// thing: a file name with a path in it, a hash that is not one, a size
// outside the bound.
func (m *Manifest) check() error {
	switch {
	case m.Version == "" || len(m.Version) > 64:
		return fmt.Errorf("%w: version", errManifest)
	case m.File == "" || m.File != path.Base(m.File) || strings.HasPrefix(m.File, ".") || !strings.HasSuffix(strings.ToLower(m.File), ".exe"):
		return fmt.Errorf("%w: file name", errManifest)
	case len(m.SHA256) != 64 || !isHex(m.SHA256):
		return fmt.Errorf("%w: hash", errManifest)
	case m.Size <= 0 || m.Size > updateMaxBytes:
		return fmt.Errorf("%w: size", errManifest)
	}
	return nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// newerThan reports whether the manifest's version is newer than this
// build's. A dev build never updates.
func (m *Manifest) newerThan(current string) bool {
	return current != "dev" && compareVersions(m.Version, current) > 0
}

// downloadUpdate fetches the manifest's file to dest, writing through a
// hash; the file is kept only when size and hash match.
func downloadUpdate(ctx context.Context, client *http.Client, server string, m *Manifest, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/desktop/"+m.File, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download answered %d", res.StatusCode)
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, m.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	if n != m.Size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(m.SHA256) {
		os.Remove(part)
		return errors.New("update: the download does not match the manifest")
	}
	os.Remove(dest)
	return os.Rename(part, dest)
}

// applyPendingUpdate swaps exe+".new" into place, if there is one. Done by
// the supervisor before it starts the child, so a download finished in an
// earlier session applies at the next start. A swap that fails halfway is
// undone; a .new that is not a Windows program is deleted.
func applyPendingUpdate(exe string) (applied bool, err error) {
	next := exe + updateNewSuffix
	st, err := os.Stat(next)
	if err != nil {
		return false, nil
	}
	if !looksLikeProgram(next) || st.Size() == 0 {
		os.Remove(next)
		return false, errors.New("update: the pending file is not a program; removed")
	}
	old := exe + updateOldSuffix
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return false, err
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe) // back as it was
		return false, err
	}
	return true, nil
}

// cleanOldUpdate removes the previous version once the new one runs. The
// supervisor that applied the swap is still running from the .old image, and
// Windows will not delete a running program, so right after an update this
// fails quietly and succeeds on the launch after -- measured, not assumed.
func cleanOldUpdate(exe string) { os.Remove(exe + updateOldSuffix) }

// looksLikeProgram checks the DOS header every Windows executable starts with.
func looksLikeProgram(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	_, err = io.ReadFull(f, b[:])
	return err == nil && b[0] == 'M' && b[1] == 'Z'
}
