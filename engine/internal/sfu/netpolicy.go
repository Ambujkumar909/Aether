package sfu

import (
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
)

// Which of this machine's addresses ICE may offer.
//
// A development laptop is not one network interface. It is the real one, plus
// a Hyper-V or WSL virtual switch, plus a Docker bridge, plus a VPN adapter,
// plus two or three unplugged NICs each holding a 169.254 link-local address
// it assigned itself. Every one of those is "up", and by default pion offers
// a host candidate for each.
//
// ICE then picks the pair that connects first, and it chose 172.24.64.1 --
// the WSL adapter -- for a phone joining over Wi-Fi. That subnet exists only
// inside the laptop. The connection "succeeded" (the laptop's own browser
// could reach it), the roster filled in, and no audio or video ever reached
// the phone in either direction. Nothing logged an error, because nothing
// had gone wrong: ICE had done exactly what it was told to do.
//
// The rule below is the smallest one that fixes that: offer only addresses
// on interfaces the operating system itself would use to reach the network.
// Asking the OS which interface a packet to a public address would leave
// through -- via a UDP socket that is never actually sent on -- is the one
// portable way to find the real adapter without maintaining a list of
// virtual-adapter names that will be wrong on the next machine.
//
// Loopback is always allowed: it is what a browser on the same machine as
// the server uses, and it is what every test in this package uses.

// AddressPolicy decides which local IPs may appear as ICE candidates.
type AddressPolicy struct {
	mu      sync.RWMutex
	allowed map[string]bool // exact IPs; nil means allow everything
	Log     *slog.Logger
}

// Refresh replaces this policy's allowed set with a newer policy's, in
// place, so every router and peer holding this pointer sees it. Returns
// whether the set changed.
func (p *AddressPolicy) Refresh(from *AddressPolicy) bool {
	if p == nil || from == nil {
		return false
	}
	from.mu.RLock()
	next := make(map[string]bool, len(from.allowed))
	for k, v := range from.allowed {
		next[k] = v
	}
	nilNext := from.allowed == nil
	from.mu.RUnlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if nilNext {
		// "Could not determine": keep what we had rather than open up to
		// every adapter on a transient routing-table miss.
		return false
	}
	same := len(next) == len(p.allowed) && p.allowed != nil
	for k := range next {
		if !p.allowed[k] {
			same = false
		}
	}
	p.allowed = next
	return !same
}

// DefaultAddressPolicy allows loopback plus every address on the interface
// the OS routes to the internet through. If that cannot be determined, it
// allows everything, so a machine with no default route still works over
// loopback rather than failing closed on a guess.
func DefaultAddressPolicy(log *slog.Logger) *AddressPolicy {
	return addressPolicy(log, false)
}

// addressPolicy is DefaultAddressPolicy; quiet skips the informational log
// line, for the watcher that asks every few seconds.
func addressPolicy(log *slog.Logger, quiet bool) *AddressPolicy {
	p := &AddressPolicy{Log: log}
	if log == nil {
		p.Log = slog.Default()
	}

	iface := defaultRouteInterface()
	if iface == nil {
		p.Log.Warn("ice: could not determine the default-route interface; offering every address")
		return p
	}

	addrs, err := iface.Addrs()
	if err != nil {
		p.Log.Warn("ice: could not read addresses of default-route interface",
			"iface", iface.Name, "err", err)
		return p
	}

	p.allowed = map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
	}
	var offered []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		// IPv4 only. The second thing ICE chose, after the virtual adapter
		// was removed, was the interface's global IPv6 address -- a
		// 2409:40d4:: prefix handed out by a mobile carrier through a phone
		// hotspot. Every device on that hotspot gets its own such address,
		// and they are routed by the carrier, not by the phone, so two
		// laptops a metre apart could not reach each other's. The publisher
		// legs connected (each browser to itself) and no subscriber leg ever
		// did, which read as "chat works, nothing else does". The LAN
		// address everybody actually shares is the IPv4 one.
		if ipn.IP.To4() == nil {
			continue
		}
		p.allowed[ipn.IP.String()] = true
		offered = append(offered, ipn.IP.String())
	}
	sort.Strings(offered)
	if !quiet {
		p.Log.Info("ice: candidate addresses restricted to the default-route interface",
			"iface", iface.Name, "addresses", strings.Join(offered, " "))
	}
	return p
}

// BindAddresses returns the concrete IPv4 addresses media should bind, in a
// stable order. Empty means "no restriction was determined", and the caller
// should bind the wildcard.
//
// This exists because filtering candidates is not enough. A UDP mux bound to
// 0.0.0.0 reports EVERY interface address as a listen address, and pion
// gathers a host candidate for each of them without consulting the IP
// filter at all. The WSL virtual adapter therefore kept appearing as a
// candidate -- and once even won -- after the filter was in place. Binding
// one socket per allowed address removes the others structurally: an
// address with no socket has nothing to announce.
func (p *AddressPolicy) BindAddresses() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.allowed == nil {
		return nil
	}
	var out []string
	for ip := range p.allowed {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil || parsed.IsLoopback() {
			continue
		}
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// allowedHas reports an exact entry; for the tests.
func (p *AddressPolicy) allowedHas(ip string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowed[ip]
}

// Allow reports whether ICE may offer this address. Signature matches
// pion's SettingEngine.SetIPFilter.
func (p *AddressPolicy) Allow(ip net.IP) bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.allowed == nil {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	return p.allowed[ip.String()]
}

// defaultRouteInterface finds the interface the OS would send a packet to a
// public address through. The socket is connected, never written to: UDP
// connect only consults the routing table and binds the local address, so no
// packet leaves the machine.
func defaultRouteInterface() *net.Interface {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, RFC 5737: never routed
	if err != nil {
		return nil
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil {
		return nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local.IP) {
				return &ifaces[i]
			}
		}
	}
	return nil
}
