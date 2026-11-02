// impair is a network impairment proxy for measuring media under a bad link
// on one machine. It forwards TCP byte streams and UDP datagrams and applies
// the same impairment model to both: a base one-way delay, random jitter,
// random loss (UDP only; TCP loss is what a stall looks like at the byte
// stream), and a periodic stall -- every `period`, everything is held for
// `stall` and released at once. That is what Wi-Fi power save, a channel
// scan or a retransmission burst does to a call, and it is what loopback
// never does.
//
//	impair -tcp 8445:127.0.0.1:8444 -udp 8803:127.0.0.1:8802 -delay 15ms -jitter 20ms -loss 0.01 -period 2s -stall 300ms
//
// UDP is NAT-like: each client source address gets its own upstream socket,
// so replies find their way back through the same impairment.
package main

import (
	"flag"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

type model struct {
	delay, jitter time.Duration
	loss          float64
	period, stall time.Duration
	start         time.Time
}

// releaseAt is when something that arrived at t may go out: after the base
// delay and jitter, and, if a stall is in progress at that moment, not
// before the stall ends. Stalls repeat every period from process start.
func (m *model) releaseAt(t time.Time) time.Time {
	out := t.Add(m.delay)
	if m.jitter > 0 {
		out = out.Add(time.Duration(rand.Int63n(int64(m.jitter))))
	}
	if m.period > 0 && m.stall > 0 {
		since := out.Sub(m.start) % m.period
		if since < m.stall {
			out = out.Add(m.stall - since)
		}
	}
	return out
}

// A delay line that preserves order: releases are monotonic, so a byte
// stream stays a byte stream and a burst is a burst, never a reorder.
type line struct {
	mu   sync.Mutex
	last time.Time
}

func (l *line) hold(m *model) {
	at := m.releaseAt(time.Now())
	l.mu.Lock()
	if at.Before(l.last) {
		at = l.last
	}
	l.last = at
	l.mu.Unlock()
	time.Sleep(time.Until(at))
}

func pipe(dst io.Writer, src io.Reader, m *model, l *line) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			l.hold(m)
			if _, werr := dst.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func serveTCP(spec string, m *model) {
	listen, target := split(spec)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalf("tcp listen %s: %v", listen, err)
	}
	log.Printf("tcp %s -> %s", listen, target)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				return
			}
			var l1, l2 line
			go func() { pipe(up, c, m, &l1); up.(*net.TCPConn).CloseWrite() }()
			pipe(c, up, m, &l2)
			c.Close()
			up.Close()
		}()
	}
}

func serveUDP(spec string, m *model) {
	listen, target := split(spec)
	laddr, _ := net.ResolveUDPAddr("udp", listen)
	taddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		log.Fatalf("udp target: %v", err)
	}
	front, err := net.ListenUDP("udp", laddr)
	if err != nil {
		log.Fatalf("udp listen %s: %v", listen, err)
	}
	log.Printf("udp %s -> %s", listen, target)
	var mu sync.Mutex
	ups := map[string]*net.UDPConn{}
	lines := map[string]*line{}
	buf := make([]byte, 65536)
	for {
		n, from, err := front.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if m.loss > 0 && rand.Float64() < m.loss {
			continue
		}
		key := from.String()
		mu.Lock()
		up := ups[key]
		if up == nil {
			up, err = net.DialUDP("udp", nil, taddr)
			if err != nil {
				mu.Unlock()
				continue
			}
			ups[key] = up
			lines[key] = &line{}
			back := &line{}
			// Replies: upstream -> this client, through the same model.
			go func(up *net.UDPConn, to *net.UDPAddr) {
				b := make([]byte, 65536)
				for {
					k, _, err := up.ReadFromUDP(b)
					if err != nil {
						return
					}
					if m.loss > 0 && rand.Float64() < m.loss {
						continue
					}
					pkt := make([]byte, k)
					copy(pkt, b[:k])
					go func() {
						back.hold(m)
						front.WriteToUDP(pkt, to)
					}()
				}
			}(up, from)
		}
		l := lines[key]
		mu.Unlock()
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go func() {
			l.hold(m)
			up.Write(pkt)
		}()
	}
}

func split(spec string) (listen, target string) {
	i := strings.Index(spec, ":")
	j := strings.Index(spec[i+1:], ":")
	if i < 0 || j < 0 {
		log.Fatalf("spec %q: want listenPort:host:port", spec)
	}
	return ":" + spec[:i], spec[i+1:]
}

func main() {
	tcp := flag.String("tcp", "", "listenPort:host:port for a TCP byte-stream proxy (e.g. 8445:127.0.0.1:8444)")
	udp := flag.String("udp", "", "listenPort:host:port for a UDP datagram proxy (e.g. 8803:127.0.0.1:8802)")
	m := &model{start: time.Now()}
	flag.DurationVar(&m.delay, "delay", 0, "base one-way delay")
	flag.DurationVar(&m.jitter, "jitter", 0, "uniform random extra delay, 0..jitter")
	flag.Float64Var(&m.loss, "loss", 0, "UDP loss probability per datagram (0..1)")
	flag.DurationVar(&m.period, "period", 0, "how often a stall begins (0 = never)")
	flag.DurationVar(&m.stall, "stall", 0, "how long each stall holds everything")
	flag.Parse()
	log.Printf("model: delay %v jitter %v loss %.3f stall %v every %v", m.delay, m.jitter, m.loss, m.stall, m.period)
	if *tcp != "" {
		go serveTCP(*tcp, m)
	}
	if *udp != "" {
		go serveUDP(*udp, m)
	}
	select {}
}
