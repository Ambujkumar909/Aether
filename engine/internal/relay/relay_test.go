package relay

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // this is exactly what the TURN REST spec and pion use
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The relay exists so a call survives a network that blocks UDP. These test
// the parts testable without a second machine on a hostile network: that it
// binds, that it advertises URLs a remote device can actually reach, and that
// the credentials it mints are signed and time-limited exactly as its own
// auth handler will re-derive them. The relaying itself needs the hostile
// network to be a meaningful test.

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testSecret() string {
	return base64.StdEncoding.EncodeToString([]byte("a-32-byte-test-secret-000000000!"))
}

// The exact derivation pion's NewLongTermAuthHandler uses: the password is
// base64(HMAC-SHA1(secret, username)), and the username is the expiry unix
// time. Recomputed here so the test does not depend on pion internals and
// would catch pion changing the scheme underneath us.
func expectedPassword(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestRelayAdvertisesReachableURLsAndACredential(t *testing.T) {
	s, err := New("192.0.2.10", testSecret(), quietLog())
	if err != nil {
		t.Skipf("cannot bind relay ports in this environment: %v", err)
	}
	defer s.Close()

	servers, err := s.ICEServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d ICE server blocks, want 1", len(servers))
	}
	urls := servers[0].URLs

	// Every URL must point at the address given, not a virtual adapter. This
	// is the bug that shipped once: every relay URL pointed at the WSL
	// adapter (172.24.64.1), a subnet no other device can reach.
	for _, u := range urls {
		if !strings.Contains(u, "192.0.2.10") {
			t.Errorf("relay URL does not use the public IP: %s", u)
		}
	}
	// The TCP/443 path must be advertised: it is the one that survives a
	// locked-down firewall, and dropping it silently would leave the relay
	// working only on ports such a network is most likely to block.
	if !slices.Contains(urls, "turn:192.0.2.10:443?transport=tcp") {
		t.Errorf("the TCP/443 fallback URL is missing: %v", urls)
	}
	if servers[0].Username == "" || servers[0].Credential == "" {
		t.Fatal("ICE server block has no credential")
	}
}

// The advertised password must be the HMAC of the advertised username under
// the server's secret. If it is not, pion's auth handler re-derives a
// different key and refuses every allocation -- the fallback path would
// authenticate-fail and silently carry nothing.
func TestAdvertisedCredentialIsCorrectlySigned(t *testing.T) {
	secret := testSecret()
	s, err := New("192.0.2.12", secret, quietLog())
	if err != nil {
		t.Skipf("cannot bind relay ports: %v", err)
	}
	defer s.Close()

	servers, err := s.ICEServers()
	if err != nil {
		t.Fatal(err)
	}
	user := servers[0].Username
	pass := servers[0].Credential

	if got := expectedPassword(secret, user); got != pass {
		t.Fatalf("advertised password is not HMAC-SHA1(secret, username):\n got  %s\n want %s", pass, got)
	}
	// A password signed with a DIFFERENT secret must not match -- the whole
	// point of signing. This is the line that fails if the secret is ignored.
	if expectedPassword("some-other-secret", user) == pass {
		t.Fatal("the credential verified under a foreign secret; it is not actually signed")
	}
}

// The credential is time-limited: its username is a future expiry, and it sits
// within the declared TTL. Without this, "short-lived" is decoration.
func TestCredentialIsTimeLimited(t *testing.T) {
	s, err := New("192.0.2.13", testSecret(), quietLog())
	if err != nil {
		t.Skipf("cannot bind relay ports: %v", err)
	}
	defer s.Close()

	servers, _ := s.ICEServers()
	exp, err := strconv.ParseInt(servers[0].Username, 10, 64)
	if err != nil {
		t.Fatalf("username is not an expiry timestamp: %q", servers[0].Username)
	}
	now := time.Now().Unix()
	if exp <= now {
		t.Fatalf("credential already expired: exp=%d now=%d", exp, now)
	}
	if exp > now+int64(CredentialTTL.Seconds())+5 {
		t.Fatalf("credential outlives its TTL: exp=%d, ttl=%s", exp, CredentialTTL)
	}
}

func TestRequiresPublicIPAndSecret(t *testing.T) {
	if _, err := New("", testSecret(), quietLog()); err == nil {
		t.Error("relay started with no public IP")
	}
	if _, err := New("192.0.2.20", "", quietLog()); err == nil {
		t.Error("relay started with no secret")
	}
}

func TestNilRelayIsHarmless(t *testing.T) {
	var s *Server
	s.Close()
	if servers, err := s.ICEServers(); err != nil || servers != nil {
		t.Fatalf("nil relay ICEServers: got %v, %v", servers, err)
	}
}

func TestClosePathsAreSafe(t *testing.T) {
	s, err := New("192.0.2.11", testSecret(), quietLog())
	if err != nil {
		t.Skipf("cannot bind relay ports: %v", err)
	}
	s.Close()
	s.Close() // second close must not panic
	// Ports released: rebinding udp/3478 should succeed, or be taken by
	// something else (a skip, not a failure).
	if c, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(3478))); err == nil {
		c.Close()
	}
}
