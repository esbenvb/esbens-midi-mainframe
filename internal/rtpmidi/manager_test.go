package rtpmidi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emm/internal/router"
)

// recHost is a real router that also records how the Manager used it.
type recHost struct {
	*router.Router
	mu      sync.Mutex
	added   []string
	removed []string
}

func newRecHost() *recHost {
	return &recHost{Router: router.New(router.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})}
}

func (h *recHost) AddPort(p router.Port) *router.Handle {
	h.mu.Lock()
	h.added = append(h.added, p.Info().ID+"|"+p.Info().Name)
	h.mu.Unlock()
	return h.Router.AddPort(p)
}

func (h *recHost) RemovePort(id string) {
	h.mu.Lock()
	h.removed = append(h.removed, id)
	h.mu.Unlock()
	h.Router.RemovePort(id)
}

func (h *recHost) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.added), len(h.removed)
}

type recAnnouncer struct {
	mu    sync.Mutex
	calls []string
	live  map[string]string
	fail  atomic.Bool
}

func newRecAnnouncer() *recAnnouncer { return &recAnnouncer{live: map[string]string{}} }

func (a *recAnnouncer) Announce(id, name string, port int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "announce "+id+" "+name)
	if a.fail.Load() {
		return errors.New("avahi is not running")
	}
	a.live[id] = name
	return nil
}
func (a *recAnnouncer) Withdraw(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "withdraw "+id)
	delete(a.live, id)
}
func (a *recAnnouncer) Close() {}
func (a *recAnnouncer) snapshot() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for k, v := range a.live {
		out[k] = v
	}
	return out
}

// memPort is a router port that records what it is sent (a stand-in synth).
type memPort struct {
	info router.PortInfo
	mu   sync.Mutex
	got  []byte
}

func newMemPort(id string, in, out bool) *memPort {
	return &memPort{info: router.PortInfo{ID: id, Name: id, Kind: router.KindVirtual, Input: in, Output: out}}
}
func (p *memPort) Info() router.PortInfo { return p.info }
func (p *memPort) Write(b []byte) error {
	p.mu.Lock()
	p.got = append(p.got, b...)
	p.mu.Unlock()
	return nil
}
func (p *memPort) Close() error { return nil }
func (p *memPort) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.got...)
}

func newManager(t *testing.T, h router.Host, a Announcer, onChange func()) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{Host: h, Announcer: a, BindAddr: testBind, Logger: quietLogger(), OnChange: onChange})
	t.Cleanup(m.Close)
	return m
}

func portByID(snap router.Snapshot, id string) (router.PortState, bool) {
	for _, p := range snap.Ports {
		if p.ID == id {
			return p, true
		}
	}
	return router.PortState{}, false
}

