//go:build darwin && cgo

package device

// macOS CoreMIDI backend (development aid; the Pi uses the rawmidi backend).
//
// Threading model, which is the part that is easy to get wrong:
//
//   - CoreMIDI delivers *notifications* (device added/removed, setup changed)
//     on the CFRunLoop of the thread that called MIDIClientCreate. A Go
//     goroutine normally hops between OS threads, so Run pins itself to one
//     thread, creates the client there and then spins that thread's run loop
//     in short slices until the context ends. Hot-plug reconciliation runs on
//     that same goroutine, never inside the notification callback. Because
//     notification delivery is not fully reliable, a once-a-second rescan
//     backs it up (see cmRescan).
//   - Incoming MIDI arrives on CoreMIDI's own high-priority threads. The read
//     proc only copies the bytes into a per-port channel; a per-port goroutine
//     feeds router.Handle.Receive (which is not safe for concurrent use and may
//     block for back-pressure), so one stalled destination cannot stall the
//     CoreMIDI thread shared with other sources.
//   - Callbacks reach Go through small integer tokens looked up in a registry,
//     never raw Go pointers; a callback for a port that has just been disposed
//     finds no token and does nothing.
//
// Output uses MIDISend with packets of at most 256 bytes. MIDISend returns as
// soon as CoreMIDI has queued the data, so unlike the Linux rawmidi backend
// there is no real back-pressure from the device.

