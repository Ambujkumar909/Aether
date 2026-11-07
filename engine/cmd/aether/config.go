package main

import (
	"flag"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Configuration for a container: every flag can also come from the
// environment, as AETHER_ plus the flag's name in capitals with dashes as
// underscores (-media-port is AETHER_MEDIA_PORT). A flag given on the command
// line wins over the environment, so an operator can always override what an
// image or a compose file sets.

// envName is the variable a flag reads.
func envName(flagName string) string {
	return "AETHER_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnv sets every flag not given on the command line from its variable,
// and returns the names it set. A malformed value is an error naming the
// variable, never a silent default: a server that starts with a setting the
// operator did not ask for is worse than one that refuses to start.
func applyEnv(fs *flag.FlagSet, getenv func(string) (string, bool)) ([]string, error) {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var set []string
	var bad error
	fs.VisitAll(func(f *flag.Flag) {
		if bad != nil || given[f.Name] {
			return
		}
		v, ok := getenv(envName(f.Name))
		if !ok {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			bad = fmt.Errorf("%s=%q: %v", envName(f.Name), v, err)
			return
		}
		set = append(set, f.Name)
	})
	return set, bad
}

// wasSet reports whether a flag was given, on the command line or by the
// environment (applyEnv's list).
func wasSet(fs *flag.FlagSet, fromEnv []string, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	for _, n := range fromEnv {
		if n == name {
			set = true
		}
	}
	return set
}

// parsePortRange reads "49160-49200" (inclusive). Empty is no range.
func parsePortRange(s string) (uint16, uint16, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("port range %q: want min-max, e.g. 49160-49200", s)
	}
	a, err1 := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
	b, err2 := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
	if err1 != nil || err2 != nil || a == 0 || b < a {
		return 0, 0, fmt.Errorf("port range %q: want min-max with 1 <= min <= max <= 65535", s)
	}
	return uint16(a), uint16(b), nil
}

// parsePublicIP checks the address browsers reach this server on: an IPv4
// address a remote device can route to. Loopback, unspecified and link-local
// addresses are refused -- announcing one sends every remote peer at a
// subnet that exists only on this machine.
func parsePublicIP(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("public IP %q: want an IPv4 address", s)
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return "", fmt.Errorf("public IP %q: not reachable from another device", s)
	}
	return ip.String(), nil
}
