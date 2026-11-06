package sfu

import (
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
)

// sendQueueLen bounds one receiver's outbound packets: about half a second
// of nine tiles plus three talkers, and room for a keyframe burst on each.
const sendQueueLen = 512

// sendQueue is one receiver's outbound packets, written by one goroutine.
//
// Publishers used to send to every subscriber themselves, synchronously, from
// the goroutine that reads their track. A talker heard by 99 people was about
// 5,000 encrypted sends a second on one goroutine; when it fell behind, the
// packets waited in pion's inbound buffer, about a megabyte per stream, and
// arrived 41-50 s late (ARCHITECTURE 6bu). One slow receiver also delayed
// every other receiver of the track.
//
// Now a publisher only hands each forwarded packet to its receiver's queue,
// which never blocks: a full queue drops the packet and counts it. Order is
// kept because each track's packets are handed over by one goroutine and
// written by one. The payload is shared with the other receivers and never
// written to.
type sendQueue struct {
	ch      chan queued
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
}

type queued struct {
	s   *slot
	d   *Downstream
	pkt rtp.Packet
}

func newSendQueue() *sendQueue {
	q := &sendQueue{ch: make(chan queued, sendQueueLen), done: make(chan struct{})}
	go q.run()
	return q
}

// push hands a packet over without waiting. False means it was dropped: the
// queue is full (the receiver is not keeping up) or closed.
func (q *sendQueue) push(s *slot, d *Downstream, pkt rtp.Packet) bool {
	select {
	case <-q.done:
		return false
	default:
	}
	select {
	case q.ch <- queued{s, d, pkt}:
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

func (q *sendQueue) run() {
	for {
		select {
		case <-q.done:
			return
		case it := <-q.ch:
			if err := it.s.local.WriteRTP(&it.pkt); err == nil {
				it.d.count(len(it.pkt.Payload))
			}
		}
	}
}

// close stops the writer. The channel itself is never closed, so a publisher
// handing over a packet at the same moment cannot panic; what is left in it
// goes with the queue.
func (q *sendQueue) close() {
	if q != nil {
		q.once.Do(func() { close(q.done) })
	}
}
