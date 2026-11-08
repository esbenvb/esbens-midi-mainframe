package rtpmidi

import (
	"crypto/rand"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Timers. Variables only so that tests can shorten them.
var (
	// peerTimeout: no packet at all from a peer for this long and it is dropped.
	// Apple's initiators send a clock sync at least every 60 s, so this leaves
	// them a comfortable margin.
	peerTimeout     = 90 * time.Second
	reapInterval    = time.Second            // how often the responder checks timeouts / sends RS
	ckInterval      = 10 * time.Second       // initiator: CK0 period once synchronised
	handshakeTry    = time.Second            // initiator: resend IN after this long
	handshakeTries  = 10                     // initiator: give up after this many IN attempts per port
	pendingTimeout  = 10 * time.Second       // responder: half-open invitation lifetime
	maxPeers        = 16                     // responder: established sessions per endpoint
	maxPending      = 32                     // responder: half-open invitations per endpoint
	maxPendingPerIP = 4                      // responder: half-open invitations per source address
	lateWindow      = 64                     // sequence numbers behind this far count as duplicates
	writeTimeout    = 100 * time.Millisecond // Send never waits longer than this for the socket
	logEvery        = 10 * time.Second       // rate limit of log lines a stranger on the network can provoke
)

// smallDelivery is the size up to which a delivery to the Handler is not paid
// for from the SysEx budget: whatever one packet can carry is smaller than this
// (a list is at most 4095 bytes; running status expands it to at most 6142).
// Only reassembled SysEx can be bigger.
const smallDelivery = 8 << 10

// Config configures a Server or Client.
type Config struct {
	Name        string // session name shown to peers
	ControlPort int    // UDP control port; data port = ControlPort+1. 0 = pick a free adjacent pair
	BindAddr    string // "" = all interfaces
	SSRC        uint32 // 0 = random
	// Journal is reserved for the sender-side recovery journal (RFC 6295 §5).
	// NOT IMPLEMENTED: setting it has no effect (see journal.go).
	Journal bool
	// SysexBudget pays for SysEx reassembly and for reassembled SysEx waiting to
	// be delivered. nil means the process-wide SysexBudget.
	SysexBudget *ByteBudget
	// ReleaseNotesOnLoss makes the receive side send Note Off for every note a
	// peer is holding when packets from it are lost, or when it leaves (BY,
	// timeout). Without the recovery journal this is what keeps a lost Note Off
	// from leaving a note stuck forever. The Manager turns it on.
	ReleaseNotesOnLoss bool
	Logger             *slog.Logger
}

// PeerInfo describes a connected peer.
type PeerInfo struct {
	Name  string
	Addr  string
	SSRC  uint32
	Since time.Time
}

// Handler receives what the peers send.
type Handler struct {
	// OnMIDI gets a plain MIDI byte stream: explicit status bytes, SysEx
	// reassembled to F0…F7, no timestamps. One call may hold several commands.
	// Calls are made one at a time, in order, from a dedicated goroutine, and
	// may block: back-pressure reaches the UDP socket buffer.
	OnMIDI func(p PeerInfo, data []byte)
	// OnPeers is called (without locks held) when the peer set changes.
	OnPeers func()
}

// clock is the session clock: 100 µs ticks since creation (10 kHz, as in AppleMIDI).
type clock struct{ t0 time.Time }

func newClock() clock { return clock{t0: time.Now()} }

func (c clock) ticks() uint64 { return uint64(time.Since(c.t0) / (100 * time.Microsecond)) }

// link is one established association with a peer (either role).
type link struct {
	name  string
	ssrc  uint32
	token uint32
	ctrl  *net.UDPAddr // peer's control endpoint
	data  *net.UDPAddr // peer's data endpoint
	host  net.IP       // the machine the peer is on; packets for this session must come from it
	since time.Time

	// seen is when the last packet from the peer arrived. A time.Time taken from
	// time.Now carries a monotonic clock reading, which is what makes idle()
	// immune to the wall clock being stepped (NTP at boot on a Pi without RTC).
	seenMu sync.Mutex
	seen   time.Time

	// rxMu orders the receive side of the link against its retirement: the data
	// goroutine holds it from decoding a packet until the result is queued for
	// delivery, and retire (BY, timeout, replacement, Close) takes it to release
	// held notes behind whatever was already queued. gone is set by retire.
	rxMu sync.Mutex
	gone bool

	txMu  sync.Mutex
	txSeq uint16

	rx rxState // used only by the goroutine reading the data socket

	offset atomic.Int64 // peer clock minus ours, in ticks (valid when synced)
	synced atomic.Bool

	dirty   atomic.Bool   // data received since the last RS
	rxSeq   atomic.Uint32 // highest sequence number received
	journal *journalState // nil unless Config.Journal

	held          heldNotes // notes the peer currently holds (as decoded)
	releaseOnLoss bool
}

// heldNotes tracks which notes a peer has switched on.
type heldNotes struct {
	mu sync.Mutex
	on [16][16]byte // bit per note per channel
}

// observe updates the tracker from a plain MIDI byte stream with explicit status bytes.
func (h *heldNotes) observe(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0xF0:
			j := i + 1
			for j < len(b) && b[j] != 0xF7 {
				j++
			}
			i = j + 1
		case c >= 0xF8:
			i++
		case c >= 0xF0:
			i += 1 + dataLen(c)
		case c >= 0x80:
			n := dataLen(c)
			if i+n < len(b) && (c&0xF0 == 0x90 || c&0xF0 == 0x80) {
				ch, note := c&0x0F, b[i+1]&0x7F
				if c&0xF0 == 0x90 && b[i+2] != 0 {
					h.on[ch][note>>3] |= 1 << (note & 7)
				} else {
					h.on[ch][note>>3] &^= 1 << (note & 7)
				}
			}
			i += 1 + n
		default:
			i++
		}
	}
}

