package sfu

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/pion/webrtc/v4"
)

// canBind reports whether host:port is free for UDP right now, by binding it
// and letting go.
func canBind(host string, port int) bool {
	c, err := net.ListenPacket("udp4", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// The machine's address changes: the media port follows it. Real sockets on
// loopback aliases (127.0.0.x all route to loopback on Windows and Linux).
func TestRebindFollowsTheAddress(t *testing.T) {
	port := freePort(t)
	tr, err := ListenMedia([]string{"127.0.0.1"}, port, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if canBind("127.0.0.1", port) {
		t.Fatal("127.0.0.1 should be held by the transport")
	}

	// The lease renews onto a new address.
	changed, err := tr.Rebind([]string{"127.0.0.2"})
	if err != nil || !changed {
		t.Fatalf("rebind to the new address: changed=%v err=%v", changed, err)
	}
	if !canBind("127.0.0.1", port) {
		t.Fatal("the old address is still held after it left")
	}
	if canBind("127.0.0.2", port) {
		t.Fatal("the new address is not bound")
	}
	if got := tr.Addrs(); len(got) != 1 || got[0] != "127.0.0.2" {
		t.Fatalf("Addrs = %v", got)
	}

	// Same set again: nothing happens.
	kept := tr.udpConns()[0]
	if changed, err := tr.Rebind([]string{"127.0.0.2"}); changed || err != nil {
		t.Fatalf("an unchanged set rebound: changed=%v err=%v", changed, err)
	}

	// A second address appears (a cable plugged in): the first keeps its
	// socket -- calls on it are untouched -- and the second is added.
	if changed, err := tr.Rebind([]string{"127.0.0.2", "127.0.0.3"}); !changed || err != nil {
		t.Fatalf("adding an address: changed=%v err=%v", changed, err)
	}
	conns := tr.udpConns()
	if len(conns) != 2 || conns[0] != kept {
		t.Fatalf("the kept address's socket was replaced (%d sockets)", len(conns))
	}
	if canBind("127.0.0.3", port) {
		t.Fatal("the added address is not bound")
	}

	// A bind that fails (somebody else holds the port there) changes nothing.
	squat, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.4", fmt.Sprint(port)))
	if err != nil {
		t.Skipf("cannot hold 127.0.0.4 for the failure case: %v", err)
	}
	if changed, err := tr.Rebind([]string{"127.0.0.4"}); changed || err == nil {
		t.Fatalf("a failed bind reported changed=%v err=%v", changed, err)
	}
	squat.Close()
	if got := tr.Addrs(); len(got) != 2 || got[0] != "127.0.0.2" || got[1] != "127.0.0.3" {
		t.Fatalf("a failed rebind changed the set: %v", got)
	}
	if canBind("127.0.0.2", port) || canBind("127.0.0.3", port) {
		t.Fatal("a failed rebind released a kept address")
	}

	// Empty (no route for a moment) is not "unbind everything".
	if changed, _ := tr.Rebind(nil); changed || len(tr.Addrs()) != 2 {
		t.Fatalf("an empty set unbound the transport: %v", tr.Addrs())
	}

	// Close releases everything, twice is fine.
	tr.Close()
	tr.Close()
	if !canBind("127.0.0.2", port) || !canBind("127.0.0.3", port) {
		t.Fatal("Close left an address bound")
	}
}

// Apply (every new peer) racing Rebind (the watcher): no race, no torn read.
func TestRebindRacesApply(t *testing.T) {
	port := freePort(t)
	tr, err := ListenMedia([]string{"127.0.0.1"}, port, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				se := webrtc.SettingEngine{}
				tr.Apply(&se)
				_ = tr.Addrs()
			}
		}()
	}
	sets := [][]string{{"127.0.0.2"}, {"127.0.0.1", "127.0.0.2"}, {"127.0.0.1"}}
	for i := 0; i < 60; i++ {
		if _, err := tr.Rebind(sets[i%len(sets)]); err != nil {
			t.Fatalf("rebind %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// The registry's watcher: a new reading of the routing table refreshes the
// candidate filter in place (every router holds the same pointer) and
// rebinds the port; a reading with no address keeps what there was; Close
// ends the watcher goroutine.
func TestRegistryFollowsAddressChange(t *testing.T) {
	port := freePort(t)
	rg := NewRegistry(ledger.NewMeter("test"), quietLog(), nil)
	rg.Addrs = &AddressPolicy{Log: quietLog(), allowed: map[string]bool{"127.0.0.1": true}}
	tr, err := ListenMedia([]string{"127.0.0.1"}, port, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	rg.Transport = tr
	held := rg.Addrs // what a router created earlier holds

	next := &AddressPolicy{Log: quietLog(), allowed: map[string]bool{"127.0.0.5": true}}
	if !rg.recheckAddressesFor(next, []string{"127.0.0.5"}) {
		t.Fatal("an address change was not applied")
	}
	if got := tr.Addrs(); len(got) != 1 || got[0] != "127.0.0.5" {
		t.Fatalf("port not rebound: %v", got)
	}
	if !held.Allow(net.ParseIP("127.0.0.5").To4()) || held.allowedHas("127.0.0.1") {
		t.Fatal("the filter every router holds was not refreshed in place")
	}
	// Nothing to bind: keep.
	if rg.recheckAddressesFor(&AddressPolicy{Log: quietLog(), allowed: map[string]bool{}}, nil) {
		t.Fatal("an empty reading changed something")
	}
	if got := tr.Addrs(); len(got) != 1 {
		t.Fatalf("an empty reading unbound the port: %v", got)
	}

	// The watcher goroutine stops with the registry.
	done := make(chan struct{})
	go func() {
		rg.WatchAddresses(10*time.Millisecond, func() *AddressPolicy { return rg.Addrs })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	rg.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchAddresses did not return after Close")
	}
	rg.Close() // twice is fine
}
