package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"emm/internal/router"
)

// rawHarness runs a rawBackend against a fake sysfs, fake device nodes and a
// real router (the router's Handle cannot be faked outside its package).
type rawHarness struct {
	t      *testing.T
	sys    *rawFakeSys
	acc    *rawFakeAccess
	r      *router.Router
	b      *rawBackend
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T) *rawHarness {
	t.Helper()
	h := &rawHarness{t: t, sys: newRawFakeSys(t), acc: newRawFakeAccess(t)}
	h.r = router.New(router.Options{Logger: rawDiscardLogger()})
	h.b = newRawBackend(h.sys.opts(), h.acc)
	h.b.retryBase = 5 * time.Millisecond
	h.b.maxAttempts = 4
	h.b.readerStopWait = 2 * time.Second
	h.b.closeWait = time.Second
	return h
}

func (h *rawHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.b.Run(ctx, h.r) }()
	h.t.Cleanup(h.stop)
}

func (h *rawHarness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Error("Run did not return after cancel")
	}
}

func (h *rawHarness) hasPort(id string) bool { return h.r.HasPort(id) }

func (h *rawHarness) waitPort(id string, want bool) {
	h.t.Helper()
	state := map[bool]string{true: "present", false: "absent"}[want]
	rawWaitFor(h.t, "port "+id+" "+state, func() bool { return h.hasPort(id) == want })
}

const synthID = "usb:1234:abcd:SN-0001:0"

func synthAttrs(devnum string) rawUSBAttrs {
	return rawUSBAttrs{vid: "1234", pid: "abcd", manufacturer: "Acme", product: "Synth", serial: "SN-0001", devnum: devnum}
}

