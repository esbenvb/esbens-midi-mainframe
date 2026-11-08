package app

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

type fakePort struct{ info router.PortInfo }

func (p *fakePort) Info() router.PortInfo { return p.info }
func (p *fakePort) Write([]byte) error    { return nil }
func (p *fakePort) Close() error          { return nil }

// fakeRTP mimics rtpmidi.Manager: it registers a router port per spec.
type fakeRTP struct {
	r     *router.Router
	mu    sync.Mutex
	have  map[string]RTPSpec
	calls int
	taken map[int]bool // UDP ports "in use by another program"
}

func (f *fakeRTP) Sync(specs []RTPSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	want := map[string]RTPSpec{}
	for _, s := range specs {
		want[s.ID] = s
	}
	for id := range f.have {
		if _, ok := want[id]; !ok {
			f.r.RemovePort(id)
			delete(f.have, id)
		}
	}
	for id, s := range want {
		if old, ok := f.have[id]; ok && old == s {
			continue
		}
		if f.taken[s.ControlPort] {
			continue // bind failure: no port is registered
		}
		f.r.AddPort(&fakePort{router.PortInfo{ID: id, Name: s.Name, Kind: router.KindRTP, Input: true, Output: true}})
		f.have[id] = s
	}
	return nil
}

func (f *fakeRTP) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.have {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func usbPort(id, name string, in, out bool) *fakePort {
	return &fakePort{router.PortInfo{ID: id, Name: name, Kind: router.KindUSB, Group: name, Input: in, Output: out}}
}

type harness struct {
	r     *router.Router
	st    *config.Store
	rtp   *fakeRTP
	rec   *Reconciler
	on    bool
	onMu  sync.Mutex
	calls int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &harness{on: true}
	h.r = router.New(router.Options{Logger: log})
	st, err := config.Open(filepath.Join(t.TempDir(), "state.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	st.SaveDelay = time.Millisecond
	t.Cleanup(func() { _ = st.Flush() })
	h.st = st
	h.rtp = &fakeRTP{r: h.r, have: map[string]RTPSpec{}, taken: map[int]bool{}}
	h.rec = &Reconciler{Router: h.r, Store: st, RTP: h.rtp, Log: log,
		PortFree:   func(p int) bool { return !h.rtp.taken[p] && !h.rtp.taken[p+1] },
		RTPEnabled: func() bool { h.onMu.Lock(); defer h.onMu.Unlock(); return h.on },
		OnChange:   func() { h.calls++ }}
	return h
}

func (h *harness) setRTP(on bool) { h.onMu.Lock(); h.on = on; h.onMu.Unlock() }

func (h *harness) conns() map[string]bool {
	m := map[string]bool{}
	for _, c := range h.r.Connections() {
		m[c.From+" > "+c.To] = true
	}
	return m
}

func TestNewUSBPortGetsEndpointAndDefaultConnections(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.r.AddPort(usbPort("usb:3:4:1.2:0", "Keys", true, false))
	h.rec.Reconcile()

	if got := h.rtp.ids(); len(got) != 2 || got[0] != "rtp:usb:1:2:1.1:0" || got[1] != "rtp:usb:3:4:1.2:0" {
		t.Fatalf("endpoints: %v", got)
	}
	c := h.conns()
	for _, want := range []string{
		"usb:1:2:1.1:0 > rtp:usb:1:2:1.1:0", "rtp:usb:1:2:1.1:0 > usb:1:2:1.1:0",
		"usb:3:4:1.2:0 > rtp:usb:3:4:1.2:0", // input-only device: one direction
	} {
		if !c[want] {
			t.Errorf("missing connection %q (have %v)", want, c)
		}
	}
	if c["rtp:usb:3:4:1.2:0 > usb:3:4:1.2:0"] {
		t.Error("connection into an input-only device")
	}
	if len(c) != 3 {
		t.Errorf("unexpected connections: %v", c)
	}
	st := h.st.Snapshot()
	if st.Ports["usb:1:2:1.1:0"].RTPPort != 5004 || st.Ports["usb:3:4:1.2:0"].RTPPort != 5006 {
		t.Fatalf("port allocation: %+v", st.Ports)
	}
	if !st.Ports["usb:1:2:1.1:0"].RTPDefaultsDone {
		t.Fatal("defaults must be remembered")
	}
	if k := st.Known["usb:1:2:1.1:0"]; k.Name != "Digitakt" {
		t.Fatalf("known: %+v", k)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.rec.Reconcile()
	h.rec.Reconcile() // the endpoint's own port is recorded on the second pass
	before := len(h.conns())
	h.calls = 0
	for i := 0; i < 3; i++ {
		h.rec.Reconcile()
	}
	if len(h.conns()) != before {
		t.Fatal("connections changed on re-reconcile")
	}
	if h.calls != 0 {
		t.Fatalf("OnChange fired %d times for no change", h.calls)
	}
}

func TestDeletedDefaultsStayDeleted(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.rec.Reconcile()
	_ = h.r.Disconnect("usb:1:2:1.1:0", "rtp:usb:1:2:1.1:0")
	h.rec.Reconcile()
	if h.conns()["usb:1:2:1.1:0 > rtp:usb:1:2:1.1:0"] {
		t.Fatal("user-deleted default connection came back")
	}
}

func TestPortsAreStableAcrossRestarts(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))
	h.r.AddPort(usbPort("usb:3:4:1.2:0", "B", true, true))
	h.rec.Reconcile()
	snap := h.st.Snapshot()
	pa, pb := snap.Ports["usb:1:2:1.1:0"].RTPPort, snap.Ports["usb:3:4:1.2:0"].RTPPort
	if pa == pb || pa == 0 || pb == 0 {
		t.Fatalf("ports %d %d", pa, pb)
	}
	// Plug in a third device first on the next boot: existing assignments hold.
	h2 := newHarness(t)
	h2.st.Update(func(st *config.State) { *st = snap })
	h2.r.AddPort(usbPort("usb:9:9:1.3:0", "C", true, true))
	h2.r.AddPort(usbPort("usb:3:4:1.2:0", "B", true, true))
	h2.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))
	h2.rec.Reconcile()
	got := h2.st.Snapshot()
	if got.Ports["usb:1:2:1.1:0"].RTPPort != pa || got.Ports["usb:3:4:1.2:0"].RTPPort != pb {
		t.Fatalf("assignments moved: %+v", got.Ports)
	}
	if pc := got.Ports["usb:9:9:1.3:0"].RTPPort; pc == pa || pc == pb || pc == 0 {
		t.Fatalf("new device port collides: %d", pc)
	}
}

