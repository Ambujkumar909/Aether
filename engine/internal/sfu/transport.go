package sfu

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Where the media actually enters and leaves this machine.
//
// By default WebRTC opens a fresh, random UDP port for every peer connection.
// That is correct for a browser and wrong for a server. Firewalls -- the one
// on this laptop, the one on the router, the one in the corporate network --
// allow inbound traffic to ports somebody named, and nobody can name a port
// that is chosen at runtime. The failure it produces was captured on the
// wire: a participant's laptop exchanged 44 kB over TCP to port 8443 --
// signalling, chat, everything the WebSocket carries -- and sent exactly
// zero UDP packets that reached this host. Not one STUN binding request. Her
// browser was sending them; they were being dropped at a random port that
// nothing had been told to allow.
//
// Every conferencing product that works without asking users to configure
// anything does the same two things:
//
//  1. ONE fixed UDP port for all media, multiplexed by ICE ufrag. Zoom uses
//     8801, Teams 3478. One port is something an installer can allow once,
//     and something an administrator can be told.
//
//  2. TCP fallback on a port that is already known to get through. Some
//     networks drop all UDP; media then rides TCP, slower under loss but
//     working. Here that is a second listener on the media port.
//
// This file provides both. The port defaults to 8801, which is Zoom's, so it
// is already open on many networks and means something to an administrator.
// It was going to be the web port, on the reasoning that a port the browser is
// already talking to is demonstrably reachable -- but the TCP fallback is a
// TCP listener, and two TCP listeners cannot share a port. The first build
// that tried it had the media plane bind first and the web server fail to
// start at all.

// MediaTransport holds the shared listeners every peer connection uses.
//
// The listeners are per address (see ListenMedia), and the set of addresses
// can change while the server runs: a laptop's Wi-Fi lease renews onto a new
// address, a cable is plugged in. Rebind follows that without a restart.
// Measured before it existed: the Wi-Fi address changed twice in one
// evening, the server kept its media port on an address the machine no
// longer had, every WebRTC connection sat in "new" with no candidate pair,
// and every call fell back to the TCP socket after six seconds -- the
// transport that measured 7.5% of speech lost under Wi-Fi stalls.
type MediaTransport struct {
	UDPPort int
	TCPPort int
	Log     *slog.Logger

	mu       sync.RWMutex
	udpMux   ice.UDPMux
	tcpMux   ice.TCPMux
	children map[string]*boundAddr // by address, for Rebind

	// tcpOnly is set on the view TCPOnly returns. A view shares the
	// listeners and must not close them; see Close.
	tcpOnly bool

	// wildcard is true when bound to every interface (no addresses named),
	// which only the in-process tests do. It gates the loopback candidate:
	// tests need it to pair pion with pion on one machine; a real deployment
	// must not advertise 127.0.0.1 to remote peers, where it is noise that
	// every relay candidate gets pointlessly paired against.
	wildcard bool

	// PublicIP, when set, replaces the address in every host candidate this
	// server announces, keeping the port. It is the address browsers reach
	// the media port on when the server is behind a 1:1 NAT -- a container
	// (its interface is a private bridge address only the host can reach) or
	// a cloud VM with a mapped public address. Empty announces the bound
	// addresses themselves, which is right on a LAN.
	PublicIP string
}

// One address's sockets: a UDP socket and its mux, and (best effort) a TCP
// listener and its mux.
type boundAddr struct {
	udp    net.PacketConn
	udpMux ice.UDPMux
	tcp    net.Listener
	tcpMux ice.TCPMux
}

func (b *boundAddr) close() {
	if b.udpMux != nil {
		_ = b.udpMux.Close()
	}
	if b.udp != nil {
		_ = b.udp.Close()
	}
	if b.tcpMux != nil {
		_ = b.tcpMux.Close()
	}
	if b.tcp != nil {
		_ = b.tcp.Close()
	}
}

