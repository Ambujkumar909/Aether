package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
)

// live opens a database the way the server does (WAL, one connection) with a
// table the tests write to.
func live(t *testing.T) (*database.DB, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aether.db")
	db, err := database.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE bk_rows (n INTEGER PRIMARY KEY, body TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db, p
}

func rows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM bk_rows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func clock(start time.Time) func() time.Time {
	var i atomic.Int64
	return func() time.Time { return start.Add(time.Duration(i.Add(1)) * time.Second) }
}

// A backup taken while the server writes is a complete, consistent database,
// and the writer is never stopped for it.
func TestSnapshotsOfALiveDatabaseAreConsistentAndDoNotStopWrites(t *testing.T) {
	db, path := live(t)
	ctx := context.Background()
	body := strings.Repeat("x", 512)
	for i := 0; i < 20000; i++ { // ~10 MB, so a snapshot takes real time
		if _, err := db.Exec(`INSERT INTO bk_rows (body) VALUES (?)`, body); err != nil {
			t.Fatal(err)
		}
	}
	var written atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Exec(`INSERT INTO bk_rows (body) VALUES (?)`, "live"); err == nil {
				written.Add(1)
			}
		}
	}()
	dir := filepath.Join(t.TempDir(), "backups")
	prev := 0
	for i := 0; i < 3; i++ {
		before := written.Load()
		res, err := Run(ctx, Options{DBPath: path, Dir: dir, Keep: 10, Now: clock(time.Unix(1_700_000_000+int64(i)*10, 0))})
		if err != nil {
			t.Fatal(err)
		}
		during := written.Load() - before
		if during == 0 {
			t.Fatalf("snapshot %d: the writer committed nothing while the backup ran", i)
		}
		if err := Verify(ctx, res.File); err != nil {
			t.Fatal(err)
		}
		n := rows(t, res.File)
		if n < 20000 || n < prev {
			t.Fatalf("snapshot %d has %d rows (previous %d)", i, n, prev)
		}
		prev = n
	}
	close(stop)
	wg.Wait()
}

func TestRetentionKeepsTheNewest(t *testing.T) {
	_, path := live(t)
	dir := filepath.Join(t.TempDir(), "backups")
	now := clock(time.Unix(1_700_000_000, 0))
	var made []string
	for i := 0; i < 5; i++ {
		res, err := Run(context.Background(), Options{DBPath: path, Dir: dir, Keep: 2, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, res.File)
	}
	left, _ := Snapshots(dir)
	if len(left) != 2 || left[0] != made[4] || left[1] != made[3] {
		t.Fatalf("kept %v, want the two newest", left)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Fatalf("left behind %d entries (a .part file?)", len(ents))
	}
}

// Files are mirrored: new blobs copied, deleted blobs removed, partial uploads
// never copied, unchanged blobs not copied again.
func TestFilesAreMirroredIncludingDeletions(t *testing.T) {
	_, path := live(t)
	files := filepath.Join(t.TempDir(), "files")
	put := func(key, body string) {
		p := filepath.Join(files, key[:2], key)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("aaaa1111", "one")
	put("bbbb2222", "two")
	// A partial upload, named the way DiskStore names one (CreateTemp ".part-*").
	if err := os.WriteFile(filepath.Join(files, "bb", ".part-1234"), []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	run := func() Result {
		res, err := Run(context.Background(), Options{DBPath: path, FilesDir: files, Dir: dir, Keep: 3, Now: clock(time.Now())})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run(); r.FilesCopied != 2 {
		t.Fatalf("first run copied %d, want 2", r.FilesCopied)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "files", "aa", "aaaa1111")); err != nil || string(b) != "one" {
		t.Fatalf("mirror content %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "bb", ".part-1234")); err == nil {
		t.Fatal("a partial upload was backed up")
	}
	if r := run(); r.FilesCopied != 0 || r.FilesPruned != 0 {
		t.Fatalf("an unchanged tree was copied again: %+v", r)
	}
	if err := os.Remove(filepath.Join(files, "bb", "bbbb2222")); err != nil {
		t.Fatal(err)
	}
	put("cccc3333", "three")
	if r := run(); r.FilesCopied != 1 || r.FilesPruned != 1 {
		t.Fatalf("after one delete and one add: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "bb", "bbbb2222")); err == nil {
		t.Fatal("a deleted file lives on in the backup")
	}
}

// The restore drill: back up, change, restore, and the server opens exactly
// the snapshot; the replaced database is kept; a corrupt snapshot is refused.
func TestRestoreBringsBackExactlyTheSnapshot(t *testing.T) {
	db, path := live(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		db.Exec(`INSERT INTO bk_rows (body) VALUES (?)`, fmt.Sprint("before ", i))
	}
	res, err := Run(ctx, Options{DBPath: path, Dir: filepath.Join(t.TempDir(), "b"), Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		db.Exec(`INSERT INTO bk_rows (body) VALUES (?)`, "after")
	}
	db.Close() // the server is stopped for a restore

	if err := Restore(ctx, res.File, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".before-restore"); err != nil {
		t.Fatalf("the replaced database was not kept: %v", err)
	}
	re, err := database.Open(ctx, path) // what the server does on start
	if err != nil {
		t.Fatalf("the server cannot open the restored database: %v", err)
	}
	var n int
	re.QueryRow(`SELECT count(*) FROM bk_rows`).Scan(&n)
	re.Close()
	if n != 50 {
		t.Fatalf("restored %d rows, want the snapshot's 50", n)
	}

	bad := filepath.Join(t.TempDir(), "aether-20991231T000000.000Z.db")
	os.WriteFile(bad, []byte("SQLite format 3\x00 but then garbage"), 0o600)
	if err := Restore(ctx, bad, path); err == nil {
		t.Fatal("a corrupt snapshot was restored")
	}
	re, _ = database.Open(ctx, path)
	re.QueryRow(`SELECT count(*) FROM bk_rows`).Scan(&n)
	re.Close()
	if n != 50 {
		t.Fatalf("a refused restore changed the database: %d rows", n)
	}
}

func TestBackupNeedsADatabaseAndADestination(t *testing.T) {
	if _, err := Run(context.Background(), Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("no database accepted")
	}
	if _, err := Run(context.Background(), Options{DBPath: "x.db"}); err == nil {
		t.Fatal("no destination accepted")
	}
}
