package ledger

import (
	"sync"
	"sync/atomic"
	"time"
)

// Class is the accounting bucket a byte falls into. The taxonomy is chosen
// so that every bucket answers a decision: can this be removed, and if not,
// why not.
type Class uint8

const (
	ClassMediaPayload  Class = iota // picture and sound. Never remove.
	ClassRTPHeader                  // fixed 12 bytes. Irreducible browser-side.
	ClassRTPExtension               // partially droppable once SSRCs are bound.
	ClassSRTPTag                    // integrity. Never remove.
	ClassTransport                  // UDP/TCP/TURN framing.
	ClassIP                         // network header.
	ClassEthernet                   // link framing and interframe gap.
	ClassRTCP                       // feedback. Compressible and batchable.
	ClassSignalControl              // our binary control frames.
	ClassSignalFraming              // WebSocket header, mask, TLS record.
	ClassProbe                      // deliberate bandwidth probing padding.
	ClassRetransmit                 // RTX. Real cost of loss recovery.
	ClassFEC                        // forward error correction.
	numClasses
)

var classNames = [numClasses]string{
	"media payload", "RTP header", "RTP extensions", "SRTP tag",
	"UDP/TURN", "IP header", "Ethernet", "RTCP feedback",
	"signal control", "signal framing", "bandwidth probe",
	"retransmit", "FEC",
}

func (c Class) String() string {
	if int(c) >= len(classNames) {
		return "unknown"
	}
	return classNames[c]
}

// Removable reports whether bytes in this class could in principle be
// eliminated without touching what the user sees or hears. This is the
// column that turns an accounting table into a work queue.
func (c Class) Removable() bool {
	switch c {
	case ClassMediaPayload, ClassSRTPTag, ClassIP, ClassRTPHeader:
		return false
	default:
		return true
	}
}

// Counter is one class of one meter. Split across a cache line boundary is
// not worth avoiding here: the hot path is a single atomic add per packet,
// and packets arrive at tens of thousands per second per core, not millions.
type Counter struct {
	Bytes   atomic.Int64
	Packets atomic.Int64
}

// Meter records byte flow for one direction of one stream, participant, or
// server. Meters form a tree: a stream meter rolls into a participant meter,
// which rolls into a room meter. Rollup happens on read, not on write, so
// the packet path only ever touches one meter.
type Meter struct {
	Label    string
	Started  time.Time
	counters [numClasses]Counter

	mu       sync.RWMutex
	children []*Meter
}

func NewMeter(label string) *Meter {
	return &Meter{Label: label, Started: time.Now()}
}

// Child creates and registers a sub-meter.
func (m *Meter) Child(label string) *Meter {
	c := NewMeter(label)
	m.mu.Lock()
	m.children = append(m.children, c)
	m.mu.Unlock()
	return c
}

// Children returns a snapshot of the sub-meters.
func (m *Meter) Children() []*Meter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*Meter(nil), m.children...)
}

// Count records n bytes in a class, attributing one packet.
func (m *Meter) Count(c Class, n int) {
	if m == nil || c >= numClasses {
		return
	}
	m.counters[c].Bytes.Add(int64(n))
	m.counters[c].Packets.Add(1)
}

// CountBytes records bytes without implying a packet boundary, for classes
// accumulated across a frame.
func (m *Meter) CountBytes(c Class, n int) {
	if m == nil || c >= numClasses {
		return
	}
	m.counters[c].Bytes.Add(int64(n))
}

// CountRTP is the hot path: one call attributes a whole media packet across
// every layer it traverses, so the accounting cannot drift out of step with
// what was actually transmitted.
//
// payloadLen is the media bytes; the rest is derived from the path profile,
// which is exact rather than estimated because we control the socket and
// know precisely what we handed to it.
func (m *Meter) CountRTP(p Path, extBytes, payloadLen int) {
	if m == nil {
		return
	}
	m.counters[ClassMediaPayload].Bytes.Add(int64(payloadLen))
	m.counters[ClassMediaPayload].Packets.Add(1)
	m.counters[ClassRTPHeader].Bytes.Add(RTPFixedHeader)
	m.counters[ClassRTPExtension].Bytes.Add(int64(extBytes))
	m.counters[ClassSRTPTag].Bytes.Add(int64(p.SRTPTagBytes))
	m.counters[ClassTransport].Bytes.Add(int64(p.Transport.overhead()))
	if p.IPv6 {
		m.counters[ClassIP].Bytes.Add(IPv6Header)
	} else {
		m.counters[ClassIP].Bytes.Add(IPv4Header)
	}
	if p.CountEthernet {
		m.counters[ClassEthernet].Bytes.Add(EthernetOverhead)
	}
}

