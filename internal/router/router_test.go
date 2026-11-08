package router

import (
	"bytes"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"emm/internal/midi"
)

// fakePort records everything written to it. gate, when non-nil, makes every
// Write wait for a token (to simulate a slow device).
type fakePort struct {
	info PortInfo

	mu     sync.Mutex
	writes [][]byte
	closed bool
	gate   chan struct{}
}

func newPort(id string, in, out bool) *fakePort {
	return &fakePort{info: PortInfo{ID: id, Name: id, Kind: KindVirtual, Input: in, Output: out}}
}

func (p *fakePort) Info() PortInfo { return p.info }
func (p *fakePort) Write(b []byte) error {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	p.writes = append(p.writes, append([]byte(nil), b...))
	p.mu.Unlock()
	return nil
}
func (p *fakePort) Close() error { p.mu.Lock(); p.closed = true; p.mu.Unlock(); return nil }

func (p *fakePort) got() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []byte
	for _, w := range p.writes {
		all = append(all, w...)
	}
	return all
}

func (p *fakePort) chunks() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.writes...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newRouter() *Router {
	return New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func mustConnect(t *testing.T, r *Router, from, to string, f Filter) {
	t.Helper()
	if err := r.Connect(from, to, f); err != nil {
		t.Fatalf("connect %s->%s: %v", from, to, err)
	}
}

func TestRouteAtoB(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, true), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})

	ha.Receive([]byte{0x90, 60, 100, 0x80, 60, 0})
	waitFor(t, "two messages at b", func() bool { return len(b.got()) == 6 })
	if !bytes.Equal(b.got(), []byte{0x90, 60, 100, 0x80, 60, 0}) {
		t.Fatalf("got %x", b.got())
	}
	if len(a.got()) != 0 {
		t.Fatal("source received its own data")
	}
}

func TestFanOutAndMerge(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, true), newPort("b", true, true)
	c, d := newPort("c", false, true), newPort("d", false, true)
	ha, hb := r.AddPort(a), r.AddPort(b)
	r.AddPort(c)
	r.AddPort(d)
	mustConnect(t, r, "a", "c", Filter{})
	mustConnect(t, r, "a", "d", Filter{})
	mustConnect(t, r, "b", "c", Filter{})

	ha.Receive([]byte{0xC0, 1})
	hb.Receive([]byte{0xC1, 2})
	waitFor(t, "c has both", func() bool { return len(c.got()) == 4 })
	waitFor(t, "d has one", func() bool { return len(d.got()) == 2 })
	if !bytes.Equal(d.got(), []byte{0xC0, 1}) {
		t.Fatalf("d got %x", d.got())
	}
}

func TestConnectValidation(t *testing.T) {
	r := newRouter()
	r.AddPort(newPort("in", true, false))
	r.AddPort(newPort("out", false, true))
	if err := r.Connect("in", "in", Filter{}); err != ErrSamePort {
		t.Errorf("same port: %v", err)
	}
	if err := r.Connect("out", "out", Filter{}); err != ErrSamePort {
		t.Errorf("same port: %v", err)
	}
	if err := r.Connect("out", "in", Filter{}); err == nil {
		t.Error("connecting output->input must fail")
	}
	if err := r.Connect("in", "out", Filter{Block: []string{"bogus"}}); err == nil {
		t.Error("bad filter accepted")
	}
	if err := r.Connect("in", "out", Filter{Channels: []int{17}}); err == nil {
		t.Error("bad channel accepted")
	}
	if err := r.Connect("in", "out", Filter{Transpose: 100}); err == nil {
		t.Error("bad transpose accepted")
	}
	if err := r.Disconnect("in", "out"); err != ErrNoSuchConn {
		t.Errorf("disconnect missing: %v", err)
	}
}

func TestFilters(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)

	mustConnect(t, r, "a", "b", Filter{Block: []string{"cc", "clock"}, Channels: []int{1, 3}})
	ha.Receive([]byte{
		0x90, 60, 100, // ch1 note: pass
		0xB0, 7, 1, // cc: blocked
		0xF8,        // clock: blocked
		0x91, 61, 1, // ch2 note: not allowed
		0x92, 62, 2, // ch3 note: pass
		0xFA, // start: pass
	})
	waitFor(t, "filtered output", func() bool { return len(b.got()) == 7 })
	if !bytes.Equal(b.got(), []byte{0x90, 60, 100, 0x92, 62, 2, 0xFA}) {
		t.Fatalf("got %x", b.got())
	}
}

func TestMapChannelAndTranspose(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{MapChannel: 5, Transpose: 12})
	ha.Receive([]byte{0x90, 60, 100, 0x81, 120, 0, 0xB2, 7, 99})
	// 120+12 = 132 is out of range: dropped. CC is remapped but not transposed.
	waitFor(t, "output", func() bool { return len(b.got()) == 6 })
	if !bytes.Equal(b.got(), []byte{0x94, 72, 100, 0xB4, 7, 99}) {
		t.Fatalf("got %x", b.got())
	}
}

func TestSetFilterLive(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	ha.Receive([]byte{0xC0, 1})
	waitFor(t, "first", func() bool { return len(b.got()) == 2 })
	if err := r.SetFilter("a", "b", Filter{Block: []string{"program"}}); err != nil {
		t.Fatal(err)
	}
	ha.Receive([]byte{0xC0, 2})
	ha.Receive([]byte{0xB0, 1, 1})
	waitFor(t, "cc", func() bool { return len(b.got()) == 5 })
	if !bytes.Equal(b.got(), []byte{0xC0, 1, 0xB0, 1, 1}) {
		t.Fatalf("got %x", b.got())
	}
}

