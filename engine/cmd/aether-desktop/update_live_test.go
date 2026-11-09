//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Against a real server, over a real TLS connection: the self-signed
// certificate is trusted only through its pin, and a wrong pin or none
// refuses it. Opt-in: AETHER_UPDATE_SERVER=https://host:port, a server
// started with -desktop-dir.
func TestLiveTheUpdateIsFetchedOverThePinnedConnectionOnly(t *testing.T) {
	server := os.Getenv("AETHER_UPDATE_SERVER")
	if server == "" {
		t.Skip("AETHER_UPDATE_SERVER not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pin, err := PinFor(ctx, server)
	if err != nil || pin == "" {
		t.Fatalf("pin: %q %v", pin, err)
	}
	// No pin: a self-signed server must be refused.
	if _, err := fetchManifest(ctx, pinnedClient(server, ""), server); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("unpinned client accepted the self-signed server: %v", err)
	}
	// The wrong pin: refused too.
	if _, err := fetchManifest(ctx, pinnedClient(server, CertPin(strings.Repeat("A", len(pin)))), server); err == nil {
		t.Fatal("a wrong pin was accepted")
	}
	client := pinnedClient(server, pin)
	m, err := fetchManifest(ctx, client, server)
	if err != nil || m == nil {
		t.Fatalf("manifest: %v %v", m, err)
	}
	if !m.newerThan("1.0.0") || m.newerThan("dev") {
		t.Fatalf("version %q compares wrong", m.Version)
	}
	dest := filepath.Join(t.TempDir(), exeName+updateNewSuffix)
	if err := downloadUpdate(ctx, client, server, m, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || int64(len(b)) != m.Size || !looksLikeProgram(dest) {
		t.Fatalf("downloaded %d bytes (want %d): %v", len(b), m.Size, err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		t.Fatal("hash differs from the manifest")
	}
	t.Logf("fetched %s, %d bytes, over the pinned connection", m.Version, len(b))
}
