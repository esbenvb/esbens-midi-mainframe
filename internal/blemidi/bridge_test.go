package blemidi

import (
	"bytes"
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"emm/internal/router"
)

// fakeDevice is a router port that records what it receives.
type fakeDevice struct {
	info router.PortInfo
	mu   sync.Mutex
	buf  []byte
}

func newDevice(id string, in, out bool) *fakeDevice {
	return &fakeDevice{info: router.PortInfo{ID: id, Name: id, Kind: router.KindVirtual, Input: in, Output: out}}
}
func (d *fakeDevice) Info() router.PortInfo { return d.info }
func (d *fakeDevice) Write(p []byte) error {
	d.mu.Lock()
	d.buf = append(d.buf, p...)
	d.mu.Unlock()
	return nil
}
func (d *fakeDevice) Close() error { return nil }
func (d *fakeDevice) got() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.buf...)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	r      *router.Router
	tree   *gattTree
	bus    *fakeBus
	br     *bridge
	cancel context.CancelFunc
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := router.New(router.Options{Logger: discardLog()})
	tree, fb := newTestTree()
	br := newBridge(r, discardLog(), tree, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go br.sendLoop(ctx)
	go br.recvLoop(ctx)
	br.addPort("EMM")
	t.Cleanup(func() { cancel(); br.close() })
	return &rig{r: r, tree: tree, bus: fb, br: br, cancel: cancel}
}

func (g *rig) central(packets ...[]byte) {
	for _, p := range packets {
		g.tree.intercept(writeCall(chrcPath, ifaceChrc, "WriteValue", p, map[string]dbus.Variant{}))
	}
}

// received decodes everything notified to subscribed centrals so far.
func (g *rig) received() []byte {
	var d Decoder
	var out []byte
	for _, p := range g.bus.valuePackets() {
		d.Feed(p, func(b []byte) { out = append(out, b...) })
	}
	return out
}

func TestBridgeRegistersRouterPort(t *testing.T) {
	g := newRig(t)
	snap := g.r.Snapshot()
	if len(snap.Ports) != 1 {
		t.Fatalf("ports: %+v", snap.Ports)
	}
	p := snap.Ports[0]
	if p.ID != "ble:0" || p.Kind != router.KindBLE || !p.Input || !p.Output || p.Name != "EMM" || p.Detail != "Bluetooth LE MIDI" {
		t.Fatalf("port %+v", p)
	}
}