func TestDormantConnectionWakesUp(t *testing.T) {
	r := newRouter()
	r.Load([]ConnSpec{{From: "a", To: "b", Filter: Filter{}}, {From: "x", To: "x"}, {From: "a", To: "c", Filter: Filter{Block: []string{"nope"}}}})
	if got := r.Connections(); len(got) != 1 {
		t.Fatalf("expected 1 valid connection, got %d", len(got))
	}
	snap := r.Snapshot()
	if len(snap.Connections) != 1 || snap.Connections[0].Online {
		t.Fatal("connection must be dormant")
	}
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	if r.Snapshot().Connections[0].Online {
		t.Fatal("still dormant with only one end")
	}
	r.AddPort(b)
	if !r.Snapshot().Connections[0].Online {
		t.Fatal("should be online")
	}
	ha.Receive([]byte{0xC0, 9})
	waitFor(t, "delivery", func() bool { return len(b.got()) == 2 })

	// Unplug and replug the destination: the connection survives.
	r.RemovePort("b")
	if r.Snapshot().Connections[0].Online {
		t.Fatal("should be dormant again")
	}
	b2 := newPort("b", false, true)
	r.AddPort(b2)
	ha.Receive([]byte{0xC0, 10})
	waitFor(t, "delivery after replug", func() bool { return len(b2.got()) == 2 })
}

func TestHeldNotesReleasedOnDisconnect(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	ha.Receive([]byte{0x90, 60, 100, 0x93, 64, 100, 0x90, 62, 100, 0x90, 62, 0}) // 62 released again
	waitFor(t, "notes", func() bool { return len(b.got()) == 12 })

	if err := r.Disconnect("a", "b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "note offs", func() bool { return len(b.got()) == 12+6 })
	tail := b.got()[12:]
	want := map[string]bool{string([]byte{0x80, 60, 0}): true, string([]byte{0x83, 64, 0}): true}
	for i := 0; i < len(tail); i += 3 {
		if !want[string(tail[i:i+3])] {
			t.Fatalf("unexpected release %x", tail[i:i+3])
		}
		delete(want, string(tail[i:i+3]))
	}
	if len(want) != 0 {
		t.Fatalf("missing release %v", want)
	}
}

func TestHeldNotesReleasedWhenSourceUnplugged(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	ha.Receive([]byte{0x90, 60, 100})
	waitFor(t, "note on", func() bool { return len(b.got()) == 3 })
	r.RemovePort("a")
	waitFor(t, "release", func() bool { return len(b.got()) == 6 })
	if !bytes.Equal(b.got()[3:], []byte{0x80, 60, 0}) {
		t.Fatalf("got %x", b.got())
	}
	if !a.closed {
		t.Fatal("removed port must be closed")
	}
	// Receiving on a stale handle must be harmless.
	ha.Receive([]byte{0x90, 61, 100})
}

func TestSysExDoesNotInterleaveBetweenSources(t *testing.T) {
	r := newRouter()
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1, 2, 3}) // sysex starts
	hc.Receive([]byte{0x90, 60, 100}) // must wait
	hc.Receive([]byte{0xF8})          // realtime: allowed immediately
	ha.Receive([]byte{4, 5, 0xF7})    // sysex ends
	hc.Receive([]byte{0x80, 60, 0})   // after
	waitFor(t, "all output", func() bool { return len(b.got()) == 4+1+3+3+3 })

	want := []byte{0xF0, 1, 2, 3, 0xF8, 4, 5, 0xF7, 0x90, 60, 100, 0x80, 60, 0}
	if !bytes.Equal(b.got(), want) {
		t.Fatalf("got  %x\nwant %x", b.got(), want)
	}
}

func TestSysExTwoSourcesSerialised(t *testing.T) {
	r := newRouter()
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1})
	hc.Receive([]byte{0xF0, 9})
	ha.Receive([]byte{2, 0xF7})
	hc.Receive([]byte{8, 0xF7})
	waitFor(t, "output", func() bool { return len(b.got()) == 8 })
	want := []byte{0xF0, 1, 2, 0xF7, 0xF0, 9, 8, 0xF7}
	if !bytes.Equal(b.got(), want) {
		t.Fatalf("got  %x\nwant %x", b.got(), want)
	}
}

func TestStaleSysExOwnerIsReleased(t *testing.T) {
	old := sysexOwnerStale
	sysexOwnerStale = 60 * time.Millisecond
	defer func() { sysexOwnerStale = old }()

	r := newRouter()
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1, 2}) // never finished
	hc.Receive([]byte{0x90, 60, 100})
	// c's note is held back, then released once a is deemed dead; the
	// destination gets an F7 to close a's message first.
	waitFor(t, "release", func() bool { return len(b.got()) == 3+1+3 })
	want := []byte{0xF0, 1, 2, 0xF7, 0x90, 60, 100}
	if !bytes.Equal(b.got(), want) {
		t.Fatalf("got  %x\nwant %x", b.got(), want)
	}
}

func TestSourceRemovalMidSysExReleasesDestination(t *testing.T) {
	r := newRouter()
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1, 2})
	hc.Receive([]byte{0xC0, 7})
	r.RemovePort("a")
	waitFor(t, "release", func() bool { return len(b.got()) == 3+1+2 })
	want := []byte{0xF0, 1, 2, 0xF7, 0xC0, 7}
	if !bytes.Equal(b.got(), want) {
		t.Fatalf("got  %x\nwant %x", b.got(), want)
	}
}