func TestEndpointSurvivesUnplugWhileReferenced(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.rec.Reconcile()
	h.r.RemovePort("usb:1:2:1.1:0")
	h.rec.Reconcile()
	if got := h.rtp.ids(); len(got) != 1 {
		t.Fatalf("network endpoint of an unplugged (but wired-up) device must stay: %v", got)
	}
	// With its connections gone, the endpoint is dropped.
	for _, c := range h.r.Connections() {
		_ = h.r.Disconnect(c.From, c.To)
	}
	h.rec.Reconcile()
	if got := h.rtp.ids(); len(got) != 0 {
		t.Fatalf("stale endpoint kept: %v", got)
	}
}

func TestGlobalDisableAndReenable(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.rec.Reconcile()
	h.setRTP(false)
	h.rec.Reconcile()
	if len(h.rtp.ids()) != 0 {
		t.Fatal("endpoints must go when RTP is disabled")
	}
	h.setRTP(true)
	h.rec.Reconcile()
	if len(h.rtp.ids()) != 1 {
		t.Fatal("endpoint must return")
	}
}

func TestPerPortOptOutAndAliasName(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	h.r.AddPort(usbPort("usb:3:4:1.2:0", "Keys", true, true))
	no := false
	h.st.Update(func(st *config.State) {
		st.Ports["usb:3:4:1.2:0"] = config.PortSettings{RTP: &no}
		st.Ports["usb:1:2:1.1:0"] = config.PortSettings{Alias: "  Drum Brain "}
	})
	h.rec.Reconcile()
	if got := h.rtp.ids(); len(got) != 1 || got[0] != "rtp:usb:1:2:1.1:0" {
		t.Fatalf("endpoints: %v", got)
	}
	if s := h.rtp.have["rtp:usb:1:2:1.1:0"]; s.Name != "Drum Brain" {
		t.Fatalf("alias not used as network name: %+v", s)
	}
	// Rename takes effect on the next pass.
	h.st.Update(func(st *config.State) {
		ps := st.Ports["usb:1:2:1.1:0"]
		ps.Alias = "Beat Box"
		st.Ports["usb:1:2:1.1:0"] = ps
	})
	h.rec.Reconcile()
	if s := h.rtp.have["rtp:usb:1:2:1.1:0"]; s.Name != "Beat Box" {
		t.Fatalf("rename not propagated: %+v", s)
	}
}