/*
#cgo CFLAGS: -Wno-deprecated-declarations -Wno-unused-function
#cgo LDFLAGS: -framework CoreMIDI -framework CoreFoundation
#include <CoreMIDI/CoreMIDI.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Implemented in Go (//export below). Declarations only: this preamble is
// compiled into more than one C file.
extern void goCMRead(uintptr_t token, void *data, int n);
extern void goCMNotify(uintptr_t token, int messageID);

static void emmReadProc(const MIDIPacketList *pl, void *readRefCon, void *srcRefCon) {
	const MIDIPacket *p = &pl->packet[0];
	for (UInt32 i = 0; i < pl->numPackets; i++) {
		if (p->length > 0) {
			goCMRead((uintptr_t)srcRefCon, (void *)p->data, (int)p->length);
		}
		p = MIDIPacketNext(p);
	}
}

static void emmNotifyProc(const MIDINotification *m, void *refCon) {
	goCMNotify((uintptr_t)refCon, (int)m->messageID);
}

static int emmClientCreate(const char *name, uintptr_t token, uint32_t *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
	MIDIClientRef c = 0;
	OSStatus st = MIDIClientCreate(s, emmNotifyProc, (void *)token, &c);
	CFRelease(s);
	*out = (uint32_t)c;
	return (int)st;
}

static int emmInputPortCreate(uint32_t client, const char *name, uint32_t *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
	MIDIPortRef p = 0;
	OSStatus st = MIDIInputPortCreate((MIDIClientRef)client, s, emmReadProc, NULL, &p);
	CFRelease(s);
	*out = (uint32_t)p;
	return (int)st;
}

static int emmOutputPortCreate(uint32_t client, const char *name, uint32_t *out) {
	CFStringRef s = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
	MIDIPortRef p = 0;
	OSStatus st = MIDIOutputPortCreate((MIDIClientRef)client, s, &p);
	CFRelease(s);
	*out = (uint32_t)p;
	return (int)st;
}

static int emmConnect(uint32_t port, uint32_t src, uintptr_t token) {
	return (int)MIDIPortConnectSource((MIDIPortRef)port, (MIDIEndpointRef)src, (void *)token);
}

static int emmDisconnect(uint32_t port, uint32_t src) {
	return (int)MIDIPortDisconnectSource((MIDIPortRef)port, (MIDIEndpointRef)src);
}

// Sends n bytes to a destination as packets of at most 256 bytes.
// Returns 0, an OSStatus, or -1 if a packet could not be built.
static int emmSend(uint32_t port, uint32_t dst, const void *data, int n) {
	Byte buf[512];
	int off = 0;
	while (off < n) {
		int len = n - off;
		if (len > 256) len = 256;
		MIDIPacketList *pl = (MIDIPacketList *)buf;
		MIDIPacket *pkt = MIDIPacketListInit(pl);
		pkt = MIDIPacketListAdd(pl, sizeof(buf), pkt, 0, (ByteCount)len, (const Byte *)data + off);
		if (!pkt) return -1;
		OSStatus st = MIDISend((MIDIPortRef)port, (MIDIEndpointRef)dst, pl);
		if (st != noErr) return (int)st;
		off += len;
	}
	return 0;
}

// String property selectors; keep in sync with the Go constants.
static int emmStr(uint32_t obj, int which, char *buf, int n) {
	CFStringRef key;
	switch (which) {
	case 0: key = kMIDIPropertyName; break;
	case 1: key = kMIDIPropertyDisplayName; break;
	case 2: key = kMIDIPropertyManufacturer; break;
	case 3: key = kMIDIPropertyModel; break;
	case 4: key = kMIDIPropertyDriverOwner; break;
	default: return -1;
	}
	CFStringRef s = NULL;
	if (MIDIObjectGetStringProperty((MIDIObjectRef)obj, key, &s) != noErr || !s) return -1;
	Boolean ok = CFStringGetCString(s, buf, n, kCFStringEncodingUTF8);
	CFRelease(s);
	return ok ? (int)strlen(buf) : -1;
}

// Integer property selectors; keep in sync with the Go constants.
static int emmInt(uint32_t obj, int which, int32_t *v) {
	CFStringRef key;
	switch (which) {
	case 0: key = kMIDIPropertyUniqueID; break;
	case 1: key = kMIDIPropertyOffline; break;
	case 2: key = kMIDIPropertyPrivate; break;
	default: return -1;
	}
	SInt32 x = 0;
	OSStatus st = MIDIObjectGetIntegerProperty((MIDIObjectRef)obj, key, &x);
	*v = (int32_t)x;
	return (int)st;
}

static int emmNumDevices(void) { return (int)MIDIGetNumberOfDevices(); }
static uint32_t emmDevice(int i) { return (uint32_t)MIDIGetDevice((ItemCount)i); }
static int emmNumEntities(uint32_t dev) { return (int)MIDIDeviceGetNumberOfEntities((MIDIDeviceRef)dev); }
static uint32_t emmEntity(uint32_t dev, int i) { return (uint32_t)MIDIDeviceGetEntity((MIDIDeviceRef)dev, (ItemCount)i); }
static int emmEntNumSources(uint32_t e) { return (int)MIDIEntityGetNumberOfSources((MIDIEntityRef)e); }
static uint32_t emmEntSource(uint32_t e, int i) { return (uint32_t)MIDIEntityGetSource((MIDIEntityRef)e, (ItemCount)i); }
static int emmEntNumDests(uint32_t e) { return (int)MIDIEntityGetNumberOfDestinations((MIDIEntityRef)e); }
static uint32_t emmEntDest(uint32_t e, int i) { return (uint32_t)MIDIEntityGetDestination((MIDIEntityRef)e, (ItemCount)i); }
static int emmNumSources(void) { return (int)MIDIGetNumberOfSources(); }
static uint32_t emmSource(int i) { return (uint32_t)MIDIGetSource((ItemCount)i); }
static int emmNumDests(void) { return (int)MIDIGetNumberOfDestinations(); }
static uint32_t emmDest(int i) { return (uint32_t)MIDIGetDestination((ItemCount)i); }

static int emmRunSlice(double secs) {
	return (int)CFRunLoopRunInMode(kCFRunLoopDefaultMode, secs, false);
}
static void emmDisposePort(uint32_t p) { MIDIPortDispose((MIDIPortRef)p); }
static void emmDisposeClient(uint32_t c) { MIDIClientDispose((MIDIClientRef)c); }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"emm/internal/router"
)

// Selectors shared with the C helpers above.
const (
	cmStrName         = 0
	cmStrDisplayName  = 1
	cmStrManufacturer = 2
	cmStrModel        = 3
	cmStrDriverOwner  = 4

	cmIntUniqueID = 0
	cmIntOffline  = 1
	cmIntPrivate  = 2
)

// CoreMIDI notification message IDs (MIDINotificationMessageID) we react to.
const (
	cmMsgSetupChanged   = 1
	cmMsgObjectAdded    = 2
	cmMsgObjectRemoved  = 3
	cmMsgPropertyChange = 4
)

const (
	cmDebounce     = 100 * time.Millisecond // quiet time before re-enumerating
	cmMaxDirty     = time.Second            // …but never postpone longer than this
	cmRescan       = time.Second            // safety net: re-enumerate even without notifications
	cmRunSlice     = 0.05                   // seconds per CFRunLoop slice
	cmRxQueueDepth = 256
	cmRxBlockMax   = 2 * time.Second // how long the read proc may wait for the router
)

// ---- token registry --------------------------------------------------------

// cmRxSink receives bytes from a CoreMIDI read proc.
type cmRxSink interface{ onBytes([]byte) }

var (
	cmRegistry sync.Map // uintptr token -> cmRxSink or *coreMIDI
	cmNextTok  atomic.Uintptr
)

func cmRegister(v any) uintptr {
	t := cmNextTok.Add(1)
	cmRegistry.Store(t, v)
	return t
}

//export goCMRead
func goCMRead(token C.uintptr_t, data unsafe.Pointer, n C.int) {
	v, ok := cmRegistry.Load(uintptr(token))
	if !ok {
		return
	}
	if sink, ok := v.(cmRxSink); ok {
		sink.onBytes(C.GoBytes(data, n))
	}
}

//export goCMNotify
func goCMNotify(token C.uintptr_t, messageID C.int) {
	v, ok := cmRegistry.Load(uintptr(token))
	if !ok {
		return
	}
	b, ok := v.(*coreMIDI)
	if !ok {
		return
	}
	b.log.Debug("notification", "id", int(messageID))
	switch int(messageID) {
	case cmMsgSetupChanged, cmMsgObjectAdded, cmMsgObjectRemoved, cmMsgPropertyChange:
		b.markDirty()
	}
}

// ---- reading the object graph ---------------------------------------------

func cmStr(obj uint32, which int) string {
	var buf [256]C.char
	n := C.emmStr(C.uint32_t(obj), C.int(which), &buf[0], C.int(len(buf)))
	if n < 0 {
		return ""
	}
	return C.GoStringN(&buf[0], n)
}

func cmInt(obj uint32, which int) (int32, bool) {
	var v C.int32_t
	if C.emmInt(C.uint32_t(obj), C.int(which), &v) != 0 {
		return 0, false
	}
	return int32(v), true
}

// cmUsable is false for offline and private objects.
func cmUsable(obj uint32) bool {
	if v, ok := cmInt(obj, cmIntOffline); ok && v != 0 {
		return false
	}
	if v, ok := cmInt(obj, cmIntPrivate); ok && v != 0 {
		return false
	}
	return true
}

func cmReadEP(ref uint32) (cmEP, bool) {
	if ref == 0 || !cmUsable(ref) {
		return cmEP{}, false
	}
	uid, _ := cmInt(ref, cmIntUniqueID)
	name := cmFirstNonEmpty(cmStr(ref, cmStrDisplayName), cmStr(ref, cmStrName))
	return cmEP{Ref: ref, UID: uid, Name: name}, true
}

// readGraph snapshots the CoreMIDI object graph.
func readGraph() cmGraph {
	var g cmGraph
	for i, n := 0, int(C.emmNumDevices()); i < n; i++ {
		dev := uint32(C.emmDevice(C.int(i)))
		if dev == 0 || !cmUsable(dev) {
			continue
		}
		d := cmDevice{
			Name:         cmStr(dev, cmStrName),
			Manufacturer: cmStr(dev, cmStrManufacturer),
			Model:        cmStr(dev, cmStrModel),
			Driver:       cmStr(dev, cmStrDriverOwner),
		}
		for j, ne := 0, int(C.emmNumEntities(C.uint32_t(dev))); j < ne; j++ {
			ent := uint32(C.emmEntity(C.uint32_t(dev), C.int(j)))
			if ent == 0 {
				continue
			}
			e := cmEntity{Name: cmStr(ent, cmStrName)}
			for k, ns := 0, int(C.emmEntNumSources(C.uint32_t(ent))); k < ns; k++ {
				if ep, ok := cmReadEP(uint32(C.emmEntSource(C.uint32_t(ent), C.int(k)))); ok {
					e.Sources = append(e.Sources, ep)
				}
			}
			for k, nd := 0, int(C.emmEntNumDests(C.uint32_t(ent))); k < nd; k++ {
				if ep, ok := cmReadEP(uint32(C.emmEntDest(C.uint32_t(ent), C.int(k)))); ok {
					e.Dests = append(e.Dests, ep)
				}
			}
			d.Entities = append(d.Entities, e)
		}
		g.Devices = append(g.Devices, d)
	}
	for i, n := 0, int(C.emmNumSources()); i < n; i++ {
		if ep, ok := cmReadEP(uint32(C.emmSource(C.int(i)))); ok {
			g.Sources = append(g.Sources, ep)
		}
	}
	for i, n := 0, int(C.emmNumDests()); i < n; i++ {
		if ep, ok := cmReadEP(uint32(C.emmDest(C.int(i)))); ok {
			g.Dests = append(g.Dests, ep)
		}
	}
	return g
}

// ListCoreMIDI returns the ports the backend would currently expose,
// including virtual ones. It is a diagnostic aid and needs no running backend.
func ListCoreMIDI() ([]CoreMIDIEndpointInfo, error) {
	specs := buildSpecs(readGraph(), true)
	out := make([]CoreMIDIEndpointInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.info())
	}
	return out, nil
}

// ---- the backend -----------------------------------------------------------

type coreMIDI struct {
	opts CoreMIDIOptions
	log  *slog.Logger

	token                   uintptr
	client, inPort, outPort uint32
	host                    router.Host
	ports                   map[string]*cmPort // only touched by the Run goroutine
	wg                      sync.WaitGroup     // per-port rx goroutines

	dirty      atomic.Bool
	dirtySince atomic.Int64 // unix nanos of the first notification of this burst
	lastNotify atomic.Int64 // unix nanos of the latest one
	started    atomic.Bool
}

// NewCoreMIDI returns the macOS CoreMIDI backend.
func NewCoreMIDI(o CoreMIDIOptions) router.Backend {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &coreMIDI{opts: o, log: log.With("component", "coremidi"), ports: map[string]*cmPort{}}
}

func (b *coreMIDI) Name() string { return "coremidi" }

func (b *coreMIDI) markDirty() {
	now := time.Now().UnixNano()
	b.lastNotify.Store(now)
	if b.dirty.CompareAndSwap(false, true) {
		b.dirtySince.Store(now)
	}
}

func (b *coreMIDI) Run(ctx context.Context, h router.Host) error {
	if !b.started.CompareAndSwap(false, true) {
		return errors.New("coremidi: Run called twice")
	}
	// Notifications arrive on the run loop of the thread that creates the
	// client, so stay on this one thread for the whole lifetime of Run.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	b.host = h
	b.token = cmRegister(b)
	defer cmRegistry.Delete(b.token)

	name := C.CString("EMM")
	defer C.free(unsafe.Pointer(name))
	var c, in, out C.uint32_t
	if st := C.emmClientCreate(name, C.uintptr_t(b.token), &c); st != 0 {
		return fmt.Errorf("coremidi: MIDIClientCreate: OSStatus %d (is the MIDI server reachable?)", int(st))
	}
	b.client = uint32(c)
	defer C.emmDisposeClient(c)
	inName := C.CString("EMM in")
	defer C.free(unsafe.Pointer(inName))
	if st := C.emmInputPortCreate(c, inName, &in); st != 0 {
		return fmt.Errorf("coremidi: MIDIInputPortCreate: OSStatus %d", int(st))
	}
	b.inPort = uint32(in)
	outName := C.CString("EMM out")
	defer C.free(unsafe.Pointer(outName))
	if st := C.emmOutputPortCreate(c, outName, &out); st != 0 {
		C.emmDisposePort(in)
		return fmt.Errorf("coremidi: MIDIOutputPortCreate: OSStatus %d", int(st))
	}
	b.outPort = uint32(out)
	// Disposed before the client (defers run last-in first).
	defer C.emmDisposePort(out)
	defer C.emmDisposePort(in)

	b.reconcile()
	lastScan := time.Now()
	for ctx.Err() == nil {
		start := time.Now()
		C.emmRunSlice(C.double(cmRunSlice))
		due := false
		if b.dirty.Load() {
			now := time.Now().UnixNano()
			quiet := time.Duration(now - b.lastNotify.Load())
			burst := time.Duration(now - b.dirtySince.Load())
			if quiet >= cmDebounce || burst >= cmMaxDirty {
				b.dirty.Store(false)
				due = true
			}
		}
		// Notifications are the fast path, but not a guarantee: CoreMIDI was
		// observed to stop delivering them to later clients once a process has
		// created several (a single long-lived client, as in the daemon, was
		// reliable). So also look again every second; reconcile only acts on
		// differences and costs a few dozen cheap calls.
		if due || time.Since(lastScan) >= cmRescan {
			b.reconcile()
			lastScan = time.Now()
		}
		// If the run loop has nothing attached it returns at once; don't spin.
		if el := time.Since(start); el < 20*time.Millisecond {
			time.Sleep(20*time.Millisecond - el)
		}
	}
	b.shutdown()
	return nil
}

// reconcile brings the router's ports in line with CoreMIDI's current state.
func (b *coreMIDI) reconcile() {
	specs := buildSpecs(readGraph(), b.opts.IncludeVirtual)
	want := make(map[string]cmPortSpec, len(specs))
	for _, s := range specs {
		want[s.id] = s
	}
	for id, p := range b.ports {
		if s, ok := want[id]; !ok || !p.spec.same(s) {
			b.removePort(id)
		}
	}
	for _, s := range specs {
		if _, ok := b.ports[s.id]; !ok {
			b.addPort(s)
		}
	}
}

// cmConnectSource subscribes the backend's input port to a source endpoint
// (MIDIPortConnectSource) and returns the OSStatus. A variable only so a test
// can make it fail.
var cmConnectSource = func(inPort, src uint32, token uintptr) int {
	return int(C.emmConnect(C.uint32_t(inPort), C.uint32_t(src), C.uintptr_t(token)))
}

func (b *coreMIDI) addPort(s cmPortSpec) {
	p := &cmPort{
		b: b, spec: s,
		rx:   make(chan []byte, cmRxQueueDepth),
		done: make(chan struct{}),
		info: router.PortInfo{
			ID: s.id, Name: s.name, Kind: s.kind, Group: s.group, Detail: s.detail,
			Input: s.src.Ref != 0, Output: s.dst.Ref != 0,
		},
	}
	// Connect the source before the port is published, so that a failure never
	// shows up in the router as a port that appears and vanishes again. Bytes
	// that arrive before rxLoop runs wait in p.rx.
	if p.info.Input {
		p.token = cmRegister(p)
		if st := cmConnectSource(b.inPort, s.src.Ref, p.token); st != 0 {
			cmRegistry.Delete(p.token)
			p.token = 0
			if s.dst.Ref == 0 {
				b.log.Warn("cannot connect source; not adding input-only port", "port", s.id, "name", s.name, "status", st)
				return
			}
			// Keep the working destination: the port goes up as output-only
			// (and, with spec unchanged, is left alone by later rescans).
			b.log.Warn("cannot connect source; adding port as output-only", "port", s.id, "name", s.name, "status", st)
			p.info.Input = false
		}
	}
	p.handle = b.host.AddPort(p)
	if p.info.Input {
		b.wg.Add(1)
		go p.rxLoop()
	}
	b.ports[s.id] = p
	b.log.Info("port added", "id", s.id, "name", s.name, "in", p.info.Input, "out", p.info.Output)
}

func (b *coreMIDI) removePort(id string) {
	delete(b.ports, id)
	b.host.RemovePort(id) // closes the port
	b.log.Info("port removed", "id", id)
}

func (b *coreMIDI) shutdown() {
	for id := range b.ports {
		b.removePort(id)
	}
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		b.log.Warn("rx goroutines slow to stop")
	}
}

// ---- one router port -------------------------------------------------------

type cmPort struct {
	b      *coreMIDI
	spec   cmPortSpec
	info   router.PortInfo
	handle *router.Handle
	token  uintptr

	rx   chan []byte
	done chan struct{}
	once sync.Once
}

func (p *cmPort) Info() router.PortInfo { return p.info }

// onBytes runs on a CoreMIDI thread. It hands the bytes to rxLoop, waiting a
// little for room (so a SysEx is not torn) but never forever.
func (p *cmPort) onBytes(b []byte) {
	select {
	case p.rx <- b:
		return
	default:
	}
	t := time.NewTimer(cmRxBlockMax)
	defer t.Stop()
	select {
	case p.rx <- b:
	case <-p.done:
	case <-t.C:
		p.b.log.Warn("router too slow; dropping received MIDI", "port", p.info.ID, "bytes", len(b))
	}
}

func (p *cmPort) rxLoop() {
	defer p.b.wg.Done()
	for {
		select {
		case b := <-p.rx:
			p.handle.Receive(b) // single goroutine per port: serialised
		case <-p.done:
			return
		}
	}
}

func (p *cmPort) Write(data []byte) error {
	if p.spec.dst.Ref == 0 {
		return errors.New("coremidi: port has no destination")
	}
	select {
	case <-p.done:
		return errors.New("coremidi: port closed")
	default:
	}
	if len(data) == 0 {
		return nil
	}
	if rc := C.emmSend(C.uint32_t(p.b.outPort), C.uint32_t(p.spec.dst.Ref),
		unsafe.Pointer(&data[0]), C.int(len(data))); rc != 0 {
		return fmt.Errorf("coremidi: MIDISend to %q: OSStatus %d", p.info.Name, int(rc))
	}
	return nil
}

func (p *cmPort) Close() error {
	p.once.Do(func() {
		close(p.done)
		if p.token != 0 {
			cmRegistry.Delete(p.token)
			// Fails harmlessly when the device is already gone.
			C.emmDisconnect(C.uint32_t(p.b.inPort), C.uint32_t(p.spec.src.Ref))
		}
	})
	return nil
}
