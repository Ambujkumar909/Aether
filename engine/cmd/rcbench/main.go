// rcbench drives a real remote-control input stream over a real WebSocket so
// that tshark can measure what it actually costs on the wire.
//
// Everything else about the input protocol is measured in Go tests, which
// count payload bytes. Payload bytes are not what the user pays for: a 5-byte
// mouse move riding its own TCP segment costs 5 bytes of payload and about
// 100 bytes of Ethernet, IPv6, TCP and WebSocket framing. This program exists
// so that number can be captured rather than modelled.
//
//	go run ./cmd/rcbench -port 8901 -seconds 60
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aether-rtc/aether/internal/rc"
	"github.com/coder/websocket"
)

func main() {
	port := flag.Int("port", 8901, "loopback port to use")
	seconds := flag.Int("seconds", 60, "seconds of remote control to simulate")
	window := flag.Duration("window", 15*time.Millisecond, "coalescing window")
	batched := flag.Bool("batch", true, "coalesce events into one message per window")
	screenBps := flag.Int("screen", 0, "simulated screen bytes per second sent the other way on the SAME socket")
	noInput := flag.Bool("no-input", false, "send no input at all, to price the screen stream alone")
	flag.Parse()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	type result struct {
		Messages  int `json:"messages"`
		Events    int `json:"events"`
		Payload   int `json:"payloadBytes"`
		Applied   int `json:"applied"`
		Refused   int `json:"refused"`
		WindowMs  int `json:"windowMs"`
		ScreenIn  int `json:"screenBytesReceived"`
		Batched   bool
		SessionID string
	}
	done := make(chan result, 1)

	// The controlled machine: decodes and applies through the real safety
	// session, so the measurement includes a working consent check rather
	// than a bare decoder.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)

		sess := rc.NewSession(countingInjector{})
		sess.Grant(rc.Grant{
			SessionID: "bench", ControllerID: "op",
			Expires: time.Now().Add(time.Hour),
		})

		// The screen stream runs the other way on the SAME connection, which
		// is the whole point of the comparison: an input message travelling
		// against a stream of screen data can ride a TCP segment that had to
		// be sent anyway to acknowledge it.
		if *screenBps > 0 {
			go func() {
				const frameHz = 20
				per := *screenBps / frameHz
				if per < 1 {
					per = 1
				}
				buf := make([]byte, per)
				t := time.NewTicker(time.Second / frameHz)
				defer t.Stop()
				for range t.C {
					if err := c.Write(context.Background(), websocket.MessageBinary, buf); err != nil {
						return
					}
				}
			}()
		}

		var res result
		res.SessionID = "bench"
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				break
			}
			res.Messages++
			res.Payload += len(data)
			events, err := rc.DecodeAll(data)
			if err != nil {
				log.Printf("decode: %v", err)
				break
			}
			res.Events += len(events)
			n, _ := sess.ApplyBatch("bench", events, time.Now())
			res.Applied += n
		}
		res.Refused = int(sess.Refused)
		done <- res
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// Let the listener settle so the capture sees the handshake.
	time.Sleep(200 * time.Millisecond)

	ctx := context.Background()
	cli, _, err := websocket.Dial(ctx, "ws://"+addr, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}

	// The controller must drain the screen stream, or the server's writes
	// block in the socket buffer and the capture measures a stalled
	// connection rather than a working one. An earlier version of this
	// benchmark omitted this and reported 175 kB on the wire for 480 kB of
	// screen payload, which is impossible and was the giveaway.
	screenIn := make(chan int, 1)
	go func() {
		got := 0
		for {
			_, data, err := cli.Read(ctx)
			if err != nil {
				screenIn <- got
				return
			}
			got += len(data)
		}
	}()

	rng := rand.New(rand.NewSource(5))
	x, y := uint16(30000), uint16(30000)
	pending := make([]rc.Event, 0, 64)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		out := pending
		if *batched {
			out = rc.Coalesce(out)
		}
		buf := make([]byte, 0, 256)
		for _, e := range out {
			buf, err = e.Encode(buf)
			if err != nil {
				log.Fatalf("encode: %v", err)
			}
		}
		if err := cli.Write(ctx, websocket.MessageBinary, buf); err != nil {
			log.Fatalf("write: %v", err)
		}
		pending = pending[:0]
	}

	// The same session shape the Go measurement uses: six seconds in ten
	// pointing and clicking, three typing, one reading.
	tick := time.NewTicker(*window)
	defer tick.Stop()
	start := time.Now()
	total := time.Duration(*seconds) * time.Second
	movesPerWindow := int(float64(*window) / float64(time.Second) * 60)
	if movesPerWindow < 1 {
		movesPerWindow = 1
	}

	for elapsed := time.Duration(0); elapsed < total; elapsed = time.Since(start) {
		<-tick.C
		if *noInput {
			continue
		}
		sec := int(elapsed.Seconds())
		switch {
		case sec%10 < 6:
			for i := 0; i < movesPerWindow; i++ {
				x = uint16(int(x) + rng.Intn(400) - 200)
				y = uint16(int(y) + rng.Intn(400) - 200)
				pending = append(pending, rc.Event{Kind: rc.KindMouseMove, X: x, Y: y})
			}
			if rng.Intn(40) == 0 {
				pending = append(pending,
					rc.Event{Kind: rc.KindMouseButton, Button: rc.ButtonLeft, Down: true, X: x, Y: y},
					rc.Event{Kind: rc.KindMouseButton, Button: rc.ButtonLeft, Down: false, X: x, Y: y})
			}
			flush()
		case sec%10 < 9:
			if rng.Intn(5) == 0 {
				vk := uint16(0x41 + rng.Intn(26))
				pending = append(pending,
					rc.Event{Kind: rc.KindKey, VK: vk, Down: true},
					rc.Event{Kind: rc.KindKey, VK: vk, Down: false})
				flush()
			}
		default:
			// Reading. Nothing is sent at all, which is the point: an idle
			// controller must cost nothing.
		}
	}
	flush()
	_ = cli.Close(websocket.StatusNormalClosure, "")
	var screenBytes int
	select {
	case screenBytes = <-screenIn:
	case <-time.After(3 * time.Second):
	}

	select {
	case res := <-done:
		res.WindowMs = int(window.Milliseconds())
		res.ScreenIn = screenBytes
		res.Batched = *batched
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	case <-time.After(10 * time.Second):
		log.Fatal("server never reported")
	}
}

// countingInjector accepts everything and does nothing, so the benchmark
// measures the protocol rather than the operating system.
type countingInjector struct{}

func (countingInjector) MouseMove(x, y uint16) error           { return nil }
func (countingInjector) MouseButton(b rc.Button, d bool) error { return nil }
func (countingInjector) MouseWheel(dx, dy int16) error         { return nil }
func (countingInjector) Key(vk uint16, down bool) error        { return nil }
func (countingInjector) Text(s string) error                   { return nil }