func bindAddr(host string, port int, log *slog.Logger) (*boundAddr, error) {
	hp := net.JoinHostPort(host, fmt.Sprint(port))
	udp, err := net.ListenPacket("udp4", hp)
	if err != nil {
		return nil, fmt.Errorf("media: bind udp %s: %w", hp, err)
	}
	b := &boundAddr{udp: udp, udpMux: ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: udp})}
	// TCP fallback. Its failure is not fatal: UDP is the normal path, and
	// a network that blocks UDP is the exception this covers rather than
	// the case it depends on.
	tcp, err := net.Listen("tcp4", hp)
	if err != nil {
		log.Warn("media: tcp fallback unavailable on this address", "addr", hp, "err", err)
		return b, nil
	}
	b.tcp = tcp
	b.tcpMux = ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: tcp, ReadBufferSize: 8})
	return b, nil
}

// ListenMedia binds the shared media port.
//
// addrs names the interfaces to bind; nil or empty means every interface.
// One UDP socket and one TCP listener are opened PER ADDRESS and joined with
// pion's multi-mux, rather than one wildcard socket for all of them. The
// difference matters: a wildcard mux announces every interface the machine
// has, bypassing the candidate filter entirely, and the virtual adapters
// that filter exists to remove came straight back. An address with no socket
// has nothing to announce.
//
// Named addresses are bound exactly as given -- loopback is NOT added. It
// used to be, so in-process tests could pair on one machine, but a bound
// socket is an announced candidate: the multi-mux reports every address it
// holds, and pion's loopback gate does not apply to it. So a production
// server kept advertising 127.0.0.1:8801 to every remote peer as a
// candidate they had to fail against. Tests bind the wildcard, which
// includes loopback on its own.
//
// If a UDP bind fails the error is returned rather than silently falling
// back to random ports: random ports are the bug this exists to remove, and
// starting up in the broken configuration with no indication is worse than
// refusing to start.
func ListenMedia(addrs []string, port int, log *slog.Logger) (*MediaTransport, error) {
	if log == nil {
		log = slog.Default()
	}
	t := &MediaTransport{UDPPort: port, TCPPort: port, Log: log, children: map[string]*boundAddr{}}
	hosts := append([]string(nil), addrs...)
	if len(hosts) == 0 {
		hosts = []string{"0.0.0.0"}
		t.wildcard = true
	}
	for _, h := range hosts {
		b, err := bindAddr(h, port, log)
		if err != nil {
			t.closeAll()
			return nil, err
		}
		t.children[h] = b
	}
	t.compose()
	log.Info("media: listening", "addrs", strings.Join(hosts, " "), "udp", port,
		"tcp", t.tcpMux != nil,
		"note", "allow inbound UDP and TCP on this one port; nothing else is needed")
	return t, nil
}

// compose rebuilds the multi-muxes over the current children. Caller holds
// mu (or owns t exclusively). The previous multi-mux wrappers are dropped,
// not closed: closing one closes every child it wraps, including the ones
// that are kept. A multi-mux holds no goroutine of its own.
func (t *MediaTransport) compose() {
	var udp []ice.UDPMux
	var tcp []ice.TCPMux
	for _, h := range t.sortedHosts() {
		b := t.children[h]
		udp = append(udp, b.udpMux)
		if b.tcpMux != nil {
			tcp = append(tcp, b.tcpMux)
		}
	}
	t.udpMux, t.tcpMux = nil, nil
	if len(udp) > 0 {
		t.udpMux = ice.NewMultiUDPMuxDefault(udp...)
	}
	if len(tcp) > 0 {
		t.tcpMux = ice.NewMultiTCPMuxDefault(tcp...)
	}
}

func (t *MediaTransport) sortedHosts() []string {
	hs := make([]string, 0, len(t.children))
	for h := range t.children {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return hs
}

// udpConns is the bound UDP sockets, in address order; for the tests.
func (t *MediaTransport) udpConns() []net.PacketConn {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []net.PacketConn
	for _, h := range t.sortedHosts() {
		out = append(out, t.children[h].udp)
	}
	return out
}

// Addrs returns the addresses currently bound, sorted.
func (t *MediaTransport) Addrs() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.sortedHosts()
}