// release returns Note Off messages for every held note and forgets them.
func (h *heldNotes) release() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []byte
	for ch := 0; ch < 16; ch++ {
		for n := 0; n < 128; n++ {
			if h.on[ch][n>>3]&(1<<(uint(n)&7)) != 0 {
				out = append(out, 0x80|byte(ch), byte(n), 0)
			}
		}
		h.on[ch] = [16]byte{}
	}
	return out
}

type rxState struct {
	dec    listDecoder
	have   bool
	expect uint16
	lost   uint64
}

func newLink(name string, ssrc, token uint32, ctrl, data *net.UDPAddr, journal bool) *link {
	l := &link{name: name, ssrc: ssrc, token: token, ctrl: ctrl, data: data, since: time.Now(),
		txSeq: uint16(randUint32())}
	switch {
	case data != nil:
		l.host = data.IP
	case ctrl != nil:
		l.host = ctrl.IP
	}
	l.rx.dec.budget = SysexBudget
	l.touch()
	if journal {
		l.journal = newJournalState()
	}
	return l
}

func (l *link) touch() {
	now := time.Now()
	l.seenMu.Lock()
	l.seen = now
	l.seenMu.Unlock()
}

func (l *link) lastSeen() time.Time {
	l.seenMu.Lock()
	defer l.seenMu.Unlock()
	return l.seen
}

// idle is how long ago the last packet arrived, on the monotonic clock.
func (l *link) idle() time.Duration { return time.Since(l.lastSeen()) }

// fromHost reports whether a packet came from the machine this session was set
// up with. Only the address is compared, not the port: the peer's control and
// data sockets differ, and a NAT may remap either.
func (l *link) fromHost(from *net.UDPAddr) bool {
	return from != nil && l.host != nil && l.host.Equal(from.IP)
}

// retire ends the receive side of the link: packets that already passed the
// lookup are dropped from now on, and the SysEx half-received so far goes back
// to the budget. With notes set and ReleaseNotesOnLoss on, it returns Note Offs
// for the notes the peer still holds; the caller queues them. retire waits for
// a packet being delivered, so the Offs always come after the Ons.
func (l *link) retire(notes bool, queue func(offs []byte)) {
	l.rxMu.Lock()
	defer l.rxMu.Unlock()
	l.gone = true
	l.rx.dec.discard()
	if notes && l.releaseOnLoss && queue != nil {
		if offs := l.held.release(); len(offs) > 0 {
			queue(offs)
		}
	}
}

func (l *link) info() PeerInfo {
	return PeerInfo{Name: l.name, Addr: l.data.String(), SSRC: l.ssrc, Since: l.since}
}

func (l *link) nextSeq() uint16 {
	l.txMu.Lock()
	defer l.txMu.Unlock()
	s := l.txSeq
	l.txSeq++
	return s
}

// receive processes one RTP-MIDI packet from the peer and returns the plain
// MIDI bytes it carried (nil if none).
func (l *link) receive(h rtpHeader, payload []byte, log *slog.Logger) []byte {
	var released []byte
	if l.rx.have {
		diff := int(int16(h.seq - l.rx.expect))
		switch {
		case diff < 0 && diff >= -lateWindow:
			return nil // duplicate or reordered: already past it
		case diff != 0:
			// Packets were lost (or the peer jumped): whatever was half-received
			// is not trustworthy any more, and a lost Note Off would leave a note
			// stuck, so let go of everything the peer was holding.
			if diff > 0 {
				l.rx.lost += uint64(diff)
			}
			log.Debug("rtp sequence gap", "peer", l.name, "missing", diff)
			l.rx.dec.reset()
			if l.releaseOnLoss {
				released = l.held.release()
			}
		}
	}
	l.rx.have, l.rx.expect = true, h.seq+1
	l.rxSeq.Store(uint32(h.seq))
	l.dirty.Store(true)

	sec, err := parseSection(payload)
	if err != nil {
		log.Debug("bad RTP-MIDI payload", "peer", l.name, "err", err)
		return released
	}
	out, err := l.rx.dec.decode(sec.list, sec.z, nil)
	if err != nil {
		log.Debug("bad MIDI command list", "peer", l.name, "err", err)
	}
	if l.releaseOnLoss {
		l.held.observe(out)
	}
	if len(released) > 0 {
		out = append(released, out...)
	}
	return out
}

func addrEqual(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}

// packetsFor builds the RTP packets carrying lists to link l.
func (l *link) packetsFor(lists [][]byte, ssrc uint32, ts uint32) [][]byte {
	pkts := make([][]byte, 0, len(lists))
	for _, list := range lists {
		seq := l.nextSeq()
		var jr []byte
		if l.journal != nil {
			jr = l.journal.build(seq, list)
		}
		sec := buildSection(list, jr)
		pkt := rtpHeader{marker: true, pt: payloadType, seq: seq, ts: ts, ssrc: ssrc}.appendTo(make([]byte, 0, rtpHeaderLen+len(sec)))
		pkts = append(pkts, append(pkt, sec...))
	}
	return pkts
}

func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