func TestLargeSysExBackpressureIsLossless(t *testing.T) {
	r := New(Options{QueueBytes: 2048, SysExChunk: 128, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a := newPort("a", true, false)
	b := newPort("b", false, true)
	b.gate = make(chan struct{})
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})

	const size = 64 * 1024
	msg := make([]byte, 0, size+2)
	msg = append(msg, 0xF0)
	for i := 0; i < size; i++ {
		msg = append(msg, byte(i*7)&0x7F)
	}
	msg = append(msg, 0xF7)

	done := make(chan struct{})
	go func() { // the source: feeds in 1 KiB reads, like a USB read loop
		for off := 0; off < len(msg); off += 1024 {
			end := off + 1024
			if end > len(msg) {
				end = len(msg)
			}
			ha.Receive(msg[off:end])
		}
		close(done)
	}()

	// The device is slow: let it accept one chunk at a time. The source must
	// be held up by the full queue instead of data being lost.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("source finished although the destination accepted nothing: no back-pressure")
	default:
	}
	go func() {
		for {
			select {
			case b.gate <- struct{}{}:
			case <-done:
				// drain the remainder
				for i := 0; i < 1000; i++ {
					select {
					case b.gate <- struct{}{}:
					case <-time.After(200 * time.Millisecond):
						return
					}
				}
				return
			}
		}
	}()
	waitFor(t, "all bytes through", func() bool { return len(b.got()) >= len(msg) })
	if !bytes.Equal(b.got(), msg) {
		t.Fatalf("sysex corrupted or reordered (%d bytes, want %d)", len(b.got()), len(msg))
	}
	if d := r.Stats()["b"].Dropped; d != 0 {
		t.Fatalf("dropped %d", d)
	}
	for _, c := range b.chunks() {
		if len(c) > 128 {
			t.Fatalf("chunk of %d bytes exceeds SysExChunk", len(c))
		}
	}
}

func TestNonSysExDroppedWhenQueueFull(t *testing.T) {
	r := New(Options{QueueBytes: 8, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a, b := newPort("a", true, false), newPort("b", false, true)
	b.gate = make(chan struct{})
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	for i := 0; i < 100; i++ {
		ha.Receive([]byte{0xB0, 1, byte(i)})
	}
	if d := r.Stats()["b"].Dropped; d == 0 {
		t.Fatal("expected drops with a stuck destination")
	}
	close(b.gate)
}

func TestSnapshotAndStats(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", DefaultFilter())
	ha.Receive([]byte{0x90, 60, 100, 0xFE})
	waitFor(t, "stats", func() bool { return r.Stats()["b"].TxMsgs == 1 })
	s := r.Stats()
	if s["a"].RxMsgs != 2 || s["a"].RxBytes != 4 {
		t.Fatalf("rx stats %+v", s["a"])
	}
	if s["b"].TxBytes != 3 {
		t.Fatalf("tx stats %+v", s["b"])
	}
	snap := r.Snapshot()
	if len(snap.Ports) != 2 || len(snap.Connections) != 1 || !snap.Connections[0].Online {
		t.Fatalf("snapshot %+v", snap)
	}
	if got := snap.Connections[0].Filter.Block; len(got) != 1 || got[0] != "active_sense" {
		t.Fatalf("default filter %v", got)
	}
}

func TestConnectionsChangedCallback(t *testing.T) {
	var mu sync.Mutex
	var last []ConnSpec
	calls := 0
	r := New(Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnConnectionsChanged: func(c []ConnSpec) {
			mu.Lock()
			last, calls = c, calls+1
			mu.Unlock()
		},
	})
	r.AddPort(newPort("a", true, false))
	r.AddPort(newPort("b", false, true))
	mustConnect(t, r, "a", "b", Filter{Channels: []int{2, 1, 2}})
	_ = r.Disconnect("a", "b")
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || len(last) != 0 {
		t.Fatalf("calls=%d last=%v", calls, last)
	}
}

func TestChangesChannel(t *testing.T) {
	r := newRouter()
	ch := r.Changes()
	r.AddPort(newPort("a", true, true))
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no change notification")
	}
	select {
	case <-r.Changes():
		t.Fatal("fresh channel must be open")
	default:
	}
}

