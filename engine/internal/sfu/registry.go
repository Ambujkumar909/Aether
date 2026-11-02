package sfu

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// Registry owns one media Router per room.
type Registry struct {
	Meter *ledger.Meter
	Log   *slog.Logger
	ICE   []webrtc.ICEServer
	Addrs *AddressPolicy
	// Transport is the shared media port. nil means per-connection random
	// ports, which only tests should want.
	Transport *MediaTransport

	mu      sync.RWMutex
	routers map[string]*Router
	closed  bool

	stopWatch chan struct{} // closed by Close; ends WatchAddresses
	watchOnce sync.Once
}

func NewRegistry(meter *ledger.Meter, log *slog.Logger, ice []webrtc.ICEServer) *Registry {
	return &Registry{
		Meter: meter, Log: log, ICE: ice,
		// Decided at startup and then re-checked by WatchAddresses: it used
		// to be decided once, on the belief that the default route does not
		// change between meetings. On a laptop it does -- the Wi-Fi lease
		// renewed onto a new address twice in one evening -- and every call
		// after that fell back to the socket path.
		Addrs:     DefaultAddressPolicy(log),
		routers:   make(map[string]*Router),
		stopWatch: make(chan struct{}),
	}
}

// GetOrCreate returns the room's media router, starting it on first use, or
// nil once the registry has been closed.
//
// The nil is the point. A router owns a goroutine that recomputes every
// subscriber's layer once a second for the life of the room, and the registry
// is what remembers to stop it. Creating one after Close therefore starts a
// loop with no owner: nothing holds a reference, nothing will ever call Stop,
// and it runs until the process dies.
//
// That is reachable during ordinary shutdown rather than being theoretical.
// Modules stop in reverse dependency order while the HTTP server is still
// draining, so a join already in flight can arrive here after the media plane
// has gone. Callers treat nil the same way they treat a failed peer setup,
// which is to tell the client the server is going away.
func (rg *Registry) GetOrCreate(roomID string) *Router {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if rg.closed {
		return nil
	}
	if r, ok := rg.routers[roomID]; ok {
		return r
	}
	r := NewRouter(roomID, rg.Meter.Child("media:"+roomID), rg.Log, rg.ICE, rg.Addrs, rg.Transport)
	r.Start()
	rg.routers[roomID] = r
	return r
}

// Get returns an existing router.
func (rg *Registry) Get(roomID string) (*Router, bool) {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	r, ok := rg.routers[roomID]
	return r, ok
}

// Drop stops and removes a room's router.
func (rg *Registry) Drop(roomID string) {
	rg.mu.Lock()
	r, ok := rg.routers[roomID]
	delete(rg.routers, roomID)
	rg.mu.Unlock()
	if ok {
		r.Stop()
	}
}

// All returns every active router.
func (rg *Registry) All() []*Router {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	out := make([]*Router, 0, len(rg.routers))
	for _, r := range rg.routers {
		out = append(out, r)
	}
	return out
}

// Close stops everything, and refuses to hand out anything new afterwards.
// Safe to call more than once.
func (rg *Registry) Close() {
	rg.mu.Lock()
	rg.closed = true
	rg.watchOnce.Do(func() { close(rg.stopWatch) })
	routers := make([]*Router, 0, len(rg.routers))
	for _, r := range rg.routers {
		routers = append(routers, r)
	}
	rg.routers = make(map[string]*Router)
	rg.mu.Unlock()
	for _, r := range routers {
		r.Stop()
	}
	// After the routers: closing the listeners first would make every
	// in-flight peer close fail with a transport error rather than cleanly.
	rg.Transport.Close()
}

// WatchAddresses re-reads the default-route interface every interval and,
// when its addresses change, refreshes the candidate filter and rebinds the
// media port in place (MediaTransport.Rebind): calls on addresses that
// remain are untouched; new joins use the new address. policy is the
// source of truth, injected for the tests; nil means the real routing table.
// Returns when the registry is closed.
func (rg *Registry) WatchAddresses(interval time.Duration, policy func() *AddressPolicy) {
	if policy == nil {
		policy = func() *AddressPolicy { return addressPolicy(rg.Log, true) }
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-rg.stopWatch:
			return
		case <-tick.C:
		}
		rg.recheckAddresses(policy())
	}
}

// recheckAddresses applies one reading of the routing table. Split out so a
// test can drive it without a ticker.
func (rg *Registry) recheckAddresses(p *AddressPolicy) (changed bool) {
	return rg.recheckAddressesFor(p, p.BindAddresses())
}

// recheckAddressesFor applies a policy and the addresses to bind for it.
// (BindAddresses leaves loopback out, so the tests, which can only bind
// loopback aliases, name the set directly.)
func (rg *Registry) recheckAddressesFor(p *AddressPolicy, next []string) (changed bool) {
	if len(next) == 0 {
		return false // no route right now (Wi-Fi reconnecting): keep what we have
	}
	before := rg.Transport.Addrs()
	if rg.Transport != nil {
		c, err := rg.Transport.Rebind(next)
		if err != nil {
			rg.Log.Warn("media: address changed but the new one could not be bound; will retry", "want", strings.Join(next, " "), "err", err)
			return false
		}
		changed = c
	}
	if rg.Addrs.Refresh(p) {
		changed = true
	}
	if changed {
		rg.Log.Info("media: network address changed; media port rebound", "from", strings.Join(before, " "), "to", strings.Join(next, " "))
	}
	return changed
}