func TestBackendHotplugAddRemoveReplug(t *testing.T) {
	h := newHarness(t)
	h.start()
	time.Sleep(30 * time.Millisecond)
	if len(h.r.Snapshot().Ports) != 0 {
		t.Fatal("no devices yet")
	}

	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	snap := h.r.Snapshot()
	p := snap.Ports[0]
	if p.Name != "Synth" || p.Kind != router.KindUSB || !p.Input || !p.Output || p.Group != "Acme Synth" {
		t.Fatalf("port info %+v", p.PortInfo)
	}
	dev := h.acc.dev("hw:1,0,0")

	h.sys.unplug(dir, 1, 0)
	h.waitPort(synthID, false)
	rawWaitFor(t, "node closed", dev.node.isClosed)

	// Plugged in again: a fresh enumeration (new device number) comes back.
	dir = h.sys.usb("1-1.2", synthAttrs("4"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	if n := h.acc.openCount("hw:1,0,0"); n != 2 {
		t.Fatalf("expected 2 opens, got %d", n)
	}
}

func TestBackendReceivesAndSends(t *testing.T) {
	h := newHarness(t)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	dev := h.acc.dev("hw:1,0,0")

	// device -> host -> router -> sink
	sink := &rawRecPort{info: router.PortInfo{ID: "sink", Name: "sink", Kind: router.KindVirtual, Output: true}}
	h.r.AddPort(sink)
	if err := h.r.Connect(synthID, "sink", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.toHost.Write([]byte{0x90, 60, 100, 0x80, 60, 0}); err != nil {
		t.Fatal(err)
	}
	rawWaitFor(t, "bytes at sink", func() bool { return bytes.Equal(sink.bytes(), []byte{0x90, 60, 100, 0x80, 60, 0}) })

	// source -> router -> host -> device
	src := &rawRecPort{info: router.PortInfo{ID: "src", Name: "src", Kind: router.KindVirtual, Input: true}}
	hs := h.r.AddPort(src)
	if err := h.r.Connect("src", synthID, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	hs.Receive([]byte{0x93, 64, 90})
	buf := make([]byte, 3)
	dev.fromHost.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(dev.fromHost, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, []byte{0x93, 64, 90}) {
		t.Fatalf("device received %x", buf)
	}
}

func TestBackendMultiCableDeviceDirections(t *testing.T) {
	h := newHarness(t)
	h.acc.setSubs(2, 0,
		rawSub{Sub: 0, Name: "Box MIDI 1", Input: true, Output: true},
		rawSub{Sub: 1, Name: "Box MIDI 2", Input: false, Output: true},
	)
	h.start()
	dir := h.sys.usb("1-2", rawUSBAttrs{vid: "aaaa", pid: "bbbb", product: "Box", serial: "BOX-4711"})
	h.sys.midi(dir, 2, 0)
	h.waitPort("usb:aaaa:bbbb:BOX-4711:0", true)
	h.waitPort("usb:aaaa:bbbb:BOX-4711:1", true)
	for _, p := range h.r.Snapshot().Ports {
		switch p.ID {
		case "usb:aaaa:bbbb:BOX-4711:0":
			if !p.Input || !p.Output {
				t.Errorf("port 0 should be bidirectional: %+v", p)
			}
		case "usb:aaaa:bbbb:BOX-4711:1":
			if p.Input || !p.Output {
				t.Errorf("port 1 should be output only: %+v", p)
			}
		}
	}
	// an output-only subdevice gets no reader but can be written to
	dev := h.acc.dev("hw:2,0,1")
	src := &rawRecPort{info: router.PortInfo{ID: "src", Name: "src", Kind: router.KindVirtual, Input: true}}
	hs := h.r.AddPort(src)
	if err := h.r.Connect("src", "usb:aaaa:bbbb:BOX-4711:1", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	hs.Receive([]byte{0xC0, 5})
	buf := make([]byte, 2)
	dev.fromHost.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(dev.fromHost, buf); err != nil || !bytes.Equal(buf, []byte{0xC0, 5}) {
		t.Fatalf("got %x, %v", buf, err)
	}
}

func TestBackendReadFailureRemovesPortUntilReplug(t *testing.T) {
	h := newHarness(t)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	dev := h.acc.dev("hw:1,0,0")

	// The kernel node starts failing (e.g. ENODEV just after an unplug).
	dev.toHost.Close()
	h.waitPort(synthID, false)

	// The device is still listed in sysfs: it must not flap back in.
	time.Sleep(150 * time.Millisecond)
	if h.hasPort(synthID) || h.acc.openCount("hw:1,0,0") != 1 {
		t.Fatalf("port flapped: present=%v opens=%d", h.hasPort(synthID), h.acc.openCount("hw:1,0,0"))
	}

	// A real re-plug brings it back.
	h.sys.unplug(dir, 1, 0)
	time.Sleep(50 * time.Millisecond)
	dir = h.sys.usb("1-1.2", synthAttrs("4"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	if n := h.acc.openCount("hw:1,0,0"); n != 2 {
		t.Fatalf("opens %d", n)
	}
}

func TestBackendWriteFailureRemovesPort(t *testing.T) {
	h := newHarness(t)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	dev := h.acc.dev("hw:1,0,0")

	src := &rawRecPort{info: router.PortInfo{ID: "src", Name: "src", Kind: router.KindVirtual, Input: true}}
	hs := h.r.AddPort(src)
	if err := h.r.Connect("src", synthID, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	dev.fromHost.Close() // reading side vanishes: host writes now fail with EPIPE
	hs.Receive([]byte{0x90, 60, 100})
	h.waitPort(synthID, false)
}

func TestBackendOpenRetriesWithBackoff(t *testing.T) {
	h := newHarness(t)
	h.acc.setOpenFail("hw:1,0,0", 2)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	if n := h.acc.openCount("hw:1,0,0"); n != 3 {
		t.Fatalf("expected 3 open attempts, got %d", n)
	}
}

func TestBackendGivesUpUntilReplug(t *testing.T) {
	h := newHarness(t)
	h.acc.setOpenFail("hw:1,0,0", -1)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	rawWaitFor(t, "all attempts used", func() bool { return h.acc.openCount("hw:1,0,0") == h.b.maxAttempts })
	time.Sleep(150 * time.Millisecond)
	if n := h.acc.openCount("hw:1,0,0"); n != h.b.maxAttempts {
		t.Fatalf("kept retrying: %d attempts", n)
	}
	if h.hasPort(synthID) {
		t.Fatal("port must not exist")
	}

	// After a re-plug (and with the problem gone) it works again.
	h.acc.setOpenFail("hw:1,0,0", 0)
	h.sys.unplug(dir, 1, 0)
	time.Sleep(50 * time.Millisecond)
	dir = h.sys.usb("1-1.2", synthAttrs("4"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
}

func TestBackendDuplicateSerialKeepsIdentityWhenOneIsUnplugged(t *testing.T) {
	h := newHarness(t)
	same := rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "SN0001"}
	a, b := same, same
	a.devnum, b.devnum = "5", "6"
	d1 := h.sys.usb("1-1.2", a)
	d2 := h.sys.usb("1-1.3", b)
	h.sys.midi(d1, 1, 0)
	h.sys.midi(d2, 2, 0)
	h.start() // both are present from the first poll on
	h.waitPort("usb:1234:abcd:1-1.2:0", true)
	h.waitPort("usb:1234:abcd:1-1.3:0", true)

	h.sys.unplug(d2, 2, 0)
	h.waitPort("usb:1234:abcd:1-1.3:0", false)
	time.Sleep(100 * time.Millisecond)
	if !h.hasPort("usb:1234:abcd:1-1.2:0") || h.hasPort("usb:1234:abcd:SN0001:0") {
		t.Fatalf("remaining device changed identity: %+v", h.r.Snapshot().Ports)
	}
	if n := h.acc.openCount("hw:1,0,0"); n != 1 {
		t.Fatalf("remaining device was reopened %d times", n)
	}
}

func TestBackendShutdownClosesPortsAndLeaksNothing(t *testing.T) {
	base := runtime.NumGoroutine()

	h := newHarness(t)
	h.start()
	d1 := h.sys.usb("1-1.2", synthAttrs("3"))
	d2 := h.sys.usb("1-1.3", rawUSBAttrs{vid: "1111", pid: "2222", product: "Other", serial: "OTHER-77"})
	h.sys.midi(d1, 1, 0)
	h.sys.midi(d2, 2, 0)
	h.waitPort(synthID, true)
	h.waitPort("usb:1111:2222:OTHER-77:0", true)
	dev1, dev2 := h.acc.dev("hw:1,0,0"), h.acc.dev("hw:2,0,0")

	h.stop()

	if n := len(h.r.Snapshot().Ports); n != 0 {
		t.Fatalf("%d ports left in the router", n)
	}
	if !dev1.node.isClosed() || !dev2.node.isClosed() {
		t.Fatal("device nodes not closed on shutdown")
	}
	rawWaitFor(t, "goroutines to wind down", func() bool { return runtime.NumGoroutine() <= base })
}

func TestBackendShutdownWithBlockedWriterDoesNotHang(t *testing.T) {
	h := newHarness(t)
	h.start()
	dir := h.sys.usb("1-1.2", synthAttrs("3"))
	h.sys.midi(dir, 1, 0)
	h.waitPort(synthID, true)
	h.acc.dev("hw:1,0,0") // the "device" never reads what we write

	src := &rawRecPort{info: router.PortInfo{ID: "src", Name: "src", Kind: router.KindVirtual, Input: true}}
	hs := h.r.AddPort(src)
	if err := h.r.Connect("src", synthID, router.Filter{}); err != nil {
		t.Fatal(err)
	}
	// Enough SysEx to fill the pipe and wedge the writer inside Write.
	big := append([]byte{0xF0}, bytes.Repeat([]byte{0x11}, 512*1024)...)
	big = append(big, 0xF7)
	go hs.Receive(big)
	rawWaitFor(t, "writer to block", func() bool { return h.r.Stats()[synthID].TxBytes > 0 })
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	h.stop() // Run returns, closing the port, which must wake the blocked Write
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v (the router's 2s writer grace period was hit)", d)
	}
}

// ---- rawPort in isolation ----------------------------------------------------

func newTestPort(t *testing.T, out bool) (*rawPort, *rawFakeDevice, *int) {
	t.Helper()
	node, dev := newRawPipeNode(t)
	fails := new(int)
	p := &rawPort{
		info:      router.PortInfo{ID: "p", Name: "p", Input: true, Output: out},
		node:      node,
		closeWait: time.Second,
		onFail:    func() { *fails++ },
	}
	return p, dev, fails
}

func TestRawPortWriteBlocksUntilDeviceDrains(t *testing.T) {
	p, dev, _ := newTestPort(t, true)

	const size = 1 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*13) & 0x7F
	}
	done := make(chan error, 1)
	go func() { done <- p.Write(payload) }()

	select {
	case err := <-done:
		t.Fatalf("Write returned (%v) although nobody reads the device: no back-pressure", err)
	case <-time.After(150 * time.Millisecond):
	}

	// A slow device: drains in small steps with pauses.
	got := make([]byte, 0, size)
	buf := make([]byte, 4096)
	for len(got) < size {
		n, err := dev.fromHost.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
		if len(got)%(64*1024) < n {
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write did not return after the device drained everything")
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("bytes corrupted or reordered")
	}
}

func TestRawPortCloseUnblocksWrite(t *testing.T) {
	p, _, fails := newTestPort(t, true)
	done := make(chan error, 1)
	go func() { done <- p.Write(make([]byte, 4<<20)) }() // nobody reads
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errRawPortClosed) {
			t.Fatalf("blocked Write returned %v, want errRawPortClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the Write")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("unblocking took %v", time.Since(start))
	}
	if *fails != 0 {
		t.Fatal("a deliberate close must not be reported as a failure")
	}
	if err := p.Write([]byte{0xF8}); !errors.Is(err, errRawPortClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if err := p.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestRawPortFailsOnceAndRefusesFurtherWrites(t *testing.T) {
	p, dev, fails := newTestPort(t, true)
	dev.fromHost.Close() // device end gone: writes fail with EPIPE
	err := p.Write([]byte{0x90, 60, 100})
	if err == nil {
		t.Fatal("write to a dead device succeeded")
	}
	if *fails != 1 {
		t.Fatalf("onFail called %d times", *fails)
	}
	if err := p.Write([]byte{0x90, 61, 100}); !errors.Is(err, errRawPortFailed) {
		t.Fatalf("second write: %v", err)
	}
	if *fails != 1 {
		t.Fatalf("onFail called %d times", *fails)
	}
}

func TestRawPortInputOnlyRefusesWrites(t *testing.T) {
	p, _, _ := newTestPort(t, false)
	if err := p.Write([]byte{0xF8}); !errors.Is(err, errRawInputOnly) {
		t.Fatalf("got %v", err)
	}
}

func TestRawPortEmptyWriteIsNoop(t *testing.T) {
	p, _, fails := newTestPort(t, true)
	if err := p.Write(nil); err != nil || *fails != 0 {
		t.Fatalf("%v %d", err, *fails)
	}
}

func TestRawPortCloseTimeoutDoesNotWedge(t *testing.T) {
	slow := &slowCloseNode{release: make(chan struct{})}
	p := &rawPort{info: router.PortInfo{ID: "p", Output: true}, node: slow, closeWait: 50 * time.Millisecond}
	start := time.Now()
	err := p.Close()
	close(slow.release)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("Close = %v after %v", err, time.Since(start))
	}
}

type slowCloseNode struct{ release chan struct{} }

func (s *slowCloseNode) Read(p []byte) (int, error)  { return 0, io.EOF }
func (s *slowCloseNode) Write(p []byte) (int, error) { return len(p), nil }
func (s *slowCloseNode) Close() error                { <-s.release; return nil }

func TestUnsupportedPlatformsOnly(t *testing.T) {
	// NewRawMIDI must always return a usable Backend; on non-Linux it fails
	// from Run with a clear error rather than at construction.
	b := NewRawMIDI(RawMIDIOptions{})
	if b.Name() != "rawmidi" {
		t.Fatal(b.Name())
	}
	if runtime.GOOS == "linux" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Run(ctx, router.New(router.Options{})); err == nil {
		t.Fatal("Run should fail off Linux")
	}
}