func udpPortFree(port int) bool {
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestManagerSyncLifecycle(t *testing.T) {
	host, ann := newRecHost(), newRecAnnouncer()
	var changes atomic.Int32
	m := newManager(t, host, ann, func() { changes.Add(1) })

	specs := []Spec{{ID: "rtp:usb:1:a", Name: "Digitakt"}, {ID: "rtp:usb:2:b", Name: "DX7"}}
	if err := m.Sync(specs); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if len(st) != 2 || st[0].ID != "rtp:usb:1:a" || st[1].ID != "rtp:usb:2:b" || st[0].ControlPort == 0 || st[0].ControlPort == st[1].ControlPort {
		t.Fatalf("status %+v", st)
	}
	snap := host.Snapshot()
	for _, s := range st {
		p, ok := portByID(snap, s.ID)
		if !ok || p.Kind != router.KindRTP || !p.Input || !p.Output || p.Name != s.Name ||
			!strings.Contains(p.Detail, "UDP") || !strings.Contains(p.Detail, strconv.Itoa(s.ControlPort)) {
			t.Fatalf("router port %+v for %+v", p, s)
		}
	}
	if got := ann.snapshot(); got["rtp:usb:1:a"] != "Digitakt" || got["rtp:usb:2:b"] != "DX7" {
		t.Fatalf("announced %v", got)
	}
	if changes.Load() == 0 {
		t.Fatal("OnChange not called by Sync")
	}

	// Idempotent: nothing is re-added, nothing restarts.
	a0, r0 := host.counts()
	if err := m.Sync(specs); err != nil {
		t.Fatal(err)
	}
	if a1, r1 := host.counts(); a1 != a0 || r1 != r0 {
		t.Fatalf("idempotent Sync touched the router: +%d -%d", a1-a0, r1-r0)
	}
	// ControlPort 0 means "keep what you have".
	if got := m.Status(); got[0].ControlPort != st[0].ControlPort {
		t.Fatal("port changed on a no-op Sync")
	}

	// Rename: same port, new name in router and announcement.
	specs[0].Name = "Digitakt II"
	if err := m.Sync(specs); err != nil {
		t.Fatal(err)
	}
	if p, _ := portByID(host.Snapshot(), "rtp:usb:1:a"); p.Name != "Digitakt II" {
		t.Fatalf("router name %q", p.Name)
	}
	if got := ann.snapshot()["rtp:usb:1:a"]; got != "Digitakt II" {
		t.Fatalf("announced name %q", got)
	}
	if got := m.Status(); got[0].Name != "Digitakt II" || got[0].ControlPort != st[0].ControlPort {
		t.Fatalf("status after rename %+v", got[0])
	}

	// Remove one: router port gone, announcement withdrawn, UDP port released.
	gone := st[1].ControlPort
	if err := m.Sync(specs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, ok := portByID(host.Snapshot(), "rtp:usb:2:b"); ok {
		t.Fatal("router port not removed")
	}
	if _, ok := ann.snapshot()["rtp:usb:2:b"]; ok {
		t.Fatal("announcement not withdrawn")
	}
	if !udpPortFree(gone) || !udpPortFree(gone+1) {
		t.Fatal("server sockets still open after removal")
	}
	if len(m.Status()) != 1 {
		t.Fatal("status still lists the removed endpoint")
	}

	// Explicit port change recreates the endpoint on the requested port.
	probe := NewServer(Config{Name: "probe", BindAddr: testBind, Logger: quietLogger()}, Handler{})
	if err := probe.Start(); err != nil {
		t.Fatal(err)
	}
	want, _ := probe.Ports()
	probe.Close()
	specs[0].ControlPort = want
	if err := m.Sync(specs); err != nil {
		t.Fatal(err)
	}
	if got := m.Status(); got[0].ControlPort != want {
		t.Fatalf("control port %d want %d", got[0].ControlPort, want)
	}

	m.Close()
	if len(host.Snapshot().Ports) != 0 {
		t.Fatal("Close left router ports behind")
	}
	if len(ann.snapshot()) != 0 {
		t.Fatal("Close left announcements behind")
	}
	if err := m.Sync(specs); err == nil {
		t.Fatal("Sync after Close must fail")
	}
}

func TestManagerFailureIsolation(t *testing.T) {
	host, ann := newRecHost(), newRecAnnouncer()
	m := newManager(t, host, ann, nil)
	busy, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.LocalAddr().(*net.UDPAddr).Port

	err = m.Sync([]Spec{
		{ID: "rtp:good1", Name: "Good 1"},
		{ID: "rtp:bad", Name: "Bad", ControlPort: busyPort},
		{ID: "rtp:good2", Name: "Good 2"},
	})
	if err == nil || !strings.Contains(err.Error(), "rtp:bad") {
		t.Fatalf("expected an error naming the failing endpoint, got %v", err)
	}
	st := m.Status()
	if len(st) != 2 || st[0].ID != "rtp:good1" || st[1].ID != "rtp:good2" {
		t.Fatalf("good endpoints must survive: %+v", st)
	}
	if _, ok := portByID(host.Snapshot(), "rtp:bad"); ok {
		t.Fatal("failed endpoint registered a router port")
	}
	if _, ok := ann.snapshot()["rtp:bad"]; ok {
		t.Fatal("failed endpoint was announced")
	}
	// Fixing the spec on the next Sync brings it up.
	if err := m.Sync([]Spec{{ID: "rtp:good1", Name: "Good 1"}, {ID: "rtp:bad", Name: "Bad"}, {ID: "rtp:good2", Name: "Good 2"}}); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 3 {
		t.Fatal("recovered endpoint missing")
	}
	// Specs without an ID are ignored rather than crashing.
	if err := m.Sync([]Spec{{Name: "anonymous"}}); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 0 {
		t.Fatal("everything should have been removed")
	}
}

func TestManagerAnnounceFailureKeepsEndpoint(t *testing.T) {
	host, ann := newRecHost(), newRecAnnouncer()
	ann.fail.Store(true)
	m := newManager(t, host, ann, nil)
	err := m.Sync([]Spec{{ID: "rtp:x", Name: "X"}})
	if err == nil || !strings.Contains(err.Error(), "announce") {
		t.Fatalf("error %v", err)
	}
	if len(m.Status()) != 1 {
		t.Fatal("endpoint must keep running when only the announcement failed")
	}
	if _, ok := portByID(host.Snapshot(), "rtp:x"); !ok {
		t.Fatal("router port missing")
	}
}

func dialManaged(t *testing.T, m *Manager, id, name string, h Handler) *Client {
	t.Helper()
	for _, s := range m.Status() {
		if s.ID == id {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, Config{Name: name, Logger: quietLogger()}, "127.0.0.1", s.ControlPort, h)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			return c
		}
	}
	t.Fatalf("no endpoint %s", id)
	return nil
}

func TestManagerEndToEndThroughRouter(t *testing.T) {
	host := newRecHost()
	r := host.Router
	var peerChanges atomic.Int32
	m := newManager(t, host, nil, func() { peerChanges.Add(1) })
	if err := m.Sync([]Spec{{ID: "rtp:synth", Name: "Synth (network)"}}); err != nil {
		t.Fatal(err)
	}

	synth := newMemPort("synth", true, true) // the USB device behind the network port
	hsynth := r.AddPort(synth)
	if err := r.Connect("rtp:synth", "synth", router.DefaultFilter()); err != nil {
		t.Fatal(err)
	}
	if err := r.Connect("synth", "rtp:synth", router.DefaultFilter()); err != nil {
		t.Fatal(err)
	}

	var cliRx collector
	c := dialManaged(t, m, "rtp:synth", "MacBook", Handler{OnMIDI: cliRx.onMIDI})
	waitFor(t, "peer visible in status", func() bool { return len(m.Status()[0].Peers) == 1 })
	if m.Status()[0].Peers[0].Name != "MacBook" {
		t.Fatalf("peers %+v", m.Status()[0].Peers)
	}

	// Network -> router -> synth.
	c.Send([]byte{0x90, 60, 100})
	waitFor(t, "synth received", func() bool { return bytes.Equal(synth.bytes(), []byte{0x90, 60, 100}) })

	// Synth -> router -> network.
	hsynth.Receive([]byte{0xB0, 7, 99, 0xFE}) // active sensing is filtered by the default filter
	waitFor(t, "client received", func() bool { return bytes.Equal(cliRx.got(), []byte{0xB0, 7, 99}) })

	// A SysEx upload from the network to the synth, bit-exact.
	dump := randomSysEx(20*1024, 5)
	for _, ch := range split(dump, 256) {
		c.Send(ch)
	}
	want := append([]byte{0x90, 60, 100}, dump...)
	waitFor(t, "dump at synth", func() bool { return len(synth.bytes()) >= len(want) })
	if !bytes.Equal(synth.bytes(), want) {
		t.Fatalf("SysEx through manager+router corrupted (%d bytes vs %d)", len(synth.bytes()), len(want))
	}
	if d := r.Stats()["synth"].Dropped; d != 0 {
		t.Fatalf("router dropped %d", d)
	}

	// A peer leaving is reported.
	n := peerChanges.Load()
	c.Close()
	waitFor(t, "peer left", func() bool { return len(m.Status()[0].Peers) == 0 && peerChanges.Load() > n })

	// Removing the endpoint turns the connection dormant instead of deleting it.
	if err := m.Sync(nil); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if len(snap.Connections) != 2 || snap.Connections[0].Online || snap.Connections[1].Online {
		t.Fatalf("connections %+v", snap.Connections)
	}
	// It comes back (same ID) and the dormant connections wake up.
	if err := m.Sync([]Spec{{ID: "rtp:synth", Name: "Synth (network)"}}); err != nil {
		t.Fatal(err)
	}
	c2 := dialManaged(t, m, "rtp:synth", "iPad", Handler{})
	before := len(synth.bytes())
	c2.Send([]byte{0xC0, 3})
	waitFor(t, "traffic after the endpoint came back", func() bool { return len(synth.bytes()) == before+2 })
}

func TestManagerTwoPeersSendConcurrently(t *testing.T) {
	host := newRecHost()
	m := newManager(t, host, nil, nil)
	if err := m.Sync([]Spec{{ID: "rtp:bus", Name: "Bus"}}); err != nil {
		t.Fatal(err)
	}
	dst := newMemPort("dst", false, true)
	host.AddPort(dst)
	if err := host.Connect("rtp:bus", "dst", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	c1 := dialManaged(t, m, "rtp:bus", "one", Handler{})
	c2 := dialManaged(t, m, "rtp:bus", "two", Handler{})
	var wg sync.WaitGroup
	const n = 200
	for _, c := range []*Client{c1, c2} {
		wg.Add(1)
		go func(c *Client, ch byte) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				c.Send([]byte{0xB0 | ch, 1, byte(i % 128)})
				time.Sleep(100 * time.Microsecond)
			}
		}(c, byte(len(c.Peer().Name)%2))
	}
	wg.Wait()
	waitFor(t, "all messages delivered", func() bool { return len(dst.bytes()) >= 2*n*3 })
	if len(dst.bytes()) != 2*n*3 {
		t.Fatalf("got %d bytes, want %d", len(dst.bytes()), 2*n*3)
	}
	// Whatever the interleaving, every message is intact (status, controller 1, value).
	b := dst.bytes()
	for i := 0; i+2 < len(b); i += 3 {
		if b[i]&0xF0 != 0xB0 || b[i+1] != 1 || b[i+2] >= 128 {
			t.Fatalf("garbled message at %d: % X", i, b[i:i+3])
		}
	}
}
