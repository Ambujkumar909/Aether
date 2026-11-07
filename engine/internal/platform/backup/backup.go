// Package backup takes consistent snapshots of a live deployment and restores
// them.
//
// The database is copied with SQLite's VACUUM INTO on a connection of its own,
// opened read-only: WAL lets it read a single consistent view while the server
// keeps writing on its one connection, so a backup never stalls the product.
// The copy is checked with integrity_check before it is named a backup, so a
// truncated or corrupt file is never mistaken for one.
//
// Uploaded files are immutable blobs, so they are mirrored: new blobs are
// copied, and blobs the server deleted are deleted from the mirror too -- a
// file somebody deleted does not live on in a backup. Restoring an older
// snapshot can therefore lack a file deleted since, which shows as
// unavailable rather than wrong.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Options names what to back up and where.
type Options struct {
	DBPath   string // the live database
	FilesDir string // uploaded files; empty skips them
	Dir      string // where snapshots and the file mirror go
	Keep     int    // database snapshots kept, newest first; <1 keeps 1
	Now      func() time.Time
}

// Result says what one run produced.
type Result struct {
	File        string // the snapshot's path
	Bytes       int64
	FilesCopied int
	FilesPruned int
	Removed     []string // snapshots deleted by retention
}

const prefix, suffix = "aether-", ".db"

// Run takes one snapshot, mirrors the files and applies retention.
func Run(ctx context.Context, o Options) (Result, error) {
	var res Result
	if o.DBPath == "" || o.Dir == "" {
		return res, errors.New("backup: database and destination are required")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return res, fmt.Errorf("backup: %w", err)
	}
	name := prefix + now().UTC().Format("20060102T150405.000Z") + suffix
	final := filepath.Join(o.Dir, name)
	part := final + ".part"
	_ = os.Remove(part)

	src, err := sql.Open("sqlite", "file:"+filepath.ToSlash(o.DBPath)+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return res, fmt.Errorf("backup: open: %w", err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, part); err != nil {
		_ = os.Remove(part)
		return res, fmt.Errorf("backup: snapshot: %w", err)
	}
	if err := Verify(ctx, part); err != nil {
		_ = os.Remove(part)
		return res, err
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		return res, fmt.Errorf("backup: %w", err)
	}
	if st, err := os.Stat(final); err == nil {
		res.Bytes = st.Size()
	}
	res.File = final

	if o.FilesDir != "" {
		res.FilesCopied, res.FilesPruned, err = mirror(o.FilesDir, filepath.Join(o.Dir, "files"))
		if err != nil {
			return res, fmt.Errorf("backup: files: %w", err)
		}
	}
	res.Removed, err = prune(o.Dir, o.Keep)
	return res, err
}

// Verify checks a database file is complete and consistent.
func Verify(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&got); err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if got != "ok" {
		return fmt.Errorf("verify %s: integrity_check: %s", path, got)
	}
	return nil
}

// Snapshots lists the snapshots in dir, newest first.
func Snapshots(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	// The timestamp in the name sorts lexically.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

func prune(dir string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	snaps, err := Snapshots(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, p := range snaps[min(keep, len(snaps)):] {
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}

// mirror makes dst hold exactly the blobs src holds (ignoring partial uploads).
func mirror(src, dst string) (copied, pruned int, err error) {
	have := map[string]bool{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == src {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".part-") {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		have[rel] = true
		to := filepath.Join(dst, rel)
		if st, err := os.Stat(to); err == nil {
			if from, err := d.Info(); err == nil && from.Size() == st.Size() {
				return nil // immutable: same name and size is the same blob
			}
		}
		if err := copyFile(p, to); err != nil {
			return err
		}
		copied++
		return nil
	})
	if err != nil {
		return copied, 0, err
	}
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if !have[rel] {
			if err := os.Remove(p); err != nil {
				return err
			}
			pruned++
		}
		return nil
	})
	return copied, pruned, err
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(to), ".mirror-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), to)
}

// Restore replaces the database at dbPath with a verified snapshot. The
// server must be stopped: a running server holds the old file open and would
// keep writing to it. The previous database is kept beside it as .before-restore.
func Restore(ctx context.Context, snapshot, dbPath string) error {
	if err := Verify(ctx, snapshot); err != nil {
		return fmt.Errorf("restore: the snapshot is not usable: %w", err)
	}
	tmp := dbPath + ".restoring"
	if err := copyFile(snapshot, tmp); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := Verify(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore: the copy is not usable: %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		_ = os.Remove(dbPath + ".before-restore")
		if err := os.Rename(dbPath, dbPath+".before-restore"); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("restore: %w", err)
		}
	}
	// The old database's write-ahead log belongs to the old file; replayed
	// over the snapshot it would corrupt it.
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + s); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return os.Rename(tmp, dbPath)
}