func TestInjectAndPanic(t *testing.T) {
	r := newRouter()
	b := newPort("b", false, true)
	r.AddPort(b)
	if err := r.Inject("nope", []byte{0xF8}); err != ErrUnknownPort {
		t.Fatal(err)
	}
	if err := r.Inject("b", []byte{0x90, 60, 100}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "inject", func() bool { return len(b.got()) == 3 })
	if err := r.Panic("b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "panic", func() bool { return len(b.got()) == 3+16*9 })
}

func TestMonitor(t *testing.T) {
	r := newRouter()
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	ch, cancel := r.SubscribeMonitor()
	defer cancel()
	ha.Receive([]byte{0x90, 60, 100})
	var rx, tx bool
	deadline := time.After(2 * time.Second)
	for !(rx && tx) {
		select {
		case e := <-ch:
			if e.Dir == "rx" && e.Port == "a" {
				rx = true
			}
			if e.Dir == "tx" && e.Port == "b" && e.From == "a" && e.Desc == "Note On ch1 C4 vel 100" {
				tx = true
			}
		case <-deadline:
			t.Fatalf("monitor incomplete rx=%v tx=%v", rx, tx)
		}
	}
	cancel()
	if r.monitorOn.Load() != 0 {
		t.Fatal("monitor still on after cancel")
	}
}

func TestFilterNormalize(t *testing.T) {
	f := Filter{Block: []string{"cc", "note", "cc"}, Channels: []int{3, 1, 3, 99}}.Normalize()
	if len(f.Block) != 2 || f.Block[0] != "note" || f.Block[1] != "cc" {
		t.Fatalf("block %v", f.Block)
	}
	if len(f.Channels) != 2 || f.Channels[0] != 1 || f.Channels[1] != 3 {
		t.Fatalf("channels %v", f.Channels)
	}
	all := make([]int, 16)
	for i := range all {
		all[i] = i + 1
	}
	if g := (Filter{Channels: all}).Normalize(); g.Channels != nil {
		t.Fatalf("all channels should normalise to none, got %v", g.Channels)
	}
}

func TestReplacePortSameID(t *testing.T) {
	r := newRouter()
	a := newPort("a", true, false)
	ha := r.AddPort(a)
	b1 := newPort("b", false, true)
	r.AddPort(b1)
	mustConnect(t, r, "a", "b", Filter{})
	b2 := newPort("b", false, true)
	r.AddPort(b2)
	ha.Receive([]byte{0xC0, 3})
	waitFor(t, "b2", func() bool { return len(b2.got()) == 2 })
	if len(b1.got()) != 0 || !b1.closed {
		t.Fatal("old port must be closed and unused")
	}
}

// ---- review fixes: regression tests ----------------------------------------

// stop removes every port, which stops the writer goroutines (tests that
// change the package-level timing variables must not leave any running).
func stop(r *Router) {
	for _, p := range r.Snapshot().Ports {
		r.RemovePort(p.ID)
	}
}

// barrier returns once everything enqueued on b so far has been written (the
// queue is FIFO; 0xFD is a realtime byte no test sends), and gives back what b
// has received, without the barrier byte.
func barrier(t *testing.T, r *Router, b *fakePort) []byte {
	t.Helper()
	before := bytes.Count(b.got(), []byte{0xFD})
	if err := r.Inject(b.info.ID, []byte{0xFD}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for bytes.Count(b.got(), []byte{0xFD}) <= before {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for barrier")
		}
		time.Sleep(50 * time.Microsecond)
	}
	return bytes.ReplaceAll(b.got(), []byte{0xFD}, nil)
}

// held returns the notes (channel<<8 | note) that the stream leaves sounding.
func held(stream []byte) map[int]bool {
	on := map[int]bool{}
	midi.NewParser().Feed(stream, func(ev midi.Event) {
		ch, ok := ev.Channel()
		if !ok || len(ev.Data) < 3 {
			return
		}
		k := ch<<8 | int(ev.Data[1])
		switch ev.Data[0] & 0xF0 {
		case 0x90:
			if ev.Data[2] != 0 {
				on[k] = true
				return
			}
			delete(on, k)
		case 0x80:
			delete(on, k)
		}
	})
	return on
}

func setTiming(t *testing.T, stale time.Duration) {
	t.Helper()
	old := sysexOwnerStale
	sysexOwnerStale = stale
	t.Cleanup(func() { sysexOwnerStale = old })
}

// R1: a live filter edit must not strand notes the old filter let through.
func TestFilterChangeReleasesHeldNotes(t *testing.T) {
	cases := []struct {
		name string
		f    Filter
	}{
		{"transpose", Filter{Transpose: 12}},
		{"channel allow-list", Filter{Channels: []int{3}}},
		{"block notes", Filter{Block: []string{"note"}}},
		{"map channel", Filter{MapChannel: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter()
			defer stop(r)
			a, b := newPort("a", true, false), newPort("b", false, true)
			ha := r.AddPort(a)
			r.AddPort(b)
			mustConnect(t, r, "a", "b", Filter{})
			ha.Receive([]byte{0x90, 60, 100, 0x91, 64, 100})
			waitFor(t, "note-ons", func() bool { return len(b.got()) == 6 })

			if err := r.SetFilter("a", "b", tc.f); err != nil {
				t.Fatal(err)
			}
			// Released by the filter change itself, with the keys still down.
			want := []byte{0x90, 60, 100, 0x91, 64, 100, 0x80, 60, 0, 0x81, 64, 0}
			waitFor(t, "release", func() bool { return len(b.got()) >= len(want) })
			if got := b.got(); !bytes.Equal(got, want) {
				t.Fatalf("got  %x\nwant %x", got, want)
			}

			// The keys come up later; whatever the new filter makes of that, the
			// destination holds nothing.
			ha.Receive([]byte{0x80, 60, 0, 0x81, 64, 0})
			if h := held(barrier(t, r, b)); len(h) != 0 {
				t.Fatalf("notes still held on the destination: %v", h)
			}
		})
	}
}

// A filter change that cannot alter a note-off leaves the held notes alone. This
// is about the notes the tracker follows: the sustain pedal is not tracked, so
// none of these cases says anything about a pedal that is down (see
// swapFilter's comment: only Panic lets go of it).
func TestFilterChangeThatLeavesNoteOffsAloneKeepsHeldNotes(t *testing.T) {
	cases := []struct {
		name string
		f    Filter
	}{
		{"same filter", Filter{}},
		{"block cc (pedal up)", Filter{Block: []string{"cc"}}},
		{"block clock and sysex", Filter{Block: []string{"sysex", "clock"}}},
		{"all channels listed", Filter{Channels: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter()
			defer stop(r)
			a, b := newPort("a", true, false), newPort("b", false, true)
			ha := r.AddPort(a)
			r.AddPort(b)
			mustConnect(t, r, "a", "b", Filter{})
			ha.Receive([]byte{0x90, 60, 100})
			waitFor(t, "note-on", func() bool { return len(b.got()) == 3 })
			if err := r.SetFilter("a", "b", tc.f); err != nil {
				t.Fatal(err)
			}
			if got := barrier(t, r, b); !bytes.Equal(got, []byte{0x90, 60, 100}) {
				t.Fatalf("a held note was cut by a filter change that does not touch notes: %x", got)
			}
			ha.Receive([]byte{0x80, 60, 0})
			if got := barrier(t, r, b); !bytes.Equal(got, []byte{0x90, 60, 100, 0x80, 60, 0}) {
				t.Fatalf("got %x", got)
			}
		})
	}
}

// R1, SysEx variant: a filter that starts blocking SysEx in the middle of a
// message must not leave the destination owned by it until the stale timer.
func TestFilterBlockingSysExMidMessageReleasesDestination(t *testing.T) {
	setTiming(t, 10*time.Second)
	r := newRouter()
	defer stop(r)
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1, 2})
	hc.Receive([]byte{0x90, 60, 100}) // waits behind a's message
	if err := r.SetFilter("a", "b", Filter{Block: []string{"sysex"}}); err != nil {
		t.Fatal(err)
	}
	ha.Receive([]byte{3, 0xF7}) // blocked now
	waitFor(t, "destination released", func() bool { return len(b.got()) == 3+1+3 })
	want := []byte{0xF0, 1, 2, 0xF7, 0x90, 60, 100}
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}

