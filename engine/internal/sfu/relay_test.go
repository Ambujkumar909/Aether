package sfu

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/relay"
	"github.com/pion/webrtc/v4"
)

// The relay path, end to end, on one machine.
//
// A client forced to relay-only -- no host candidates, exactly what a browser
// on a UDP-blocking network ends up with -- must still connect to the SFU
// through the embedded TURN server and receive media. This reproduces the
// campus failure without the campus: on the real network the remote client
// offered relay candidates and ICE still failed, and the relay and SFU were on
// the same machine there too, so whatever broke it should break here.

func relayTestPorts(t *testing.T) relay.Ports {
	t.Helper()
	return relay.Ports{UDP: freePort(t), TCP: freePort(t), AltTCP: freePort(t)}
}

func TestClientConnectsThroughRelayOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: real TURN, ICE and DTLS")
	}
	secret := base64.StdEncoding.EncodeToString([]byte("relay-integration-test-secret-00"))
	ports := relayTestPorts(t)
	rl, err := relay.NewWithPorts("127.0.0.1", secret, quietLog(), ports)
	if err != nil {
		t.Skipf("cannot bind relay in this environment: %v", err)
	}
	defer rl.Close()

	// The SFU, on its fixed media port, loopback.
	mport := freePort(t)
	tr, err := ListenMedia(nil, mport, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	r := NewRouter("relay", ledger.NewMeter("relay"), quietLog(), nil, nil, tr)
	r.Start()
	defer r.Stop()

	// ICE servers exactly as the client would fetch them from /config.
	ice, err := rl.ICEServers()
	if err != nil {
		t.Fatal(err)
	}
	var servers []webrtc.ICEServer
	for _, s := range ice {
		servers = append(servers, webrtc.ICEServer{
			URLs: s.URLs, Username: s.Username, Credential: s.Credential,
		})
	}

	// Alice publishes normally (direct). Bob is relay-only: this is the
	// participant on the hostile network.
	alice := newClient(t, r, "alice")
	bob := newClientWith(t, r, "bob", webrtc.Configuration{
		ICEServers:         servers,
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})

	stop := alice.publish("alice-cam")
	defer stop()

	select {
	case <-bob.gotTrack:
	case <-time.After(30 * time.Second):
		t.Fatalf("relay-only client never received a track through the TURN relay | alice: %s | bob: %s",
			alice.diag(), bob.diag())
	}

	// The server-side pair must show the SERVER on its media port and the
	// remote as a relay candidate: that is the proof the relay carried it.
	deadline := time.Now().Add(10 * time.Second)
	var pairStr string
	for time.Now().Before(deadline) {
		pair, err := bob.peer.sub.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err == nil && pair != nil && pair.Remote != nil {
			pairStr = fmt.Sprintf("%s <- %s", pair.Local, pair.Remote)
			if pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pairStr == "" {
		t.Fatal("no selected pair on the subscriber")
	}
	t.Logf("selected pair: %s", pairStr)

	// And real packets arrived.
	deadline = time.Now().Add(5 * time.Second)
	for bob.received.Load() < 10 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := bob.received.Load(); n < 10 {
		t.Fatalf("only %d packets arrived through the relay", n)
	}
}
