package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The sealing key goes with every backup: a database restored without it has
// every sealed secret unreadable, and the backups are what a new server is
// built from.
func TestTheSealingKeyIsBackedUpBesideTheSnapshots(t *testing.T) {
	_, path := live(t)
	key := filepath.Join(t.TempDir(), "aether.key")
	if err := os.WriteFile(key, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	if _, err := Run(context.Background(), Options{DBPath: path, KeyPath: key, Dir: dir, Keep: 3, Now: clock(time.Now())}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "aether.key"))
	if err != nil || string(got) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("key in the backup: %q %v", got, err)
	}
	// A key path that does not exist yet (nothing sealed so far) is not an error.
	if _, err := Run(context.Background(), Options{DBPath: path, KeyPath: filepath.Join(t.TempDir(), "none"), Dir: dir, Keep: 3, Now: clock(time.Now().Add(time.Second))}); err != nil {
		t.Fatalf("a missing key stopped the backup: %v", err)
	}
}