// R1 (second review): the note-offs a filter change releases must not be written
// into the middle of the same source's open SysEx: the destination would end the
// message at the 0x80 and take the rest of the data bytes for running-status
// note-offs. The message is closed with an F7 first, like Disconnect does; its
// remaining chunks are dropped.
func TestFilterChangeMidSysExClosesItBeforeTheNoteOffs(t *testing.T) {
	setTiming(t, 10*time.Second)
	cases := []struct {
		name string
		// ownerFirst: another source is in the middle of its own message at b,
		// so a's note and SysEx head are held back and a is not the owner yet.
		ownerFirst bool
		want       []byte
	}{
		{"source owns the destination", false,
			[]byte{0x90, 60, 100, 0xF0, 1, 2, 0xF7, 0x80, 60, 0}},
		{"source waits behind another message", true,
			[]byte{0xF0, 5, 6, 0xF7, 0x90, 60, 100, 0x80, 60, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter()
			defer stop(r)
			a, c := newPort("a", true, false), newPort("c", true, false)
			b := newPort("b", false, true)
			ha, hc := r.AddPort(a), r.AddPort(c)
			r.AddPort(b)
			mustConnect(t, r, "a", "b", Filter{})
			mustConnect(t, r, "c", "b", Filter{})

			if tc.ownerFirst {
				hc.Receive([]byte{0xF0, 5})
				waitFor(t, "c owns b", func() bool { return len(b.got()) == 2 })
			}
			ha.Receive([]byte{0x90, 60, 100, 0xF0, 1, 2}) // a holds a note and is mid-SysEx
			if !tc.ownerFirst {
				waitFor(t, "a owns b", func() bool { return len(b.got()) == 6 })
			}
			if err := r.SetFilter("a", "b", Filter{Transpose: 12}); err != nil {
				t.Fatal(err)
			}
			ha.Receive([]byte{3, 4, 0xF7}) // the rest of a's message
			if tc.ownerFirst {
				hc.Receive([]byte{6, 0xF7})
			}
			waitFor(t, "output", func() bool { return len(b.got()) >= len(tc.want) })
			if got := barrier(t, r, b); !bytes.Equal(got, tc.want) {
				t.Fatalf("got  %x\nwant %x", got, tc.want)
			}
			if h := held(b.got()); len(h) != 0 {
				t.Fatalf("notes still held: %v", h)
			}
		})
	}
}

