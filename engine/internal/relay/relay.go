// Package relay embeds a TURN server so a meeting survives a network that
// blocks UDP.
//
// This is not an optimisation. It is the difference between working and not on
// the networks people actually use -- a university, a hotel, a corporate VPN.
// The failure was captured on the wire: on a campus network both browsers
// offered only TCP ICE candidates and no UDP ones at all, because Chrome falls
// back to TCP when it cannot get a UDP packet out. With no relay, TCP-only
// clients have nothing to pair with and every call fails.
//
// This is exactly what Zoom and Teams do. Their media tries UDP first (Zoom on
// 8801, Teams on 3478-3481) and falls back to TCP/TLS 443 through a relay when
// UDP is blocked -- Teams' "Transport Relays" bridge TCP to UDP, which is a
// TURN server by another name. Aether keeps its fast UDP path on 8801 and adds
// this relay for the fallback.
//
// Ports:
//   - UDP 3478  -- standard TURN, helps when only the media port's UDP is
//     filtered rather than all UDP.
//   - TCP 3478  -- standard TURN over TCP.
//   - TCP 443   -- the one that matters. 443 is almost never blocked because
//     blocking it breaks the web. Plain TCP, not TLS: a self-signed
//     certificate fails the browser's validation of a turns: server, and the
//     network here blocks UDP, not TCP, so TLS buys nothing and costs the
//     connection. When Aether has a real certificate this should become TLS.
//
// Credentials are ephemeral: a short-lived HMAC username/password minted per
// client from a shared secret, so a relay address that leaks cannot be reused.
package relay

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/pion/logging"
	pion "github.com/pion/turn/v5"
)

// CredentialTTL is how long a minted TURN credential stays valid. Long enough
// to join and reconnect through a blip, short enough that a leaked one is
// worthless by the time anybody could misuse it.
const CredentialTTL = 12 * time.Hour

// Server is the embedded TURN relay and the secret its credentials are minted
// from.
type Server struct {
	inner    *pion.Server
	secret   string
	publicIP string
	ports    Ports
	conns    []net.PacketConn
	lns      []net.Listener
	log      *slog.Logger
}

// Ports names where the relay listens. Zero for any of them skips that
// listener. The defaults are the standard TURN port for UDP and TCP, and 443
// for the TCP path that survives a firewall.
type Ports struct {
	UDP    int // default 3478
	TCP    int // default 3478
	AltTCP int // default 443
}

// DefaultPorts is what production uses.
var DefaultPorts = Ports{UDP: 3478, TCP: 3478, AltTCP: 443}

func (p Ports) plan() []struct {
	network string
	port    int
} {
	var out []struct {
		network string
		port    int
	}
	if p.UDP > 0 {
		out = append(out, struct {
			network string
			port    int
		}{"udp", p.UDP})
	}
	if p.TCP > 0 {
		out = append(out, struct {
			network string
			port    int
		}{"tcp", p.TCP})
	}
	// 443 last so a bind failure there (it needs privilege on some systems)
	// does not stop the others coming up.
	if p.AltTCP > 0 {
		out = append(out, struct {
			network string
			port    int
		}{"tcp", p.AltTCP})
	}
	return out
}

// New starts the relay. publicIP is the address clients reach it on -- the same
// LAN address the web server advertises. secret is any high-entropy string;
// the same value mints and verifies credentials, so it never leaves the server.
//
// A listener that fails to bind is logged and skipped rather than fatal: 443
// may be privileged or taken, and a relay on 3478 alone is still better than
// none. Only a total failure to bind anything is an error.
func New(publicIP, secret string, log *slog.Logger) (*Server, error) {
	return NewWithPorts(publicIP, secret, log, DefaultPorts)
}

// NewWithPorts is New with the listen ports chosen by the caller. Tests use it
// to avoid the fixed production ports; a deployment behind another service on
// 443 would too.
func NewWithPorts(publicIP, secret string, log *slog.Logger, ports Ports) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if publicIP == "" {
		return nil, fmt.Errorf("relay: public IP is required")
	}
	if secret == "" {
		return nil, fmt.Errorf("relay: shared secret is required")
	}

	s := &Server{secret: secret, publicIP: publicIP, log: log, ports: ports}

	gen := &pion.RelayAddressGeneratorStatic{
		RelayAddress: net.ParseIP(publicIP),
		Address:      "0.0.0.0",
	}

	var packetConfigs []pion.PacketConnConfig
	var listenerConfigs []pion.ListenerConfig
	var bound []string

	for _, p := range ports.plan() {
		addr := net.JoinHostPort("0.0.0.0", fmt.Sprint(p.port))
		if p.network == "udp" {
			c, err := net.ListenPacket("udp4", addr)
			if err != nil {
				log.Warn("relay: could not bind", "proto", "udp", "port", p.port, "err", err)
				continue
			}
			s.conns = append(s.conns, c)
			packetConfigs = append(packetConfigs, pion.PacketConnConfig{
				PacketConn:            c,
				RelayAddressGenerator: gen,
			})
		} else {
			l, err := net.Listen("tcp4", addr)
			if err != nil {
				log.Warn("relay: could not bind", "proto", "tcp", "port", p.port, "err", err)
				continue
			}
			s.lns = append(s.lns, l)
			listenerConfigs = append(listenerConfigs, pion.ListenerConfig{
				Listener:              l,
				RelayAddressGenerator: gen,
			})
		}
		bound = append(bound, fmt.Sprintf("%s/%d", p.network, p.port))
	}

	if len(packetConfigs) == 0 && len(listenerConfigs) == 0 {
		return nil, fmt.Errorf("relay: could not bind any listener")
	}

	// pion logs at its own levels; keep it quiet unless something is wrong.
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelWarn

	inner, err := pion.NewServer(pion.ServerConfig{
		Realm:             "aether",
		AuthHandler:       pion.NewLongTermAuthHandler(secret, lf.NewLogger("turn")),
		PacketConnConfigs: packetConfigs,
		ListenerConfigs:   listenerConfigs,
		LoggerFactory:     lf,
	})
	if err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("relay: %w", err)
	}
	s.inner = inner

	log.Info("relay: TURN listening", "public_ip", publicIP, "on", bound,
		"note", "media falls back through here when the network blocks UDP; 443 is the path that survives")
	return s, nil
}

// ICEServer is one entry in the client's RTCPeerConnection iceServers list.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEServers returns a fresh, short-lived credential and the relay URLs the
// client should try. The client keeps its own UDP-direct path first; these are
// the fallback it reaches for when that path will not form.
func (s *Server) ICEServers() ([]ICEServer, error) {
	if s == nil {
		return nil, nil
	}
	user, pass, err := pion.GenerateLongTermCredentials(s.secret, CredentialTTL)
	if err != nil {
		return nil, err
	}
	var urls []string
	if s.ports.UDP > 0 {
		urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=udp", s.publicIP, s.ports.UDP))
	}
	if s.ports.TCP > 0 {
		urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=tcp", s.publicIP, s.ports.TCP))
	}
	if s.ports.AltTCP > 0 {
		urls = append(urls, fmt.Sprintf("turn:%s:%d?transport=tcp", s.publicIP, s.ports.AltTCP))
	}
	return []ICEServer{{URLs: urls, Username: user, Credential: pass}}, nil
}

// Close stops the relay and releases its ports.
func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.inner != nil {
		_ = s.inner.Close()
		s.inner = nil
	}
	s.closeListeners()
}

func (s *Server) closeListeners() {
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
	for _, l := range s.lns {
		_ = l.Close()
	}
	s.lns = nil
}
