package main

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A laptop moves between networks. A certificate written on one network does
// not name the address the server answers on today, and a browser reaching a
// server on an unnamed address shows a NAME MISMATCH rather than the ordinary
// self-signed warning -- which on a colleague's device reads as the server
// lying about who it is, and is much harder to wave through.
//
// Real files, because the thing under test is what happens on disk.

func certPaths(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func TestACertificateNamingThisMachineIsKept(t *testing.T) {
	cert, key := certPaths(t)
	first, err := ensureCert(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("a fresh certificate named nothing")
	}
	before, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}

	// Same machine, same addresses: nothing to reissue, and reissuing anyway
	// would make every device show the warning again on every restart.
	if _, err := ensureCert(cert, key); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cert)
	if string(before) != string(after) {
		t.Fatal("a still-valid certificate was replaced")
	}
}

func TestACertificateMissingThisMachinesAddressIsReissued(t *testing.T) {
	cert, key := certPaths(t)
	addrs := LANAddresses()
	if len(addrs) == 0 {
		t.Skip("no non-loopback address on this machine to test against")
	}

	// Yesterday's network: a certificate that names loopback and nothing
	// else, exactly as if this laptop had been issued one somewhere the
	// current address did not exist.
	if _, err := issueCert(cert, key, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	stale, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}

	hosts, err := ensureCert(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	fresh, _ := os.ReadFile(cert)
	if string(fresh) == string(stale) {
		t.Fatal("a certificate that does not name this machine was served anyway")
	}
	if missing := uncovered(hosts); len(missing) != 0 {
		t.Fatalf("the reissued certificate still does not name %v", missing)
	}
	// The key must have been replaced with it, or the pair does not match and
	// the server will not start at all.
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		t.Fatalf("certificate and key do not form a pair: %v", err)
	}
}

func TestUncoveredNamesEveryAddressTheCertificateLacks(t *testing.T) {
	addrs := LANAddresses()
	if len(addrs) == 0 {
		t.Skip("no non-loopback address on this machine to test against")
	}
	if got := uncovered(addrs); len(got) != 0 {
		t.Fatalf("a certificate naming every current address reported %v missing", got)
	}
	got := uncovered(nil)
	if len(got) != len(addrs) {
		t.Fatalf("a certificate naming nothing reported %d missing, want %d", len(got), len(addrs))
	}
}
