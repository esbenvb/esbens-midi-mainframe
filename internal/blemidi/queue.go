package blemidi

import (
	"context"
	"sync"
	"time"
)

// pktQueue is a small bounded FIFO of BLE-MIDI packets between the router
// (which must never be held up for long) and the Bluetooth stack (which is
// slow and can stall when a central goes out of range).
//
// When the queue is full, what a push does depends on what it carries and on
// what is queued. SysEx packets (sx) have to arrive whole or the dump is
// corrupt, so a SysEx push waits up to `wait` for the consumer to make room.
// Anything else is disposable (fresh data beats stale data): it evicts the
// oldest queued non-SysEx packet at once, and never an unsent SysEx packet. If
// nothing but SysEx is queued, it waits like a SysEx push would, and gives up
// by dropping itself rather than punching a hole in the dump. One producer and
// one consumer are expected.
type pktQueue struct {
	cap int

	mu      sync.Mutex
	items   []queued
	closed  bool
	dropped uint64

	ready chan struct{} // signalled after a push
	space chan struct{} // signalled after a pop
}

// queued is one packet and whether it belongs to a SysEx message.
type queued struct {
	p  []byte
	sx bool
}

func newPktQueue(capacity int) *pktQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &pktQueue{cap: capacity, ready: make(chan struct{}, 1), space: make(chan struct{}, 1)}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// push adds p, which carries SysEx data if sx is set. It reports how many
// packets were dropped to make room: older ones, or p itself when the queue
// held only SysEx and stayed full for `wait`. See the type comment for the
// policy.
func (q *pktQueue) push(p []byte, sx bool, wait time.Duration) (dropped int) {
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return 0
		}
		if len(q.items) < q.cap {
			q.items = append(q.items, queued{p, sx})
			q.mu.Unlock()
			signal(q.ready)
			return dropped
		}
		if !sx {
			// Disposable: make room by dropping the oldest disposable packet.
			if i := q.oldestNonSysEx(); i >= 0 {
				q.evict(i)
				q.items = append(q.items, queued{p, sx})
				q.dropped++
				q.mu.Unlock()
				signal(q.ready)
				return dropped + 1
			}
		}
		remaining := time.Until(deadline)
		if wait <= 0 || remaining <= 0 {
			if !sx {
				// Only SysEx is queued and it is not moving: lose this packet,
				// not part of the dump.
				q.dropped++
				q.mu.Unlock()
				return dropped + 1
			}
			// A SysEx push that has waited long enough: the consumer is
			// stuck, so make room, preferring a disposable packet.
			i := q.oldestNonSysEx()
			if i < 0 {
				i = 0
			}
			q.evict(i)
			q.items = append(q.items, queued{p, sx})
			q.dropped++
			q.mu.Unlock()
			signal(q.ready)
			return dropped + 1
		}
		q.mu.Unlock()
		t := time.NewTimer(remaining)
		select {
		case <-q.space:
		case <-t.C:
		}
		t.Stop()
	}
}

// oldestNonSysEx returns the index of the oldest queued packet that does not
// belong to a SysEx message, or -1. The caller holds q.mu.
func (q *pktQueue) oldestNonSysEx() int {
	for i := range q.items {
		if !q.items[i].sx {
			return i
		}
	}
	return -1
}

// evict removes the packet at index i, keeping the order of the rest. The
// caller holds q.mu.
func (q *pktQueue) evict(i int) {
	copy(q.items[i:], q.items[i+1:])
	q.items[len(q.items)-1] = queued{}
	q.items = q.items[:len(q.items)-1]
}

// pop returns the oldest packet, waiting for one. ok is false once the queue
// is closed or ctx is done.
func (q *pktQueue) pop(ctx context.Context) (p []byte, ok bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			p = q.items[0].p
			q.items[0] = queued{}
			q.items = q.items[1:]
			q.mu.Unlock()
			signal(q.space)
			return p, true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		q.mu.Unlock()
		select {
		case <-q.ready:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// clear discards everything queued.
func (q *pktQueue) clear() {
	q.mu.Lock()
	for i := range q.items {
		q.items[i] = queued{}
	}
	q.items = nil
	q.mu.Unlock()
	signal(q.space)
}

func (q *pktQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *pktQueue) droppedTotal() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

func (q *pktQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	signal(q.ready)
	signal(q.space)
}