func TestVirtualNetworkPorts(t *testing.T) {
	h := newHarness(t)
	h.st.Update(func(st *config.State) { st.VirtualRTP = []config.VirtualRTP{{ID: "rtp:net-daw", Name: "DAW"}} })
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))
	h.rec.Reconcile()
	got := h.rtp.ids()
	if len(got) != 2 {
		t.Fatalf("endpoints: %v", got)
	}
	v := h.st.Snapshot().VirtualRTP[0]
	if v.Port == 0 || v.Port == h.st.Snapshot().Ports["usb:1:2:1.1:0"].RTPPort {
		t.Fatalf("virtual port allocation: %+v", v)
	}
	if h.conns()["rtp:net-daw > usb:1:2:1.1:0"] {
		t.Fatal("virtual ports get no automatic connections")
	}
}

func TestKnownTracksNonUSBToo(t *testing.T) {
	h := newHarness(t)
	h.r.AddPort(&fakePort{router.PortInfo{ID: "ble:0", Name: "EMM", Kind: router.KindBLE, Input: true, Output: true}})
	h.rec.Reconcile()
	if k := h.st.Snapshot().Known["ble:0"]; k.Kind != router.KindBLE || k.Name != "EMM" {
		t.Fatalf("known: %+v", k)
	}
	if len(h.rtp.ids()) != 0 {
		t.Fatal("BLE ports get no network endpoint")
	}
}

func TestRunReactsToTopologyChanges(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go h.rec.Run(done)
	defer close(done)
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "Digitakt", true, true))
	deadline := time.Now().Add(3 * time.Second)
	for len(h.rtp.ids()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not reconcile after a port appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPortTakenByAnotherProgramIsAvoidedAndRecovered(t *testing.T) {
	// Allocation skips ports that are not free.
	h := newHarness(t)
	h.rtp.taken[5004], h.rtp.taken[5005] = true, true
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))
	h.rec.Reconcile()
	if p := h.st.Snapshot().Ports["usb:1:2:1.1:0"].RTPPort; p == 5004 || p == 0 {
		t.Fatalf("allocated a busy port: %d", p)
	}
	if len(h.rtp.ids()) != 1 {
		t.Fatal("endpoint should run on the free port")
	}

	// A stored port that became busy (e.g. after a software update elsewhere)
	// is dropped and re-allocated.
	h2 := newHarness(t)
	h2.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))
	h2.st.Update(func(st *config.State) { st.Ports["usb:1:2:1.1:0"] = config.PortSettings{RTPPort: 5004} })
	h2.rtp.taken[5004] = true
	h2.rec.Reconcile() // fails: port 5004 is taken, stored port is cleared
	if got := h2.st.Snapshot().Ports["usb:1:2:1.1:0"].RTPPort; got != 0 {
		t.Fatalf("failed port was kept: %d", got)
	}
	h2.rec.Reconcile() // second pass allocates a free pair
	if got := h2.st.Snapshot().Ports["usb:1:2:1.1:0"].RTPPort; got == 0 || got == 5004 {
		t.Fatalf("not re-allocated: %d", got)
	}
	if len(h2.rtp.ids()) != 1 {
		t.Fatal("endpoint did not recover")
	}
}

