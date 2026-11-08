package router

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/midi"
)

// These are variables only so that tests can shorten them.
var (
	// How long a SysEx chunk may wait for queue space before it (and the rest
	// of its message) is dropped. Other traffic never waits: it is dropped at
	// once if the queue is full.
	sysexEnqueueWait = 3 * time.Second
	// A SysEx that has been silent this long is considered dead; the
	// destination is released (and an F7 sent to close it) so that other
	// sources are not locked out forever.
	sysexOwnerStale = 2 * time.Second
	// Cap on events held back while another source is in the middle of a SysEx.
	// The bytes held back are capped as well, at the queue's own byte budget
	// (QueueBytes), so a destination never holds more than about twice that.
	// Released note-offs (ctlRelease) are not counted against either cap.
	maxDeferred = 4096
)

type ctl uint8

const (
	ctlNone ctl = iota
	ctlSourceGone
	// ctlRelease marks a note-off for a note the connection's tracker has
	// already forgotten (Disconnect, RemovePort, a filter change): if it were
	// dropped nothing would ever send it again, and the note would hang on the
	// destination. It is exempt from the byte budget of the queue and of the
	// deferred list. That is bounded: at most 16 channels x 128 notes of 3 bytes
	// per connection.
	ctlRelease
)

type queued struct {
	src string
	ev  midi.Event
	ctl ctl
}

// outQueue serialises everything written to one destination port. Many
// sources may feed it; it keeps their SysEx messages from interleaving
// (realtime bytes excepted, which MIDI allows anywhere), and it gives bursts
// somewhere to land so that a fast source (e.g. a network sample upload) does
// not have to be throttled byte-for-byte by a slow device.
type outQueue struct {
	ps  *portState
	r   *Router
	log *slog.Logger

	maxBytes int

	mu     sync.Mutex
	items  []queued
	bytes  int
	closed bool
	notify chan struct{}
	done   chan struct{}

	// writer-goroutine state
	owner     string
	ownerAt   time.Time // when the owner last finished handing us a chunk
	ownerOpen bool      // an F0 has been written to the device without its F7
	deferred  []queued
	// abandoned: sources whose current SysEx was dropped on the writer side
	// (deferral overflow); the rest of that message is discarded as it arrives,
	// so the destination never sees a message with a hole in it.
	abandoned  map[string]bool
	lastErrLog time.Time

	// Bytes in deferred. Written by the writer goroutine only, atomic so that
	// the stats can read it.
	deferredBytes atomic.Int64
}

func newOutQueue(r *Router, ps *portState, maxBytes int) *outQueue {
	q := &outQueue{
		ps: ps, r: r, log: r.log.With("port", ps.info.ID),
		maxBytes:  maxBytes,
		notify:    make(chan struct{}, 1),
		done:      make(chan struct{}),
		abandoned: map[string]bool{},
	}
	go q.run()
	return q
}

