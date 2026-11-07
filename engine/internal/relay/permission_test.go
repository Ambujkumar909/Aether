package relay

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pion/logging"
	pion "github.com/pion/turn/v5"
)

// The relay forwards for Aether's media server and for nothing else.
//
// Until this test the relay used pion's default permission handler, which
// admits every peer, and handed credentials to anyone who asked: the server
// was an open relay into its own network -- localhost services, a cloud
// metadata address, the private subnet. These run a real TURN client against
// the real relay: allocate, then try to send.

func freeUDP(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// allocate signs in to the relay with a credential it minted and returns the
// relayed connection.
func allocate(t *testing.T, s *Server) net.PacketConn {
	t.Helper()
	servers, err := s.ICEServers()
	if err != nil || len(servers) == 0 {
		t.Fatalf("no credential: %v", err)
	}
	addr := net.JoinHostPort(s.publicIP, strconv.Itoa(s.ports.UDP))
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelError
	c, err := pion.NewClient(&pion.ClientConfig{
		STUNServerAddr: addr, TURNServerAddr: addr,
		Username: servers[0].Username, Password: servers[0].Credential,
		Realm: "aether", Conn: conn, LoggerFactory: lf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	relayed, err := c.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	t.Cleanup(func() { relayed.Close(); c.Close(); conn.Close() })
	return relayed
}

// listen opens a UDP "peer" and reports whether anything arrives within d.
func listen(t *testing.T, ip string) (net.PacketConn, func(time.Duration) string) {
	t.Helper()
	c, err := net.ListenPacket("udp4", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot bind %s here: %v", ip, err)
	}
	t.Cleanup(func() { c.Close() })
	return c, func(d time.Duration) string {
		_ = c.SetReadDeadline(time.Now().Add(d))
		buf := make([]byte, 64)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			return ""
		}
		return string(buf[:n])
	}
}

func TestRelayForwardsToTheMediaServerAndNothingElse(t *testing.T) {
	// The media server is 127.0.0.2; 127.0.0.1 stands for everything else on
	// the server's own network (both are loopback, so one machine hosts both).
	media := net.ParseIP("127.0.0.2")
	port := freeUDP(t)
	s, err := NewWithOptions("127.0.0.1", testSecret(), quietLog(), Options{
		Ports:    Ports{UDP: port},
		RelayMin: 49300, RelayMax: 49310,
		Allow: func(ip net.IP) bool { return ip.Equal(media) },
	})
	if err != nil {
		t.Skipf("cannot bind relay here: %v", err)
	}
	defer s.Close()

	good, readGood := listen(t, "127.0.0.2")
	bad, readBad := listen(t, "127.0.0.1")
	relayed := allocate(t, s)

	// The allocation is inside the published range.
	if p := relayed.LocalAddr().(*net.UDPAddr).Port; p < 49300 || p > 49310 {
		t.Fatalf("relayed port %d is outside 49300-49310", p)
	}
	if _, err := relayed.WriteTo([]byte("media"), good.LocalAddr()); err != nil {
		t.Fatalf("sending to the media server: %v", err)
	}
	if got := readGood(3 * time.Second); got != "media" {
		t.Fatalf("the media server got %q, want \"media\"", got)
	}
	// Anything else on the server's network: refused, and nothing arrives.
	_, err = relayed.WriteTo([]byte("probe"), bad.LocalAddr())
	if err == nil {
		t.Fatal("the relay accepted a peer it does not own")
	}
	if got := readBad(time.Second); got != "" {
		t.Fatalf("a refused peer received %q through the relay", got)
	}
}

func TestRelayRefusesInternalAndMetadataAddresses(t *testing.T) {
	port := freeUDP(t)
	own := net.ParseIP("127.0.0.1")
	s, err := NewWithOptions("127.0.0.1", testSecret(), quietLog(), Options{
		Ports: Ports{UDP: port},
		Allow: func(ip net.IP) bool { return ip.Equal(own) },
	})
	if err != nil {
		t.Skipf("cannot bind relay here: %v", err)
	}
	defer s.Close()
	relayed := allocate(t, s)
	for _, peer := range []string{"169.254.169.254:80", "10.0.0.1:22", "192.168.1.1:53", "172.17.0.1:2375", "100.64.0.1:80"} {
		a, _ := net.ResolveUDPAddr("udp4", peer)
		if _, err := relayed.WriteTo([]byte("x"), a); err == nil {
			t.Errorf("the relay accepted %s", peer)
		}
	}
}

func TestNilAllowRefusesEveryPeer(t *testing.T) {
	port := freeUDP(t)
	s, err := NewWithOptions("127.0.0.1", testSecret(), quietLog(), Options{Ports: Ports{UDP: port}})
	if err != nil {
		t.Skipf("cannot bind relay here: %v", err)
	}
	defer s.Close()
	_, read := listen(t, "127.0.0.1")
	relayed := allocate(t, s)
	peer, _ := net.ResolveUDPAddr("udp4", "127.0.0.1:9")
	if _, err := relayed.WriteTo([]byte("x"), peer); err == nil {
		t.Fatal("a relay with no allow rule accepted a peer")
	}
	_ = read
}

func TestRelayRejectsAnInvertedPortRange(t *testing.T) {
	if _, err := NewWithOptions("127.0.0.1", testSecret(), quietLog(), Options{
		Ports: Ports{UDP: freeUDP(t)}, RelayMin: 50000, RelayMax: 40000,
	}); err == nil {
		t.Fatal("an inverted port range was accepted")
	}
}
