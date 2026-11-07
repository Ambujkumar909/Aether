package seal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func newSealer(t *testing.T) *Sealer {
	t.Helper()
	k, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestASealedValueOpensOnlyWhereItWasSealed(t *testing.T) {
	s := newSealer(t)
	secret := []byte("JBSWY3DPEHPK3PXP-twenty-bytes")
	a := s.Seal(secret, []byte("user-1"))
	if bytes.Contains(a, secret) {
		t.Fatal("the plaintext is visible in the sealed value")
	}
	if got, err := s.Open(a, []byte("user-1")); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if b := s.Seal(secret, []byte("user-1")); bytes.Equal(a, b) {
		t.Fatal("two seals of one value are identical: a nonce was reused")
	}
	if _, err := s.Open(a, []byte("user-2")); !errors.Is(err, ErrOpen) {
		t.Fatal("a value moved to another account opened")
	}
	for i := range a {
		bad := append([]byte(nil), a...)
		bad[i] ^= 1
		if _, err := s.Open(bad, []byte("user-1")); err == nil {
			t.Fatalf("a value altered at byte %d opened", i)
		}
	}
	if _, err := newSealer(t).Open(a, []byte("user-1")); err == nil {
		t.Fatal("opened with another server's key")
	}
	for _, short := range [][]byte{nil, a[:5], a[:12+15]} {
		if _, err := s.Open(short, []byte("user-1")); err == nil {
			t.Fatalf("a %d-byte value opened", len(short))
		}
	}
}

func TestTheKeyedHashNeedsTheKey(t *testing.T) {
	s, other := newSealer(t), newSealer(t)
	first, second := s.MAC("ABCD-EFGH"), s.MAC("ABCD-EFGH")
	if first != second {
		t.Fatal("not deterministic")
	}
	if s.MAC("ABCD-EFGH") == other.MAC("ABCD-EFGH") || s.MAC("ABCD-EFGH") == s.MAC("ABCD-EFGI") {
		t.Fatal("the hash does not depend on the key and the input")
	}
}

func TestTheKeyIsMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "aether.key")
	keys := make([][]byte, 16)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadOrCreateKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	for i, k := range keys {
		if len(k) != KeyLen || !bytes.Equal(k, keys[0]) {
			t.Fatalf("starter %d got a different key", i)
		}
	}
	again, _ := LoadOrCreateKey(path)
	if !bytes.Equal(again, keys[0]) {
		t.Fatal("a restart made a new key")
	}
	left, _ := os.ReadDir(filepath.Join(dir, "sub"))
	if len(left) != 1 {
		t.Fatalf("files left beside the key: %v", left)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, want 0600", fi.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a file of the wrong size was taken for a key")
	}
}