// CountSignal attributes one control frame: the encoded body plus the exact
// transport framing it incurred.
//
// masked should be true for client-to-server frames, which RFC 6455 obliges
// to carry a 4 byte masking key. coalesced is how many logical frames shared
// this transport frame, which is what makes the amortisation visible.
func (m *Meter) CountSignal(bodyLen int, masked bool, coalesced int) {
	if m == nil {
		return
	}
	framing := WSHeaderServerSmall
	if bodyLen >= 126 {
		framing = WSHeaderServerMed
	}
	if masked {
		framing += WSMask
	}
	framing += TLS13Record
	m.counters[ClassSignalControl].Bytes.Add(int64(bodyLen))
	if coalesced < 1 {
		coalesced = 1
	}
	m.counters[ClassSignalControl].Packets.Add(int64(coalesced))
	m.counters[ClassSignalFraming].Bytes.Add(int64(framing))
	m.counters[ClassSignalFraming].Packets.Add(1)
}

// Snapshot is an immutable read of a meter and its subtree.
type Snapshot struct {
	Label    string
	Elapsed  time.Duration
	Bytes    [numClasses]int64
	Packets  [numClasses]int64
	Children []Snapshot
}

// Snapshot reads this meter and, recursively, its children. Child bytes are
// included in the parent totals.
func (m *Meter) Snapshot() Snapshot {
	s := Snapshot{Label: m.Label, Elapsed: time.Since(m.Started)}
	for i := range m.counters {
		s.Bytes[i] = m.counters[i].Bytes.Load()
		s.Packets[i] = m.counters[i].Packets.Load()
	}
	for _, c := range m.Children() {
		cs := c.Snapshot()
		for i := range cs.Bytes {
			s.Bytes[i] += cs.Bytes[i]
			s.Packets[i] += cs.Packets[i]
		}
		s.Children = append(s.Children, cs)
	}
	return s
}

// Total is every accounted byte.
func (s Snapshot) Total() int64 {
	var n int64
	for _, b := range s.Bytes {
		n += b
	}
	return n
}

// Removable is the byte count that could in principle be eliminated without
// any perceptual change. It is the honest measure of remaining headroom, and
// it is the number this project is optimised against.
func (s Snapshot) Removable() int64 {
	var n int64
	for i, b := range s.Bytes {
		if Class(i).Removable() {
			n += b
		}
	}
	return n
}

// Efficiency is the share of bytes that carry picture or sound.
func (s Snapshot) Efficiency() float64 {
	t := s.Total()
	if t == 0 {
		return 0
	}
	return float64(s.Bytes[ClassMediaPayload]) / float64(t)
}

// Bitrate is the observed wire bitrate over the meter lifetime.
func (s Snapshot) Bitrate() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Total()) * 8 / s.Elapsed.Seconds()
}

// ToBreakdown converts a runtime snapshot into the same shape the analytic
// model produces, so measured and predicted numbers can be diffed directly.
// A gap between them is either a modelling bug or an unaccounted transmit
// path, and both are defects worth failing a build over.
func (s Snapshot) ToBreakdown() Breakdown {
	return Breakdown{
		Label:        s.Label,
		MediaPayload: s.Bytes[ClassMediaPayload],
		RTPHeader:    s.Bytes[ClassRTPHeader],
		RTPExtension: s.Bytes[ClassRTPExtension],
		SRTPTag:      s.Bytes[ClassSRTPTag],
		Transport:    s.Bytes[ClassTransport],
		IP:           s.Bytes[ClassIP],
		Ethernet:     s.Bytes[ClassEthernet],
		Packets:      s.Packets[ClassMediaPayload],
		Seconds:      s.Elapsed.Seconds(),
	}
}
