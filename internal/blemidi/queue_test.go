package blemidi

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestQueueFIFO(t *testing.T) {
	q := newPktQueue(8)
	for i := 0; i < 5; i++ {
		q.push([]byte{byte(i)}, false, 0)
	}
	if q.len() != 5 {
		t.Fatal("len")
	}
	for i := 0; i < 5; i++ {
		p, ok := q.pop(ctxT(t))
		if !ok || p[0] != byte(i) {
			t.Fatalf("pop %d: %v %v", i, p, ok)
		}
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	q := newPktQueue(3)
	var dropped int
	for i := 0; i < 6; i++ {
		dropped += q.push([]byte{byte(i)}, false, 0)
	}
	if dropped != 3 || q.droppedTotal() != 3 || q.len() != 3 {
		t.Fatalf("dropped=%d total=%d len=%d", dropped, q.droppedTotal(), q.len())
	}
	for want := byte(3); want < 6; want++ {
		p, _ := q.pop(ctxT(t))
		if p[0] != want {
			t.Fatalf("got %d want %d: oldest must go first", p[0], want)
		}
	}
}

func TestQueueWaitsForRoomWhenAsked(t *testing.T) {
	q := newPktQueue(2)
	q.push([]byte{0}, true, 0)
	q.push([]byte{1}, true, 0)
	go func() {
		time.Sleep(40 * time.Millisecond)
		q.pop(context.Background())
	}()
	start := time.Now()
	if d := q.push([]byte{2}, true, 2*time.Second); d != 0 {
		t.Fatalf("dropped %d although the consumer made room", d)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("push did not wait")
	}
	if q.droppedTotal() != 0 || q.len() != 2 {
		t.Fatal("state after wait")
	}
}

func TestQueueGivesUpWaitingAndDropsOldest(t *testing.T) {
	q := newPktQueue(1)
	q.push([]byte{0}, true, 0)
	start := time.Now()
	if d := q.push([]byte{1}, true, 50*time.Millisecond); d != 1 {
		t.Fatalf("dropped %d", d)
	}
	if e := time.Since(start); e < 40*time.Millisecond || e > time.Second {
		t.Fatalf("waited %v", e)
	}
	p, _ := q.pop(ctxT(t))
	if p[0] != 1 {
		t.Fatal("newest packet must survive")
	}
}

func TestQueuePopBlocksUntilPushOrCancel(t *testing.T) {
	q := newPktQueue(4)
	go func() {
		time.Sleep(20 * time.Millisecond)
		q.push([]byte{9}, false, 0)
	}()
	p, ok := q.pop(ctxT(t))
	if !ok || p[0] != 9 {
		t.Fatalf("pop = %v %v", p, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, ok := q.pop(ctx); ok {
		t.Fatal("pop must return false on cancel")
	}
}

func TestQueueCloseAndClear(t *testing.T) {
	q := newPktQueue(4)
	q.push([]byte{1}, false, 0)
	q.clear()
	if q.len() != 0 {
		t.Fatal("clear")
	}
	q.push([]byte{2}, false, 0)
	q.close()
	if p, ok := q.pop(ctxT(t)); !ok || p[0] != 2 {
		t.Fatal("a closed queue still drains")
	}
	if _, ok := q.pop(ctxT(t)); ok {
		t.Fatal("closed and empty must return false")
	}
	if d := q.push([]byte{3}, false, 0); d != 0 || q.len() != 0 {
		t.Fatal("push after close must be ignored")
	}
}

// drain pops everything currently queued.
func drain(t *testing.T, q *pktQueue) []byte {
	t.Helper()
	var out []byte
	for n := q.len(); n > 0; n-- {
		p, ok := q.pop(ctxT(t))
		if !ok {
			t.Fatal("pop failed")
		}
		out = append(out, p[0])
	}
	return out
}

// A disposable packet (realtime, a note) on a full queue evicts the oldest
// disposable packet, never an unsent SysEx packet in front of it.
func TestQueueNonSysExEvictsOldestNonSysEx(t *testing.T) {
	q := newPktQueue(5)
	q.push([]byte{1}, true, 0) // SysEx
	q.push([]byte{2}, true, 0)
	q.push([]byte{10}, false, 0) // disposable
	q.push([]byte{3}, true, 0)
	q.push([]byte{11}, false, 0)

	start := time.Now()
	if d := q.push([]byte{12}, false, 2*time.Second); d != 1 {
		t.Fatalf("dropped %d, want 1", d)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("a disposable packet must not wait when another disposable one can go")
	}
	if got, want := drain(t, q), []byte{1, 2, 3, 11, 12}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v (oldest non-SysEx evicted, SysEx intact, order kept)", got, want)
	}
	if q.droppedTotal() != 1 {
		t.Fatalf("droppedTotal %d", q.droppedTotal())
	}
}

// With nothing but SysEx queued, a disposable packet waits for the consumer
// like SysEx does, and gets in without costing anything.
func TestQueueNonSysExWaitsWhenOnlySysExIsQueued(t *testing.T) {
	q := newPktQueue(2)
	q.push([]byte{1}, true, 0)
	q.push([]byte{2}, true, 0)
	go func() {
		time.Sleep(40 * time.Millisecond)
		q.pop(context.Background())
	}()
	start := time.Now()
	if d := q.push([]byte{9}, false, 2*time.Second); d != 0 {
		t.Fatalf("dropped %d although the consumer made room", d)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("push did not wait")
	}
	if got, want := drain(t, q), []byte{2, 9}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if q.droppedTotal() != 0 {
		t.Fatal("nothing should have been dropped")
	}
}

// If the SysEx never moves, the disposable newcomer is the one that is lost.
func TestQueueNonSysExDropsItselfRatherThanSysEx(t *testing.T) {
	q := newPktQueue(2)
	q.push([]byte{1}, true, 0)
	q.push([]byte{2}, true, 0)
	start := time.Now()
	if d := q.push([]byte{9}, false, 50*time.Millisecond); d != 1 {
		t.Fatalf("dropped %d, want 1", d)
	}
	if e := time.Since(start); e < 40*time.Millisecond || e > time.Second {
		t.Fatalf("waited %v", e)
	}
	if got, want := drain(t, q), []byte{1, 2}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v: the SysEx packets must all survive", got, want)
	}
	if q.droppedTotal() != 1 {
		t.Fatalf("droppedTotal %d", q.droppedTotal())
	}

	// And without a wait it drops itself at once.
	q.push([]byte{1}, true, 0)
	q.push([]byte{2}, true, 0)
	if d := q.push([]byte{9}, false, 0); d != 1 {
		t.Fatalf("no-wait push dropped %d, want 1", d)
	}
	if got, want := drain(t, q), []byte{1, 2}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

// A SysEx push that gives up waiting still prefers to sacrifice a disposable
// packet over an older SysEx packet.
func TestQueueSysExTimeoutPrefersNonSysEx(t *testing.T) {
	q := newPktQueue(3)
	q.push([]byte{10}, false, 0)
	q.push([]byte{1}, true, 0)
	q.push([]byte{2}, true, 0)
	if d := q.push([]byte{3}, true, 20*time.Millisecond); d != 1 {
		t.Fatalf("dropped %d", d)
	}
	if got, want := drain(t, q), []byte{1, 2, 3}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

// Only SysEx queued and the SysEx push times out: the last resort is the
// oldest packet, as before.
func TestQueueSysExTimeoutDropsOldestSysExAsLastResort(t *testing.T) {
	q := newPktQueue(2)
	q.push([]byte{1}, true, 0)
	q.push([]byte{2}, true, 0)
	if d := q.push([]byte{3}, true, 20*time.Millisecond); d != 1 {
		t.Fatalf("dropped %d", d)
	}
	if got, want := drain(t, q), []byte{2, 3}; string(got) != string(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

// A saturated dump with disposable traffic mixed in: every SysEx packet comes
// out the other end, in order, however the consumer lags and however the
// realtime bytes happen to line up with the chunks.
func TestQueueSaturatedDumpWithInterleavedRealtime(t *testing.T) {
	for _, every := range []int{1, 2, 3, 5} { // one clock tick per this many SysEx packets
		t.Run(fmt.Sprintf("tick every %d", every), func(t *testing.T) {
			const packets = 400
			q := newPktQueue(8)
			var (
				mu  sync.Mutex
				got []byte
			)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					p, ok := q.pop(ctx)
					if !ok {
						return
					}
					if p[0] < 0x80 { // SysEx packets carry their sequence number
						mu.Lock()
						got = append(got, p[0])
						mu.Unlock()
					}
					time.Sleep(100 * time.Microsecond) // slower than the producer
				}
			}()
			for i := 0; i < packets; i++ {
				q.push([]byte{byte(i % 128)}, true, 5*time.Second)
				if i%every == 0 {
					q.push([]byte{0xF8}, false, 5*time.Second)
				}
			}
			waitUntil(t, "queue drained", func() bool { return q.len() == 0 })
			time.Sleep(10 * time.Millisecond)
			cancel()
			<-done

			mu.Lock()
			defer mu.Unlock()
			if len(got) != packets {
				t.Fatalf("%d of %d SysEx packets arrived (%d dropped by the queue)", len(got), packets, q.droppedTotal())
			}
			for i, b := range got {
				if b != byte(i%128) {
					t.Fatalf("SysEx packet %d arrived as %d: out of order or corrupted", i, b)
				}
			}
		})
	}
}