// R5 (second review): note-offs released by Disconnect, RemovePort or a filter
// change are the only copy of those note-offs (the tracker has forgotten them),
// so a full destination queue must not drop them. Here the queue is full of
// another source's SysEx dump, the documented burst case.
func TestReleasedNoteOffsAreNotDroppedByAFullQueue(t *testing.T) {
	old := sysexEnqueueWait
	sysexEnqueueWait = 30 * time.Second // the dump waits for room as long as the test needs
	t.Cleanup(func() { sysexEnqueueWait = old })
	setTiming(t, 30*time.Second)

	for _, how := range []string{"disconnect", "remove port", "filter change"} {
		t.Run(how, func(t *testing.T) {
			r := New(Options{QueueBytes: 64, SysExChunk: 16, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			defer stop(r)
			k, n := newPort("k", true, false), newPort("n", true, false)
			b := newPort("b", false, true)
			b.gate = make(chan struct{})
			hk, hn := r.AddPort(k), r.AddPort(n)
			r.AddPort(b)
			mustConnect(t, r, "k", "b", Filter{})
			mustConnect(t, r, "n", "b", Filter{})
			var once sync.Once
			wake := func() { once.Do(func() { close(b.gate) }) } // the device wakes up
			defer wake()                                         // whatever happens, so that stop() finds no stuck writer

			hk.Receive([]byte{0x90, 60, 100}) // the keyboard holds middle C on b
			select {
			case b.gate <- struct{}{}:
			case <-time.After(3 * time.Second):
				t.Fatal("the note-on never reached the device")
			}
			waitFor(t, "note-on written", func() bool { return len(b.got()) == 3 })

			// A network client sends a dump; b accepts nothing, so b's queue
			// fills up to its budget and the client is held back.
			dump := []byte{0xF0}
			for i := 1; i <= 100; i++ {
				dump = append(dump, byte(i))
			}
			dump = append(dump, 0xF7)
			done := make(chan struct{})
			go func() { hn.Receive(dump); close(done) }()
			waitFor(t, "b's queue full", func() bool { return r.Stats()["b"].QueuedBytes == 64 })
			time.Sleep(20 * time.Millisecond) // the next chunk has no room: the client waits
			select {
			case <-done:
				t.Fatal("the dump was not held up: the test set-up is wrong")
			default:
			}

			switch how {
			case "disconnect":
				if err := r.Disconnect("k", "b"); err != nil {
					t.Fatal(err)
				}
			case "remove port":
				r.RemovePort("k")
			case "filter change":
				if err := r.SetFilter("k", "b", Filter{Transpose: 12}); err != nil {
					t.Fatal(err)
				}
			}
			if d := r.Stats()["b"].Dropped; d != 0 {
				t.Fatalf("%d event(s) dropped: the release note-off was refused by the full queue", d)
			}

			wake()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the dump never finished")
			}
			// The note-off waits behind the dump (n owns b while it runs).
			want := append(append([]byte{0x90, 60, 100}, dump...), 0x80, 60, 0)
			waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
			if got := barrier(t, r, b); !bytes.Equal(got, want) {
				t.Fatalf("got  %x\nwant %x", got, want)
			}
			if h := held(b.got()); len(h) != 0 {
				t.Fatalf("notes stuck on the destination: %v", h)
			}
		})
	}
}

// R5 (second review): the same for the list of events held back behind another
// source's SysEx: it is capped, but a released note-off is exempt from the cap.
func TestReleasedNoteOffsAreNotDroppedByDeferralOverflow(t *testing.T) {
	old := maxDeferred
	maxDeferred = 2
	t.Cleanup(func() { maxDeferred = old })
	setTiming(t, 10*time.Second)

	r := newRouter()
	defer stop(r)
	k, n := newPort("k", true, false), newPort("n", true, false)
	b := newPort("b", false, true)
	hk, hn := r.AddPort(k), r.AddPort(n)
	r.AddPort(b)
	mustConnect(t, r, "k", "b", Filter{})
	mustConnect(t, r, "n", "b", Filter{})

	hk.Receive([]byte{0x90, 60, 100, 0x90, 61, 100, 0x90, 62, 100, 0x90, 63, 100, 0x90, 64, 100})
	waitFor(t, "five notes held", func() bool { return len(b.got()) == 15 })
	hn.Receive([]byte{0xF0, 1, 2}) // n owns b: everything else waits
	waitFor(t, "n owns b", func() bool { return len(b.got()) == 18 })
	if err := r.Disconnect("k", "b"); err != nil { // five note-offs, room for two
		t.Fatal(err)
	}
	hn.Receive([]byte{3, 0xF7})

	want := []byte{
		0x90, 60, 100, 0x90, 61, 100, 0x90, 62, 100, 0x90, 63, 100, 0x90, 64, 100,
		0xF0, 1, 2, 3, 0xF7,
		0x80, 60, 0, 0x80, 61, 0, 0x80, 62, 0, 0x80, 63, 0, 0x80, 64, 0,
	}
	waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
	if d := r.Stats()["b"].Dropped; d != 0 {
		t.Fatalf("dropped %d", d)
	}
}

// R2: a SysEx that cannot be held back must vanish as a whole, never arrive with
// a hole in it; messages of the same source that were complete are unharmed.
func TestDeferredOverflowDropsWholeSysEx(t *testing.T) {
	old := maxDeferred
	maxDeferred = 8
	defer func() { maxDeferred = old }()

	r := newRouter()
	defer stop(r)
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1})       // a owns b
	hc.Receive([]byte{0xF0, 7, 0xF7}) // complete message, held back (1 slot)
	hc.Receive([]byte{0xF0})          // a long one, in tiny reads
	for i := 0; i < 12; i++ {
		hc.Receive([]byte{1})
	}
	ha.Receive([]byte{2, 0xF7})       // a is done: held-back traffic is released
	hc.Receive([]byte{3, 0xF7})       // end of the dropped message
	hc.Receive([]byte{0xF0, 5, 0xF7}) // the next one is fine again

	want := []byte{0xF0, 1, 2, 0xF7, 0xF0, 7, 0xF7, 0xF0, 5, 0xF7}
	waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
	// overflowing chunk + 7 purged + 5 arriving later + the closing one
	if d := r.Stats()["b"].Dropped; d != 14 {
		t.Fatalf("dropped %d, want 14", d)
	}
}

// R2: held-back data is bounded in bytes too (here by QueueBytes), and counted
// in queued_bytes.
func TestDeferredBytesAreBoundedAndCounted(t *testing.T) {
	r := New(Options{QueueBytes: 64, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer stop(r)
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1})
	waitFor(t, "a owns b", func() bool { return len(b.got()) == 2 })
	hc.Receive([]byte{0xF0})
	hc.Receive([]byte{1, 2, 3, 4})
	hc.Receive([]byte{1, 2, 3, 4})
	waitFor(t, "held back bytes in the stats", func() bool { return r.Stats()["b"].QueuedBytes == 1+4+4 })

	for i := 0; i < 40; i++ { // 160 bytes: far over the 64 byte budget
		hc.Receive([]byte{1, 2, 3, 4})
	}
	ha.Receive([]byte{2, 0xF7})
	hc.Receive([]byte{0xF7})
	hc.Receive([]byte{0xF0, 5, 0xF7})
	want := []byte{0xF0, 1, 2, 0xF7, 0xF0, 5, 0xF7}
	waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
	if q := r.Stats()["b"].QueuedBytes; q != 0 {
		t.Fatalf("queued_bytes %d after everything was written or dropped", q)
	}
}

