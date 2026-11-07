package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func testFlags() (*flag.FlagSet, *string, *int, *bool) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", ":8443", "")
	port := fs.Int("media-port", 8801, "")
	secure := fs.Bool("secure-cookies", false, "")
	return fs, addr, port, secure
}

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestEnvironmentSetsFlagsTheCommandLineDidNot(t *testing.T) {
	fs, addr, port, secure := testFlags()
	if err := fs.Parse([]string{"-addr", ":9999"}); err != nil {
		t.Fatal(err)
	}
	set, err := applyEnv(fs, env(map[string]string{
		"AETHER_ADDR":           ":1111", // the command line wins
		"AETHER_MEDIA_PORT":     "8813",
		"AETHER_SECURE_COOKIES": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if *addr != ":9999" {
		t.Fatalf("the environment overrode the command line: addr %q", *addr)
	}
	if *port != 8813 || !*secure {
		t.Fatalf("environment not applied: port %d secure %v", *port, *secure)
	}
	if len(set) != 2 || !wasSet(fs, set, "secure-cookies") || !wasSet(fs, set, "addr") || wasSet(fs, set, "nothing") {
		t.Fatalf("set %v", set)
	}
}

func TestAMalformedVariableStopsTheServerAndSaysWhich(t *testing.T) {
	fs, _, _, _ := testFlags()
	_ = fs.Parse(nil)
	_, err := applyEnv(fs, env(map[string]string{"AETHER_MEDIA_PORT": "eighty"}))
	if err == nil || !contains(err.Error(), "AETHER_MEDIA_PORT") {
		t.Fatalf("a malformed variable was accepted or not named: %v", err)
	}
}

func TestPortRange(t *testing.T) {
	for in, want := range map[string][2]uint16{"": {0, 0}, "49160-49200": {49160, 49200}, " 5000 - 5000 ": {5000, 5000}} {
		a, b, err := parsePortRange(in)
		if err != nil || a != want[0] || b != want[1] {
			t.Errorf("%q: %d-%d %v", in, a, b, err)
		}
	}
	for _, in := range []string{"49200-49160", "0-10", "1-65536", "abc", "5000", "-5"} {
		if _, _, err := parsePortRange(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestPublicIPMustBeReachableFromAnotherDevice(t *testing.T) {
	if ip, err := parsePublicIP(" 203.0.113.7 "); err != nil || ip != "203.0.113.7" {
		t.Fatalf("a public address refused: %q %v", ip, err)
	}
	if ip, err := parsePublicIP(""); err != nil || ip != "" {
		t.Fatalf("empty: %q %v", ip, err)
	}
	for _, in := range []string{"127.0.0.1", "0.0.0.0", "169.254.1.1", "224.0.0.1", "::1", "2001:db8::1", "example.com", "300.1.1.1"} {
		if _, err := parsePublicIP(in); err == nil {
			t.Errorf("%q accepted as a public IP", in)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