func (q *outQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// enqueue adds an event. SysEx chunks block (up to sysexEnqueueWait) while the
// queue is full; everything else is dropped. It reports whether the event was
// accepted.
func (q *outQueue) enqueue(src string, ev midi.Event) bool {
	return q.push(queued{src: src, ev: ev}, ev.SysEx)
}

func (q *outQueue) push(it queued, wait bool) bool {
	n := len(it.ev.Data)
	var deadline time.Time
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return false
		}
		// An empty queue always accepts, so an oversized single event can't wedge.
		if q.bytes == 0 || q.bytes+n <= q.maxBytes || it.ctl != ctlNone {
			q.items = append(q.items, it)
			q.bytes += n
			q.mu.Unlock()
			q.signal()
			return true
		}
		q.mu.Unlock()
		if !wait {
			q.ps.dropped.Add(1)
			return false
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(sysexEnqueueWait)
		} else if time.Now().After(deadline) {
			q.ps.dropped.Add(1)
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// enqueueRelease adds a released note-off (see ctlRelease). Unlike enqueue it
// is accepted when the queue is full. It reports false only when the queue has
// been closed.
func (q *outQueue) enqueueRelease(src string, ev midi.Event) bool {
	return q.push(queued{src: src, ev: ev, ctl: ctlRelease}, false)
}

// sourceGone tells the writer that src no longer feeds this destination (the
// connection was removed or the port unplugged): see dropSource.
func (q *outQueue) sourceGone(src string) {
	q.push(queued{src: src, ctl: ctlSourceGone}, false)
}

// queuedBytes is the backlog for the stats: the queue plus what is held back
// behind another source's SysEx.
func (q *outQueue) queuedBytes() int {
	q.mu.Lock()
	n := q.bytes
	q.mu.Unlock()
	return n + int(q.deferredBytes.Load())
}

// close stops the writer and discards anything still queued.
func (q *outQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *outQueue) run() {
	defer close(q.done)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		for {
			q.mu.Lock()
			if q.closed {
				q.mu.Unlock()
				return
			}
			if len(q.items) == 0 {
				q.items = nil
				q.mu.Unlock()
				break
			}
			it := q.items[0]
			q.items[0] = queued{}
			q.items = q.items[1:]
			q.mu.Unlock()

			q.handle(it)

			q.mu.Lock()
			q.bytes -= len(it.ev.Data)
			q.mu.Unlock()
		}
		select {
		case <-q.notify:
		case <-tick.C:
			q.checkOwner()
		}
	}
}

// handle processes an item that has just come off the queue.
func (q *outQueue) handle(it queued) {
	if it.ctl == ctlSourceGone {
		q.dropSource(it.src)
		return
	}
	if it.ev.SysEx && q.abandoned[it.src] {
		if !it.ev.First {
			// The rest of a message we threw away: it must not be delivered
			// as a (corrupt) message of its own.
			q.ps.dropped.Add(1)
			if it.ev.Last {
				delete(q.abandoned, it.src)
			}
			return
		}
		// A new message: the abandoned one ended without us seeing its end.
		delete(q.abandoned, it.src)
	}
	q.process(it)
}

// process writes the item, or holds it back while another source owns the
// destination. Items released from the deferred list come through here and
// not through handle: they are older than any abandonment.
func (q *outQueue) process(it queued) {
	ev := it.ev
	switch ev.Class() {
	case midi.ClassRealtime:
		q.write(it)

	case midi.ClassSysEx:
		if q.owner != "" && q.owner != it.src {
			q.deferItem(it)
			return
		}
		if q.owner == "" {
			if !ev.First {
				// Continuation of a message we already gave up on: drop it.
				q.ps.dropped.Add(1)
				return
			}
			q.owner = it.src
		}
		q.write(it)
		// Stamped after the write, not before: a device may take longer than
		// sysexOwnerStale to accept a chunk, and that is not the owner being
		// silent. The clock only runs while we wait for the owner's next chunk.
		q.ownerAt = time.Now()
		if ev.First {
			q.ownerOpen = true
		}
		if ev.Last {
			q.owner, q.ownerOpen = "", false
			q.flushDeferred()
		}

	default:
		if q.owner != "" && q.owner != it.src {
			q.deferItem(it)
			return
		}
		q.write(it)
	}
}

func (q *outQueue) deferItem(it queued) {
	q.checkOwner()
	if q.owner == "" || q.owner == it.src {
		q.process(it)
		return
	}
	n := len(it.ev.Data)
	// An empty list always accepts, like an empty queue does. A released
	// note-off is always held back: it is the only copy of that note-off.
	if it.ctl != ctlRelease && (len(q.deferred) >= maxDeferred ||
		(len(q.deferred) > 0 && int(q.deferredBytes.Load())+n > q.maxBytes)) {
		q.deferOverflow(it)
		return
	}
	q.deferred = append(q.deferred, it)
	q.deferredBytes.Add(int64(n))
}

// deferOverflow handles an item that cannot be held back. A lone short message
// is simply dropped. A SysEx chunk takes its whole message with it: the chunks
// of it already held back are thrown away and the rest is discarded as it
// arrives, because a message with a hole in it would still look complete (F0
// ... F7) to the device.
func (q *outQueue) deferOverflow(it queued) {
	q.ps.dropped.Add(1)
	if !it.ev.SysEx {
		return
	}
	q.ps.dropped.Add(uint64(q.purgeOpenSysEx(it.src)))
	if !it.ev.Last {
		q.abandoned[it.src] = true
	}
}

// purgeOpenSysEx removes src's unfinished SysEx from the deferred list: its
// SysEx chunks after the last one that ended a message. Complete messages and
// everything that is not SysEx (a released note-off, say) stay. It returns the
// number of chunks removed.
func (q *outQueue) purgeOpenSysEx(src string) int {
	lastEnd := -1
	for i, d := range q.deferred {
		if d.src == src && d.ev.SysEx && d.ev.Last {
			lastEnd = i
		}
	}
	kept := q.deferred[:0]
	removed := 0
	for i, d := range q.deferred {
		if d.src == src && d.ev.SysEx && i > lastEnd {
			removed++
			q.deferredBytes.Add(-int64(len(d.ev.Data)))
			continue
		}
		kept = append(kept, d)
	}
	for i := len(kept); i < len(q.deferred); i++ {
		q.deferred[i] = queued{} // let the data go
	}
	q.deferred = kept
	return removed
}

// dropSource is the writer's side of sourceGone: src's unfinished SysEx is
// discarded (so that it can never be flushed later and hold the destination
// for a message nobody will finish), and if src owned the destination in the
// middle of a message, that message is closed with an F7 and the destination
// is handed on at once.
func (q *outQueue) dropSource(src string) {
	delete(q.abandoned, src)
	q.ps.dropped.Add(uint64(q.purgeOpenSysEx(src)))
	if q.owner == src {
		q.releaseOwner()
	}
}

func (q *outQueue) flushDeferred() {
	d := q.deferred
	q.deferred = nil
	for _, it := range d {
		q.deferredBytes.Add(-int64(len(it.ev.Data)))
		q.process(it)
	}
}

func (q *outQueue) checkOwner() {
	if q.owner != "" && time.Since(q.ownerAt) > sysexOwnerStale {
		q.log.Warn("sysex source went quiet; releasing destination", "source", q.owner)
		q.releaseOwner()
	}
}

func (q *outQueue) releaseOwner() {
	if q.ownerOpen {
		// Close the half-sent message at the device.
		q.write(queued{src: q.owner, ev: midi.Event{Data: []byte{0xF7}, SysEx: true, Last: true}})
	}
	q.owner, q.ownerOpen = "", false
	q.flushDeferred()
}

func (q *outQueue) write(it queued) {
	data := it.ev.Data
	if len(data) == 0 {
		return
	}
	if err := q.ps.port.Write(data); err != nil {
		q.ps.errors.Add(1)
		if time.Since(q.lastErrLog) > 5*time.Second {
			q.lastErrLog = time.Now()
			q.log.Warn("write failed", "err", err)
		}
		return
	}
	q.ps.txBytes.Add(uint64(len(data)))
	if !it.ev.SysEx || it.ev.First {
		q.ps.txMsgs.Add(1)
	}
	q.ps.lastTx.Store(time.Now().UnixNano())
	if q.r.monitorOn.Load() > 0 {
		q.r.emitMonitor(q.ps.info.ID, "tx", it.src, it.ev)
	}
}