// R3: Disconnect closes the connection's open SysEx at the destination at once
// (before releasing its notes), like RemovePort does.
func TestDisconnectMidSysExReleasesDestination(t *testing.T) {
	setTiming(t, 10*time.Second)
	r := newRouter()
	defer stop(r)
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0x90, 60, 100, 0xF0, 1, 2}) // a holds a note and is mid-SysEx
	waitFor(t, "a owns b", func() bool { return len(b.got()) == 6 })
	if err := r.Disconnect("a", "b"); err != nil {
		t.Fatal(err)
	}
	hc.Receive([]byte{0xC0, 7})
	want := []byte{0x90, 60, 100, 0xF0, 1, 2, 0xF7, 0x80, 60, 0, 0xC0, 7}
	waitFor(t, "destination released", func() bool { return len(b.got()) >= len(want) })
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
}

// R3: the unfinished message of a source that goes away while it waits behind
// another source's message is thrown away, not flushed later.
func TestGoneSourceDeferredSysExIsNotFlushed(t *testing.T) {
	for _, how := range []string{"disconnect", "remove port"} {
		t.Run(how, func(t *testing.T) {
			setTiming(t, 10*time.Second)
			r := newRouter()
			defer stop(r)
			a, c := newPort("a", true, false), newPort("c", true, false)
			b := newPort("b", false, true)
			ha, hc := r.AddPort(a), r.AddPort(c)
			r.AddPort(b)
			mustConnect(t, r, "a", "b", Filter{})
			mustConnect(t, r, "c", "b", Filter{})

			hc.Receive([]byte{0xF0, 5}) // c owns b
			ha.Receive([]byte{0xF0, 1, 2})
			waitFor(t, "c owns b", func() bool { return len(b.got()) == 2 })
			if how == "disconnect" {
				if err := r.Disconnect("a", "b"); err != nil {
					t.Fatal(err)
				}
			} else {
				r.RemovePort("a")
			}
			hc.Receive([]byte{6, 0xF7, 0x90, 60, 100})
			want := []byte{0xF0, 5, 6, 0xF7, 0x90, 60, 100}
			waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
			if got := barrier(t, r, b); !bytes.Equal(got, want) {
				t.Fatalf("got  %x\nwant %x", got, want)
			}
		})
	}
}

// R4: OnConnectionsChanged is delivered in order and the last call carries the
// latest state, even if an earlier call is slow. Two things are pinned: the
// calls are serialised (without persistMu the slow first call would deliver its
// stale list last), and the snapshot is taken after the lock was won, not
// before (a caller queued behind a slow one would otherwise deliver a list that
// is older than what the slow one saw). For the second, the slow callback
// changes the router while the second caller is waiting for the lock; only a
// snapshot taken after getting the lock can contain that change.
func TestConnectionsChangedNeverEndsOnStaleList(t *testing.T) {
	var mu sync.Mutex
	var last []ConnSpec
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	var r *Router
	r = New(Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnConnectionsChanged: func(c []ConnSpec) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				close(entered)
				<-release // a slow store write
				// The second caller is queued behind persistMu by now. The router
				// changes behind its back.
				r.mu.Lock()
				r.conns[connKey("x", "y")] = &conn{spec: ConnSpec{From: "x", To: "y"}}
				r.mu.Unlock()
			}
			mu.Lock()
			last = c
			mu.Unlock()
		},
	})
	defer stop(r)
	r.AddPort(newPort("a", true, false))
	r.AddPort(newPort("b", false, true))
	r.AddPort(newPort("c", false, true))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = r.Connect("a", "b", Filter{}) }()
	<-entered // the first change is being delivered
	go func() { defer wg.Done(); _ = r.Connect("a", "c", Filter{}) }()
	time.Sleep(100 * time.Millisecond) // let the second one get as far as it can
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if n := len(r.Connections()); n != 3 || len(last) != 3 {
		t.Fatalf("router has %d connections but the last delivery carried %d (want 3: the list was not taken after the lock, or the calls overlap)",
			n, len(last))
	}
}

// R5: events still being routed from a port that has been removed are not
// forwarded after its held notes were released.
func TestRemovedSourceInFlightEventsAreNotRouted(t *testing.T) {
	// Room for one 8 byte chunk and a note, not for two chunks.
	r := New(Options{QueueBytes: 12, SysExChunk: 8, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer stop(r)
	a, b := newPort("a", true, false), newPort("b", false, true)
	b.gate = make(chan struct{})
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})

	// One read buffer: a long SysEx, then a note. The SysEx fills the queue of
	// the stalled destination, so the source's goroutine waits in the middle of
	// the buffer.
	buf := []byte{0xF0}
	for i := 1; i <= 30; i++ {
		buf = append(buf, byte(i))
	}
	buf = append(buf, 0x90, 60, 100)
	done := make(chan struct{})
	go func() { ha.Receive(buf); close(done) }()
	waitFor(t, "first chunk queued", func() bool { return r.Stats()["b"].QueuedBytes == 8 })
	time.Sleep(50 * time.Millisecond) // the second chunk does not fit: the source waits for room
	select {
	case <-done:
		t.Fatal("the source was not held up: the test set-up is wrong")
	default:
	}

	r.RemovePort("a") // unplugged mid-buffer
	close(b.gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receive never finished")
	}
	got := barrier(t, r, b)
	if bytes.Contains(got, []byte{0x90, 60, 100}) {
		t.Fatalf("a note from a removed port reached the destination after the release: %x", got)
	}
}