// A device whose only connections run through its network endpoint
// ("rtp:<id>") is still referenced: its Known entry must survive the 90-day
// pruning, or the endpoint (which is built from Known) disappears with it.
func TestKnownPortReferencedOnlyThroughItsEndpointIsKept(t *testing.T) {
	h := newHarness(t)
	const x, y, gone = "usb:1:2:1.1:0", "usb:3:4:1.2:0", "usb:7:7:1.9:0"
	stale := time.Now().Add(-100 * 24 * time.Hour).Unix()
	conns := []router.ConnSpec{{From: "rtp:" + x, To: y, Filter: router.DefaultFilter()}}
	h.r.Load(conns)
	h.r.AddPort(usbPort(y, "Keys", true, true)) // X stays unplugged
	h.st.Update(func(st *config.State) {
		st.Connections = conns
		st.Known[x] = config.KnownPort{Name: "Digitakt", Kind: router.KindUSB, Input: true, Output: true, LastSeen: stale}
		st.Known[gone] = config.KnownPort{Name: "Gone", Kind: router.KindUSB, Input: true, Output: true, LastSeen: stale}
		st.Ports[x] = config.PortSettings{RTPDefaultsDone: true}
	})
	h.rec.Reconcile()
	k := h.st.Snapshot().Known
	if _, ok := k[x]; !ok {
		t.Fatal("Known entry of a device referenced through its network endpoint was pruned")
	}
	if _, ok := k[gone]; ok {
		t.Fatal("an unreferenced stale port must still be pruned")
	}
	found := false
	for _, id := range h.rtp.ids() {
		found = found || id == "rtp:"+x
	}
	if !found {
		t.Fatalf("the dormant route lost its network endpoint: %v", h.rtp.ids())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// If no UDP port pair can be probed free (fd exhaustion, sandbox policy), the
// pass must still end: alloc runs under the store mutex, so a spin there hangs
// the whole daemon.
func TestPortAllocationExhaustionIsBounded(t *testing.T) {
	h := newHarness(t)
	var logs syncBuf
	h.rec.Log = slog.New(slog.NewTextHandler(&logs, nil))
	var probes atomic.Int64
	h.rec.PortFree = func(int) bool {
		// A runaway loop would never return; let it succeed at some point so
		// the test fails with a message instead of hanging.
		return probes.Add(1) > 500000
	}
	h.st.Update(func(st *config.State) { st.VirtualRTP = []config.VirtualRTP{{ID: "rtp:net-daw", Name: "DAW"}} })
	h.r.AddPort(usbPort("usb:1:2:1.1:0", "A", true, true))

	pass := func() {
		t.Helper()
		before := probes.Load()
		done := make(chan struct{})
		go func() { h.rec.Reconcile(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("Reconcile did not finish with no free UDP ports")
		}
		if n := probes.Load() - before; n > 40000 {
			t.Fatalf("allocation probed %d port pairs in one pass; the search must stop at the top of the range", n)
		}
	}
	pass()
	pass()

	snap := h.st.Snapshot() // the store lock must be free again
	if p := snap.Ports["usb:1:2:1.1:0"].RTPPort; p != 0 {
		t.Fatalf("device endpoint got port %d with nothing free", p)
	}
	if p := snap.VirtualRTP[0].Port; p != 0 {
		t.Fatalf("virtual endpoint got port %d with nothing free", p)
	}
	if ids := h.rtp.ids(); len(ids) != 0 {
		t.Fatalf("endpoints must be skipped, not created on port 0: %v", ids)
	}
	if n := strings.Count(logs.String(), "no free UDP port pair"); n != 1 {
		t.Fatalf("want the problem logged once over two passes, got %d:\n%s", n, logs.String())
	}

	// When ports free up again the next pass allocates normally.
	h.rec.PortFree = func(int) bool { return true }
	h.rec.Reconcile()
	snap = h.st.Snapshot()
	if snap.Ports["usb:1:2:1.1:0"].RTPPort == 0 || snap.VirtualRTP[0].Port == 0 || len(h.rtp.ids()) != 2 {
		t.Fatalf("no recovery: %+v %+v %v", snap.Ports, snap.VirtualRTP, h.rtp.ids())
	}
}
