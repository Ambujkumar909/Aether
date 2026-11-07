// Package seal encrypts small secrets at rest with a key the server keeps
// beside its database: a copy of the database alone -- a stolen backup, a
// dumped table -- does not yield them.
//
// AES-256-GCM, a fresh random nonce per value, and associated data naming
// what the value belongs to, so a sealed value moved to another row does not
// open there. Keys for sealing and for keyed hashes are derived separately
// from the one master key with HKDF.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeyLen is the master key's size.
const KeyLen = 32

// Sealer seals and opens values and computes keyed hashes.
type Sealer struct {
	aead   cipher.AEAD
	macKey []byte
}

// New derives a Sealer from a master key.
func New(master []byte) (*Sealer, error) {
	if len(master) != KeyLen {
		return nil, fmt.Errorf("seal: key is %d bytes, want %d", len(master), KeyLen)
	}
	ek, err := hkdf.Key(sha256.New, master, nil, "aether seal v1", 32)
	if err != nil {
		return nil, err
	}
	mk, err := hkdf.Key(sha256.New, master, nil, "aether mac v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(ek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, macKey: mk}, nil
}

// Seal encrypts plain, bound to ad. The result is nonce || ciphertext.
func (s *Sealer) Seal(plain, ad []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // the system's randomness failing is not recoverable
	}
	return s.aead.Seal(nonce, nonce, plain, ad)
}

// ErrOpen is returned for a value that was altered, moved, or sealed with
// another key.
var ErrOpen = errors.New("seal: value does not open with this key")

// Open decrypts a value from Seal, checking it was sealed for ad.
func (s *Sealer) Open(sealed, ad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, ErrOpen
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// MAC is a keyed hash: unlike a plain hash, a copy of the stored value
// cannot be checked against guesses without the key.
func (s *Sealer) MAC(data string) string {
	m := hmac.New(sha256.New, s.macKey)
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// LoadOrCreateKey reads the master key at path, creating it (owner-only)
// on first use.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != KeyLen {
			return nil, fmt.Errorf("seal: %s is %d bytes, want %d: not a key this server made", path, len(key), KeyLen)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Written whole to a private temporary file, then linked into place: the
	// key appears complete or not at all (a crash mid-write cannot leave a
	// short one), and of two processes starting at once only one link wins.
	f, err := os.CreateTemp(filepath.Dir(path), ".key-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreateKey(path)
		}
		return nil, err
	}
	return key, nil
}