// R5: a dispatch that still holds a connection from before Disconnect (it had
// loaded the list of routes) forwards nothing.
func TestDisconnectedConnectionForwardsNothing(t *testing.T) {
	r := newRouter()
	defer stop(r)
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	r.mu.RLock()
	stale := r.ports["a"].routes.Load() // what an in-flight dispatch holds
	r.mu.RUnlock()

	if err := r.Disconnect("a", "b"); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	r.ports["a"].routes.Store(stale) // the in-flight dispatch carries on with it
	r.mu.RUnlock()
	ha.Receive([]byte{0x90, 60, 100})
	if got := barrier(t, r, b); len(got) != 0 {
		t.Fatalf("forwarded over a removed connection: %x", got)
	}
}

// R5: enqueue and observe are one step relative to release. A Disconnect that
// arrives while forward() is in the middle of that step must wait for it, then
// release the note. The test holds forward() inside its critical section by
// holding the destination queue's mutex (enqueue needs it) and starts the
// Disconnect while it is stuck there. If forward() let go of the connection's
// mutex between the enqueue and the observe, the Disconnect would find nothing
// to release and the note-on that follows would hang on the destination.
func TestDisconnectWaitsForAForwardInFlight(t *testing.T) {
	r := newRouter()
	defer stop(r)
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	r.mu.RLock()
	q := r.ports["b"].out
	r.mu.RUnlock()

	var once sync.Once
	unblock := func() { once.Do(q.mu.Unlock) }
	q.mu.Lock()
	defer unblock() // whatever happens, so that stop() finds no stuck goroutine

	recvDone := make(chan struct{})
	go func() { ha.Receive([]byte{0x90, 60, 100}); close(recvDone) }()
	time.Sleep(50 * time.Millisecond) // the receiver is stuck in enqueue, inside forward()
	discDone := make(chan struct{})
	go func() { _ = r.Disconnect("a", "b"); close(discDone) }()
	time.Sleep(50 * time.Millisecond) // the Disconnect runs as far as it can
	unblock()

	for _, c := range []chan struct{}{recvDone, discDone} {
		select {
		case <-c:
		case <-time.After(3 * time.Second):
			t.Fatal("receive or disconnect never finished")
		}
	}
	if h := held(barrier(t, r, b)); len(h) != 0 {
		t.Fatalf("notes stuck on the destination: %v", h)
	}
}

// R5: whatever the interleaving of events and Disconnect, no note is left
// sounding on the destination. This is a stress test: it only bites when the
// receiving goroutine and Disconnect really run in parallel, so it needs more
// than one CPU (TestDisconnectWaitsForAForwardInFlight pins the same property
// deterministically, on any machine).
func TestNoStuckNotesWhenDisconnectingWhileNotesFlow(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs at least two CPUs to interleave")
	}
	if p := runtime.GOMAXPROCS(0); p < 4 {
		runtime.GOMAXPROCS(4)
		t.Cleanup(func() { runtime.GOMAXPROCS(p) })
	}
	r := newRouter()
	defer stop(r)
	a, b := newPort("a", true, false), newPort("b", false, true)
	ha := r.AddPort(a)
	r.AddPort(b)
	const rounds = 2000
	for i := 0; i < rounds; i++ {
		mustConnect(t, r, "a", "b", Filter{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ha.Receive([]byte{0x90, 60, 100, 0x91, 61, 100, 0x92, 62, 100})
		}()
		runtime.Gosched()
		if err := r.Disconnect("a", "b"); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if h := held(barrier(t, r, b)); len(h) != 0 {
			t.Fatalf("round %d: notes stuck on the destination: %v", i, h)
		}
		b.mu.Lock()
		b.writes = nil
		b.mu.Unlock()
	}
}

// R6: a device that is slow to accept a chunk is not a silent owner.
func TestSlowWriteDoesNotMakeSysExOwnerStale(t *testing.T) {
	setTiming(t, 100*time.Millisecond)
	r := newRouter()
	defer stop(r)
	a, c := newPort("a", true, false), newPort("c", true, false)
	b := newPort("b", false, true)
	b.gate = make(chan struct{})
	ha, hc := r.AddPort(a), r.AddPort(c)
	r.AddPort(b)
	mustConnect(t, r, "a", "b", Filter{})
	mustConnect(t, r, "c", "b", Filter{})

	ha.Receive([]byte{0xF0, 1, 2})    // the writer sticks in the device's Write
	hc.Receive([]byte{0x90, 60, 100}) // queued behind it ...
	ha.Receive([]byte{3, 4, 0xF7})    // ... and a's next chunk right behind that
	time.Sleep(3 * sysexOwnerStale)   // the device needs far longer than the stale limit
	close(b.gate)

	want := []byte{0xF0, 1, 2, 3, 4, 0xF7, 0x90, 60, 100}
	waitFor(t, "output", func() bool { return len(b.got()) >= len(want) })
	if got := barrier(t, r, b); !bytes.Equal(got, want) {
		t.Fatalf("got  %x\nwant %x", got, want)
	}
	if d := r.Stats()["b"].Dropped; d != 0 {
		t.Fatalf("dropped %d", d)
	}
}
