//go:build darwin && cgo

package device

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emm/internal/router"
)

// tlog routes slog output into t.Log. Everything that logs must have stopped
// before the test returns.
type tlog struct{ t *testing.T }

func (w tlog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// recPort is a router destination that records what it is sent.
type recPort struct {
	id  string
	mu  sync.Mutex
	got []byte
}

func (p *recPort) Info() router.PortInfo {
	return router.PortInfo{ID: p.id, Name: p.id, Kind: router.KindVirtual, Output: true}
}
func (p *recPort) Write(b []byte) error {
	p.mu.Lock()
	p.got = append(p.got, b...)
	p.mu.Unlock()
	return nil
}
func (p *recPort) Close() error { return nil }
func (p *recPort) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.got...)
}

// feedPort is a router source the test pushes bytes into.
type feedPort struct{ id string }

func (p *feedPort) Info() router.PortInfo {
	return router.PortInfo{ID: p.id, Name: p.id, Kind: router.KindVirtual, Input: true}
}
func (p *feedPort) Write([]byte) error { return fmt.Errorf("feedPort is input only") }
func (p *feedPort) Close() error       { return nil }

func testSysEx(n int) []byte {
	b := make([]byte, 0, n+2)
	b = append(b, 0xF0)
	for i := 0; i < n; i++ {
		b = append(b, byte(i*7+3)&0x7F)
	}
	return append(b, 0xF7)
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for: %s", d, what)
}

func TestCoreMIDIEnumerate(t *testing.T) {
	infos, err := ListCoreMIDI()
	if err != nil {
		t.Skipf("CoreMIDI not usable here: %v", err)
	}
	t.Logf("%d CoreMIDI port(s) visible", len(infos))
	seen := map[string]bool{}
	for _, i := range infos {
		t.Logf("  %-22s %-8s in=%-5v out=%-5v %q group=%q (%s)", i.ID, i.Kind, i.Input, i.Output, i.Name, i.Group, i.Detail)
		if !cmIDPattern.MatchString(i.ID) {
			t.Errorf("id %q is not router-safe", i.ID)
		}
		if seen[i.ID] {
			t.Errorf("duplicate id %q", i.ID)
		}
		seen[i.ID] = true
		if !i.Input && !i.Output {
			t.Errorf("%q has neither input nor output", i.ID)
		}
	}
}