func TestBridgeCentralToDevice(t *testing.T) {
	g := newRig(t)
	synth := newDevice("synth", false, true)
	g.r.AddPort(synth)
	if err := g.r.Connect(PortID, "synth", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	g.central(
		[]byte{0x80, 0x80, 0x90, 0x3C, 0x64}, // note on
		[]byte{0x80, 0x81, 0x3E, 0x50},       // a packet of its own: running status cannot continue, ignored
		[]byte{0x80, 0x82, 0xB0, 7, 99, 0x83, 0xF8},
	)
	want := []byte{0x90, 0x3C, 0x64, 0xB0, 7, 99, 0xF8}
	waitUntil(t, "MIDI at the synth", func() bool { return len(synth.got()) >= len(want) })
	if !bytes.Equal(synth.got(), want) {
		t.Fatalf("got %x want %x", synth.got(), want)
	}
}

func TestBridgeTwoCentralsMergeInOrder(t *testing.T) {
	g := newRig(t)
	synth := newDevice("synth", false, true)
	g.r.AddPort(synth)
	_ = g.r.Connect(PortID, "synth", router.Filter{})
	for i := 0; i < 50; i++ {
		g.central([]byte{0x80, 0x80, 0xB0, 1, byte(i)})
	}
	waitUntil(t, "50 CCs", func() bool { return len(synth.got()) == 150 })
	b := synth.got()
	for i := 0; i < 50; i++ {
		if b[i*3+2] != byte(i) {
			t.Fatalf("message %d out of order: %x", i, b)
		}
	}
}

func TestBridgeDeviceToCentral(t *testing.T) {
	g := newRig(t)
	keys := newDevice("keys", true, false)
	h := g.r.AddPort(keys)
	if err := g.r.Connect("keys", PortID, router.Filter{}); err != nil {
		t.Fatal(err)
	}

	// Nobody subscribed yet: nothing is sent.
	h.Receive([]byte{0x90, 0x3C, 0x64})
	time.Sleep(50 * time.Millisecond)
	if n := len(g.bus.valuePackets()); n != 0 {
		t.Fatalf("%d packets sent with no subscriber", n)
	}

	g.tree.StartNotify()
	h.Receive([]byte{0x90, 0x3E, 0x64, 0xF8})
	want := []byte{0x90, 0x3E, 0x64, 0xF8}
	waitUntil(t, "notifications", func() bool { return len(g.received()) >= len(want) })
	if !bytes.Equal(g.received(), want) {
		t.Fatalf("got %x want %x", g.received(), want)
	}
	for _, p := range g.bus.valuePackets() {
		if p[0]&0xC0 != 0x80 {
			t.Fatalf("packet without a header byte: %x", p)
		}
	}
}

func TestBridgeBidirectionalLargeSysEx(t *testing.T) {
	g := newRig(t)
	keys := newDevice("keys", true, false)
	h := g.r.AddPort(keys)
	_ = g.r.Connect("keys", PortID, router.Filter{})
	g.tree.StartNotify()

	for _, mtu := range []uint16{0, 185} {
		g.bus.mu.Lock()
		g.bus.calls = nil
		g.bus.mu.Unlock()
		g.tree.resetDevices()
		g.tree.StartNotify()
		g.tree.noteDevice(map[string]dbus.Variant{
			"device": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0/dev_AA")),
			"mtu":    dbus.MakeVariant(mtu),
		})

		r := rand.New(rand.NewSource(7))
		msg := []byte{0xF0}
		for i := 0; i < 20000; i++ {
			msg = append(msg, byte(r.Intn(128)))
		}
		msg = append(msg, 0xF7)
		// A source delivers a dump in reads of 1 KiB.
		for off := 0; off < len(msg); off += 1024 {
			end := off + 1024
			if end > len(msg) {
				end = len(msg)
			}
			h.Receive(msg[off:end])
		}
		waitUntil(t, "whole SysEx at the central", func() bool { return len(g.received()) >= len(msg) })
		if !bytes.Equal(g.received(), msg) {
			t.Fatalf("mtu %d: SysEx corrupted (%d bytes back, want %d)", mtu, len(g.received()), len(msg))
		}
		limit := PacketLimit(int(mtu))
		for _, p := range g.bus.valuePackets() {
			if len(p) > limit {
				t.Fatalf("mtu %d: packet of %d bytes exceeds %d", mtu, len(p), limit)
			}
		}
	}
}

func TestBridgeRenamePort(t *testing.T) {
	g := newRig(t)
	synth := newDevice("synth", false, true)
	g.r.AddPort(synth)
	_ = g.r.Connect(PortID, "synth", router.Filter{})
	g.r.RemovePort(PortID) // what a rename does
	g.br.addPort("Mainframe")
	if snap := g.r.Snapshot(); len(snap.Connections) != 1 || !snap.Connections[0].Online {
		t.Fatalf("connection must survive a rename: %+v", snap.Connections)
	}
	g.central([]byte{0x80, 0x80, 0xC0, 3})
	waitUntil(t, "message after rename", func() bool { return len(synth.got()) == 2 })
}

func TestIsSysExChunk(t *testing.T) {
	for _, tc := range []struct {
		data []byte
		want bool
	}{
		{[]byte{0xF0, 0x7D, 0x01}, true},  // start of a SysEx
		{[]byte{0x01, 0x02, 0x03}, true},  // continuation
		{[]byte{0x01, 0x02, 0xF7}, true},  // continuation with the end
		{[]byte{0xF7}, true},              // the closing F7 alone
		{[]byte{0xF8}, false},             // clock
		{[]byte{0xFE}, false},             // active sensing
		{[]byte{0x90, 0x3C, 0x64}, false}, // note on
		{[]byte{0xB0, 0x07, 0x7F}, false}, // controller
		{[]byte{0xF2, 0x00, 0x00}, false}, // song position
		{[]byte{0xFF}, false},             // reset
		{nil, false},
	} {
		if got := isSysExChunk(tc.data); got != tc.want {
			t.Errorf("isSysExChunk(%x) = %v, want %v", tc.data, got, tc.want)
		}
	}
}

// bareBridge is a bridge without a send loop and with a tiny outgoing queue,
// so a test can hold the queue full.
func bareBridge(t *testing.T, queueCap int) *bridge {
	t.Helper()
	r := router.New(router.Options{Logger: discardLog()})
	tree, _ := newTestTree()
	br := newBridge(r, discardLog(), tree, 0)
	br.out = newPktQueue(queueCap)
	br.sysexWait = 40 * time.Millisecond
	tree.StartNotify()
	t.Cleanup(br.close)
	return br
}

// sysexChunks sends a SysEx to the bridge as n router chunks of 4 data bytes
// (one packet each at the default MTU): chunk i carries the byte i.
func sysexChunks(br *bridge, from, to int) {
	for i := from; i < to; i++ {
		chunk := []byte{byte(i), byte(i), byte(i), byte(i)}
		if i == 0 {
			chunk = []byte{0xF0, 0, 0, 0}
		}
		br.send(chunk)
	}
}

// queuedPackets empties the queue. It returns the sequence byte of every
// SysEx packet (F0 for the first, whose packet is header, timestamp, F0, ...;
// the first data byte of a continuation, whose packet is header, data...) and
// the number of realtime packets (header, timestamp, one realtime byte).
func queuedPackets(t *testing.T, br *bridge) (sysex []byte, other int) {
	t.Helper()
	for n := br.out.len(); n > 0; n-- {
		p, ok := br.out.pop(ctxT(t))
		if !ok {
			t.Fatal("pop failed")
		}
		switch {
		case len(p) == 3 && p[2] >= 0xF8: // header, timestamp, realtime byte
			other++
		case len(p) >= 4 && p[1]&0x80 != 0 && p[2] == 0xF0:
			sysex = append(sysex, 0xF0)
		case len(p) >= 4 && p[1]&0x80 == 0:
			sysex = append(sysex, p[1])
		default:
			t.Fatalf("unexpected packet %x", p)
		}
	}
	return sysex, other
}

// The reported failure: a realtime byte routed to the BLE port in the middle
// of a dump that has the queue full must not push an unsent SysEx packet out.
func TestBridgeRealtimeDuringFullQueueKeepsSysEx(t *testing.T) {
	const capacity = 6
	br := bareBridge(t, capacity)
	sysexChunks(br, 0, capacity) // queue full of SysEx, nothing draining
	if br.out.len() != capacity {
		t.Fatalf("queue holds %d, want %d", br.out.len(), capacity)
	}
	for i := 0; i < 5; i++ {
		br.send([]byte{0xF8}) // clock ticks from another source
	}
	sx, other := queuedPackets(t, br)
	if other != 0 {
		t.Fatalf("%d realtime packets queued; with only SysEx queued they must give way", other)
	}
	want := []byte{0xF0, 1, 2, 3, 4, 5}
	if string(sx) != string(want) {
		t.Fatalf("SysEx packets %v, want %v: a realtime byte evicted SysEx data", sx, want)
	}
}

// With disposable packets queued behind the SysEx, a realtime byte replaces
// the oldest of those, not SysEx.
func TestBridgeRealtimeEvictsOlderRealtimeNotSysEx(t *testing.T) {
	const capacity = 6
	br := bareBridge(t, capacity)
	sysexChunks(br, 0, 4)
	br.send([]byte{0xF8})
	br.send([]byte{0xFA})
	for i := 0; i < 4; i++ {
		br.send([]byte{0xF8})
	}
	sx, other := queuedPackets(t, br)
	if string(sx) != string([]byte{0xF0, 1, 2, 3}) || other != 2 {
		t.Fatalf("SysEx packets %v, %d realtime packets", sx, other)
	}
}

// The closing F7, delivered alone, is part of the dump: it waits for room
// like the rest and does not evict anything.
func TestBridgeLoneF7WaitsAndEvictsNothing(t *testing.T) {
	const capacity = 4
	br := bareBridge(t, capacity)
	br.sysexWait = 5 * time.Second
	sysexChunks(br, 0, capacity)

	var (
		mu  sync.Mutex
		got [][]byte
	)
	popped := make(chan struct{})
	go func() {
		defer close(popped)
		time.Sleep(30 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for i := 0; i < capacity+1; i++ { // the consumer catches up
			p, ok := br.out.pop(ctx)
			if !ok {
				return
			}
			mu.Lock()
			got = append(got, p)
			mu.Unlock()
		}
	}()
	br.send([]byte{0xF7})
	<-popped

	mu.Lock()
	defer mu.Unlock()
	if len(got) != capacity+1 {
		t.Fatalf("%d packets reached the consumer, want %d", len(got), capacity+1)
	}
	for i := 0; i < capacity; i++ {
		pkt, first, at := got[i], byte(i), 1 // continuation: header, data...
		if i == 0 {
			first, at = 0xF0, 2 // start: header, timestamp, F0, ...
		}
		if pkt[at] != first {
			t.Fatalf("packet %d is %x, want %x at byte %d: a SysEx packet was lost or reordered", i, pkt, first, at)
		}
	}
	if last := got[capacity]; last[len(last)-1] != 0xF7 {
		t.Fatalf("last packet %x does not end the SysEx", last)
	}
	if br.out.droppedTotal() != 0 {
		t.Fatalf("%d packets dropped", br.out.droppedTotal())
	}
}
