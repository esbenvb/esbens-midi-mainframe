//go:build darwin && cgo

package device

// Virtual CoreMIDI endpoints: one source and one destination with the same
// name, owned by a private client, the way a typical MIDI app presents itself.
//
// Production code does not use this today. It exists because cgo cannot be used
// from _test.go files: the loopback test needs a counterpart to talk to, and
// this is it. It is also the building block for a future "EMM appears as a
// virtual MIDI port on this Mac" feature.

/*
#cgo CFLAGS: -Wno-deprecated-declarations -Wno-unused-function
#cgo LDFLAGS: -framework CoreMIDI -framework CoreFoundation
#include <CoreMIDI/CoreMIDI.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdint.h>
#include <stdlib.h>

extern void goCMRead(uintptr_t token, void *data, int n);

// Read proc of the virtual *destination*: the token travels as readRefCon.
static void emmVirtReadProc(const MIDIPacketList *pl, void *readRefCon, void *srcRefCon) {
	const MIDIPacket *p = &pl->packet[0];
	for (UInt32 i = 0; i < pl->numPackets; i++) {
		if (p->length > 0) {
			goCMRead((uintptr_t)readRefCon, (void *)p->data, (int)p->length);
		}
		p = MIDIPacketNext(p);
	}
}

static int emmVirtCreate(const char *name, uintptr_t token, uint32_t *client, uint32_t *src, uint32_t *dst) {
	CFStringRef s = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
	MIDIClientRef c = 0;
	MIDIEndpointRef so = 0, de = 0;
	OSStatus st = MIDIClientCreate(s, NULL, NULL, &c);
	if (st == noErr) st = MIDISourceCreate(c, s, &so);
	if (st == noErr) st = MIDIDestinationCreate(c, s, emmVirtReadProc, (void *)token, &de);
	CFRelease(s);
	if (st != noErr) {
		if (so) MIDIEndpointDispose(so);
		if (c) MIDIClientDispose(c);
		c = so = de = 0;
	}
	*client = (uint32_t)c;
	*src = (uint32_t)so;
	*dst = (uint32_t)de;
	return (int)st;
}

// Emits data from the virtual source as packets of at most 256 bytes.
static int emmVirtEmit(uint32_t src, const void *data, int n) {
	Byte buf[512];
	int off = 0;
	while (off < n) {
		int len = n - off;
		if (len > 256) len = 256;
		MIDIPacketList *pl = (MIDIPacketList *)buf;
		MIDIPacket *pkt = MIDIPacketListInit(pl);
		pkt = MIDIPacketListAdd(pl, sizeof(buf), pkt, 0, (ByteCount)len, (const Byte *)data + off);
		if (!pkt) return -1;
		OSStatus st = MIDIReceived((MIDIEndpointRef)src, pl);
		if (st != noErr) return (int)st;
		off += len;
	}
	return 0;
}

static void emmVirtDispose(uint32_t client, uint32_t src, uint32_t dst) {
	if (src) MIDIEndpointDispose((MIDIEndpointRef)src);
	if (dst) MIDIEndpointDispose((MIDIEndpointRef)dst);
	if (client) MIDIClientDispose((MIDIClientRef)client);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// cmVirtual is a virtual source+destination pair named name. Everything sent to
// the destination is collected and available through Received.
type cmVirtual struct {
	name             string
	client, src, dst uint32
	token            uintptr

	mu   sync.Mutex
	got  []byte
	once sync.Once
}

func newCMVirtual(name string) (*cmVirtual, error) {
	v := &cmVirtual{name: name}
	v.token = cmRegister(v)
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var c, s, d C.uint32_t
	if st := C.emmVirtCreate(cname, C.uintptr_t(v.token), &c, &s, &d); st != 0 {
		cmRegistry.Delete(v.token)
		return nil, fmt.Errorf("coremidi: creating virtual endpoints: OSStatus %d", int(st))
	}
	v.client, v.src, v.dst = uint32(c), uint32(s), uint32(d)
	return v, nil
}

func (v *cmVirtual) onBytes(b []byte) {
	v.mu.Lock()
	v.got = append(v.got, b...)
	v.mu.Unlock()
}

// Received returns a copy of everything the destination has received so far.
func (v *cmVirtual) Received() []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]byte(nil), v.got...)
}

// Emit sends data out of the virtual source, as if a connected app had played it.
func (v *cmVirtual) Emit(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if st := C.emmVirtEmit(C.uint32_t(v.src), unsafe.Pointer(&data[0]), C.int(len(data))); st != 0 {
		return fmt.Errorf("coremidi: MIDIReceived: OSStatus %d", int(st))
	}
	return nil
}

// Close disposes the endpoints and the client. Safe to call more than once.
func (v *cmVirtual) Close() {
	v.once.Do(func() {
		cmRegistry.Delete(v.token)
		C.emmVirtDispose(C.uint32_t(v.client), C.uint32_t(v.src), C.uint32_t(v.dst))
	})
}