func TestCoreMIDILoopback(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()

	r := router.New(router.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := NewCoreMIDI(CoreMIDIOptions{
		IncludeVirtual: true,
		Logger:         slog.New(slog.NewTextHandler(tlog{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	runErr := make(chan error, 1)
	go func() { runErr <- be.Run(ctx, r) }()

	// If the MIDI server cannot be reached (restricted sandbox, no permission),
	// Run fails immediately: skip rather than fail.
	select {
	case err := <-runErr:
		t.Skipf("CoreMIDI backend cannot run in this environment: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	defer stop()

	name := fmt.Sprintf("EMM-test-%d", os.Getpid())
	v, err := newCMVirtual(name)
	if err != nil {
		stop()
		t.Skipf("cannot create virtual MIDI endpoints in this environment: %v", err)
	}
	defer v.Close()

	findPort := func() (router.PortState, bool) {
		for _, p := range r.Snapshot().Ports {
			if p.Name == name {
				return p, true
			}
		}
		return router.PortState{}, false
	}

	// 1. Hot-add: the endpoints were created *after* the backend started, so the
	//    port appears through a CoreMIDI notification or, failing that, the
	//    once-a-second rescan (both are within the 5 s allowed here).
	eventually(t, 5*time.Second, "virtual endpoints to appear as a router port", func() bool {
		_, ok := findPort()
		return ok
	})
	port, _ := findPort()
	if port.Kind != router.KindVirtual || !port.Input || !port.Output {
		t.Fatalf("unexpected port %+v", port.PortInfo)
	}
	if !cmIDPattern.MatchString(port.ID) || !strings.HasPrefix(port.ID, "virtual:cm-") {
		t.Fatalf("unexpected id %q", port.ID)
	}

	// 2. router -> CoreMIDI -> virtual destination, with a multi-KB SysEx.
	feed := &feedPort{id: "test-feed"}
	hf := r.AddPort(feed)
	if err := r.Connect(feed.id, port.ID, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	sysex := testSysEx(3000)
	out := append(append([]byte{0x90, 60, 100}, sysex...), 0x80, 60, 0)
	hf.Receive(out)
	eventually(t, 5*time.Second, "bytes to reach the virtual destination", func() bool {
		return len(v.Received()) >= len(out)
	})
	if got := v.Received(); !bytes.Equal(got, out) {
		t.Fatalf("router->CoreMIDI corrupted data: got %d bytes, want %d\n got  %.40x…\n want %.40x…", len(got), len(out), got, out)
	}

	// 3. virtual source -> CoreMIDI -> router -> sink, SysEx included.
	sink := &recPort{id: "test-sink"}
	r.AddPort(sink)
	if err := r.Connect(port.ID, sink.id, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	in := append(append([]byte{0xB0, 7, 99, 0xC1, 5}, testSysEx(4000)...), 0xE0, 0, 64)
	if err := v.Emit(in); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "bytes to reach the router sink", func() bool {
		return len(sink.bytes()) >= len(in)
	})
	if got := sink.bytes(); !bytes.Equal(got, in) {
		t.Fatalf("CoreMIDI->router corrupted data: got %d bytes, want %d\n got  %.40x…\n want %.40x…", len(got), len(in), got, in)
	}
	if s := r.Stats()[port.ID]; s.RxBytes != uint64(len(in)) || s.TxBytes != uint64(len(out)) {
		t.Errorf("stats rx=%d tx=%d, want rx=%d tx=%d", s.RxBytes, s.TxBytes, len(in), len(out))
	}

	// 4. Hot-remove: disposing the endpoints removes the port again.
	v.Close()
	eventually(t, 5*time.Second, "port to disappear after the endpoints were disposed", func() bool {
		_, ok := findPort()
		return !ok
	})

	// 5. Clean shutdown leaves no router ports and no goroutines behind.
	stop()
	for _, p := range r.Snapshot().Ports {
		if strings.Contains(p.ID, ":cm-") {
			t.Errorf("CoreMIDI port %q survived shutdown", p.ID)
		}
	}
	r.RemovePort(feed.id)
	r.RemovePort(sink.id)
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseGoroutines+1 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Errorf("goroutine leak: %d now vs %d before\n%s", n, baseGoroutines, buf)
	}
}

func TestCoreMIDIRunTwiceFails(t *testing.T) {
	be := NewCoreMIDI(CoreMIDIOptions{})
	cm := be.(*coreMIDI)
	cm.started.Store(true)
	if err := be.Run(context.Background(), router.New(router.Options{})); err == nil {
		t.Fatal("second Run must fail")
	}
}

// countHost wraps a router and counts what the backend does to it, per port
// name (AddPort) and per ID (RemovePort).
type countHost struct {
	router.Host
	mu      sync.Mutex
	adds    map[string][]router.PortInfo
	removes map[string]int
}

func newCountHost(h router.Host) *countHost {
	return &countHost{Host: h, adds: map[string][]router.PortInfo{}, removes: map[string]int{}}
}

func (h *countHost) AddPort(p router.Port) *router.Handle {
	info := p.Info()
	h.mu.Lock()
	h.adds[info.Name] = append(h.adds[info.Name], info)
	h.mu.Unlock()
	return h.Host.AddPort(p)
}

func (h *countHost) RemovePort(id string) {
	h.mu.Lock()
	h.removes[id]++
	h.mu.Unlock()
	h.Host.RemovePort(id)
}

func (h *countHost) addsOf(name string) []router.PortInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]router.PortInfo(nil), h.adds[name]...)
}

func (h *countHost) removesOf(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.removes[id]
}

// A source that cannot be connected must not make the port appear and vanish
// again on every rescan, and must not take the working destination with it:
// the port is published once, as output-only.
func TestCoreMIDIFailedSourceConnectDegradesToOutputOnly(t *testing.T) {
	name := fmt.Sprintf("EMM-test-failconnect-%d", os.Getpid())
	v, err := newCMVirtual(name)
	if err != nil {
		t.Skipf("cannot create virtual MIDI endpoints in this environment: %v", err)
	}
	defer v.Close()

	// Make MIDIPortConnectSource fail for the virtual device's source only. The
	// variable is set before Run starts and restored after it has returned.
	real := cmConnectSource
	failing := v.src
	var failed atomic.Int32
	cmConnectSource = func(inPort, src uint32, token uintptr) int {
		if src == failing {
			failed.Add(1)
			return -10834 // kMIDIUnknownEndpoint
		}
		return real(inPort, src, token)
	}
	defer func() { cmConnectSource = real }()

	r := router.New(router.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	host := newCountHost(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := NewCoreMIDI(CoreMIDIOptions{
		IncludeVirtual: true,
		Logger:         slog.New(slog.NewTextHandler(tlog{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	runErr := make(chan error, 1)
	go func() { runErr <- be.Run(ctx, host) }()
	stop := func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	select {
	case err := <-runErr:
		t.Skipf("CoreMIDI backend cannot run in this environment: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	defer stop() // runs before the deferred restore above

	eventually(t, 5*time.Second, "the failing port to be published", func() bool {
		return len(host.addsOf(name)) > 0
	})
	// Let several one-second rescans go by: a port that is dropped on failure
	// would be added and removed again each time.
	time.Sleep(2500 * time.Millisecond)

	adds := host.addsOf(name)
	if len(adds) != 1 {
		t.Fatalf("port was added %d times; it must be published exactly once", len(adds))
	}
	if adds[0].Input || !adds[0].Output {
		t.Fatalf("port published as input=%v output=%v, want output-only", adds[0].Input, adds[0].Output)
	}
	var id string
	for _, p := range r.Snapshot().Ports {
		if p.Name == name {
			id = p.ID
			if p.Input || !p.Output {
				t.Errorf("router has input=%v output=%v", p.Input, p.Output)
			}
		}
	}
	if id == "" {
		t.Fatal("port is not in the router")
	}
	if n := host.removesOf(id); n != 0 {
		t.Fatalf("port was removed %d times while the device was still present", n)
	}
	if failed.Load() != 1 {
		t.Fatalf("source connect was attempted %d times, want exactly 1", failed.Load())
	}

	// The destination still works.
	feed := &feedPort{id: "test-feed-failconnect"}
	hf := r.AddPort(feed)
	defer r.RemovePort(feed.id)
	if err := r.Connect(feed.id, id, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	msg := []byte{0x90, 60, 100}
	hf.Receive(msg)
	eventually(t, 5*time.Second, "bytes to reach the virtual destination", func() bool {
		return len(v.Received()) >= len(msg)
	})
	if got := v.Received(); !bytes.Equal(got, msg) {
		t.Fatalf("got %x want %x", got, msg)
	}
}