// Rebind makes the bound set exactly addrs. Addresses already bound keep
// their sockets -- a call in progress on one is not touched; new ones are
// bound; ones no longer wanted are closed (their calls were already dead:
// the address has left the machine). Peer connections created afterwards
// get the new set. On a bind failure nothing changes and the error is
// returned, so the caller can try again later. A wildcard transport, an
// empty addrs, or an unchanged set is a no-op.
func (t *MediaTransport) Rebind(addrs []string) (changed bool, err error) {
	if t == nil || t.tcpOnly || t.wildcard || len(addrs) == 0 {
		return false, nil
	}
	want := map[string]bool{}
	for _, a := range addrs {
		want[a] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	same := len(want) == len(t.children)
	for a := range want {
		if t.children[a] == nil {
			same = false
		}
	}
	if same {
		return false, nil
	}
	added := map[string]*boundAddr{}
	for a := range want {
		if t.children[a] != nil {
			continue
		}
		b, err := bindAddr(a, t.UDPPort, t.Log)
		if err != nil {
			for _, x := range added {
				x.close()
			}
			return false, err
		}
		added[a] = b
	}
	for a, b := range t.children {
		if !want[a] {
			b.close()
			delete(t.children, a)
		}
	}
	for a, b := range added {
		t.children[a] = b
	}
	t.compose()
	return true, nil
}

// TCPOnly returns a view of the same listeners that offers no UDP candidates.
//
// It exists so that the TCP fallback can be tested the way it will be used:
// with UDP genuinely absent rather than merely losing the race. It is also
// what an operator reaches for on a network known to drop UDP, where offering
// it only delays connection by the ICE timeout.
func (t *MediaTransport) TCPOnly() *MediaTransport {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return &MediaTransport{UDPPort: t.UDPPort, TCPPort: t.TCPPort, Log: t.Log, tcpMux: t.tcpMux, tcpOnly: true, wildcard: t.wildcard, PublicIP: t.PublicIP}
}

// Apply configures a SettingEngine to use the shared listeners.
func (t *MediaTransport) Apply(se *webrtc.SettingEngine) {
	if t == nil {
		return
	}
	t.mu.RLock()
	udpMux, tcpMux := t.udpMux, t.tcpMux
	t.mu.RUnlock()
	var types []webrtc.NetworkType
	if udpMux != nil && !t.tcpOnly {
		se.SetICEUDPMux(udpMux)
		types = append(types, webrtc.NetworkTypeUDP4)
	}
	// ICE-TCP is deliberately NOT offered any more. It was never shown to
	// pair (its test is an honest skip), and every "host tcp" candidate the
	// server advertised was one more dead pair a browser had to time out
	// against. The TCP fallback that actually works is the TURN relay over
	// TCP/443 (internal/relay), which is proven end to end. The TCP mux stays
	// bound only so TCPOnly() views used by tests keep a listener to point at.
	if t.tcpOnly && tcpMux != nil {
		se.SetICETCPMux(tcpMux)
		types = append(types, webrtc.NetworkTypeTCP4)
	}
	if len(types) == 0 {
		return
	}
	// Loopback candidates only for a wildcard bind, i.e. the tests. On a
	// real deployment 127.0.0.1 is unreachable for every remote peer and
	// only adds pairs that must fail before the real one is tried.
	if t.wildcard {
		se.SetIncludeLoopbackCandidate(true)
	}
	// IPv4 only, for the reason recorded in netpolicy.go: on a phone hotspot
	// every device holds its own carrier-routed IPv6 address and none of them
	// can reach each other's.
	se.SetNetworkTypes(types)
	// Behind a 1:1 NAT the bound address is unreachable from outside; the
	// candidate carries the public one instead (same port, which the NAT or
	// the container's port mapping forwards unchanged).
	if t.PublicIP != "" {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{t.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil && t.Log != nil {
			t.Log.Error("media: public address not applied", "public_ip", t.PublicIP, "err", err)
		}
	}
}

// Close releases the listeners. Safe to call more than once and on nil.
func (t *MediaTransport) Close() {
	if t == nil || t.tcpOnly {
		// A TCP-only view borrows the owner's listeners; closing them from
		// here would take the owner's UDP path down with it.
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeAll()
}

func (t *MediaTransport) closeAll() {
	for _, b := range t.children {
		b.close()
	}
	t.children = map[string]*boundAddr{}
	t.udpMux, t.tcpMux = nil, nil
}
