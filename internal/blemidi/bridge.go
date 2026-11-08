package blemidi

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"emm/internal/router"
)

// bridge connects the GATT tree to the router: packets written by centrals
// are decoded and fed into the router; router output is encoded, queued,
// paced and handed to BlueZ as notifications. It contains no BlueZ calls, so
// it is tested against a real router.
type bridge struct {
	host     router.Host
	log      *slog.Logger
	tree     *gattTree
	enc      *Encoder
	out      *pktQueue
	interval time.Duration
	// sysexWait is how long a push waits for queue space when only SysEx
	// would have to be sacrificed (see pktQueue).
	sysexWait time.Duration
	handle    atomic.Pointer[router.Handle]
}

func newBridge(host router.Host, log *slog.Logger, tree *gattTree, interval time.Duration) *bridge {
	return &bridge{host: host, log: log, tree: tree, enc: NewEncoder(), out: newPktQueue(512),
		interval: interval, sysexWait: 1500 * time.Millisecond}
}

// bleport is the router port of the Bluetooth adapter.
type bleport struct {
	info router.PortInfo
	br   *bridge
}

func (p *bleport) Info() router.PortInfo { return p.info }
func (p *bleport) Write(b []byte) error  { p.br.send(b); return nil }
func (p *bleport) Close() error          { return nil }

func portInfo(name string) router.PortInfo {
	return router.PortInfo{
		ID: PortID, Name: name, Kind: router.KindBLE,
		Input: true, Output: true, Detail: "Bluetooth LE MIDI",
	}
}

// addPort (re-)registers the router port under the given name.
func (b *bridge) addPort(name string) {
	b.handle.Store(b.host.AddPort(&bleport{info: portInfo(name), br: b}))
}

func (b *bridge) close() {
	b.out.close()
}

// isSysExChunk reports whether a router chunk carries SysEx data: the router
// delivers a SysEx message in chunks, which start with F0, with data bytes (a
// continuation), or with the closing F7 when that arrived alone. Everything
// else (channel messages, realtime bytes, other system messages) is not.
func isSysExChunk(data []byte) bool {
	return len(data) > 0 && (data[0] == 0xF0 || data[0] == 0xF7 || data[0] < 0x80)
}

// send encodes a router chunk and queues the packets for sendLoop. It is
// called from the router's writer goroutine for this port.
func (b *bridge) send(data []byte) {
	if len(data) == 0 || !b.tree.isNotifying() {
		return // nobody is listening
	}
	// SysEx integrity matters: when the queue is full, SysEx waits for room,
	// and other traffic (realtime bytes sent in the middle of a dump, notes
	// replayed after it) may only displace other non-SysEx packets. Otherwise
	// it drops the oldest of those at once rather than holding the router up.
	sx := isSysExChunk(data)
	for _, p := range b.enc.Encode(data, b.tree.effectiveMTU()) {
		if d := b.out.push(p, sx, b.sysexWait); d > 0 {
			b.log.Debug("outgoing BLE-MIDI queue full; dropped packets", "dropped", d, "sysex", sx)
		}
	}
}

// sendLoop hands queued packets to BlueZ, at least `interval` apart.
func (b *bridge) sendLoop(ctx context.Context) {
	var last time.Time
	for {
		p, ok := b.out.pop(ctx)
		if !ok {
			return
		}
		if gap := b.interval - time.Since(last); gap > 0 {
			select {
			case <-time.After(gap):
			case <-ctx.Done():
				return
			}
		}
		b.tree.notify(p)
		last = time.Now()
	}
}

// recvLoop decodes packets written by centrals and feeds the router.
func (b *bridge) recvLoop(ctx context.Context) {
	var dec Decoder
	for {
		p, ok := b.tree.inbound.pop(ctx)
		if !ok {
			return
		}
		h := b.handle.Load()
		if h == nil {
			continue
		}
		dec.Feed(p, func(m []byte) { h.Receive(m) })
	}
}
