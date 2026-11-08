package blemidi

import (
	"time"

	"emm/internal/midi"
)

// BLE-MIDI GATT identifiers (MMA/AMEI RP-052), lower case as BlueZ reports them.
const (
	ServiceUUID        = "03b80e5a-ede8-4b33-a751-6ce34ec4c700"
	CharacteristicUUID = "7772e5db-3868-4112-a1a9-f2669d106bf3"
)

const (
	// DefaultMTU is the ATT MTU every BLE link starts with.
	DefaultMTU = 23
	// attOverhead is the ATT header of a notification (opcode + handle).
	attOverhead = 3
	// maxPacket is the largest attribute value ATT allows (512 bytes).
	maxPacket = 512
	// timestampMask keeps the 13 timestamp bits.
	timestampMask = 0x1FFF
)

// dataLen returns how many data bytes follow a status byte.
func dataLen(status byte) int {
	switch {
	case status < 0xC0:
		return 2
	case status < 0xE0:
		return 1
	case status < 0xF0:
		return 2
	}
	switch status {
	case 0xF1, 0xF3:
		return 1
	case 0xF2:
		return 2
	}
	return 0
}

// PacketLimit returns the largest BLE-MIDI packet that fits one notification
// for a link with the given ATT MTU: mtu-3, but never more than 512 and never
// less than the 20 bytes every link has.
func PacketLimit(mtu int) int {
	if mtu < DefaultMTU {
		mtu = DefaultMTU
	}
	l := mtu - attOverhead
	if l > maxPacket {
		l = maxPacket
	}
	return l
}

// ---- decoder ---------------------------------------------------------------

// Decoder turns BLE-MIDI packets into a plain MIDI byte stream.
//
// It follows the rules of the specification:
//   - the first byte of a packet is a header (bit 7 set, bit 6 clear);
//   - a byte with bit 7 set is a timestamp unless the previous byte was a
//     timestamp, in which case it is a status byte;
//   - data bytes straight after a complete message (or after a timestamp) are
//     running status; running status ends with the packet but is not cancelled
//     by system common or realtime messages;
//   - a SysEx may continue in later packets, whose first byte after the
//     header is a data byte (no timestamp); a timestamp precedes F7; realtime
//     messages may interrupt a SysEx.
//
// Output messages are always complete (running status expanded). Malformed
// input never panics; it is dropped or tolerated. A Decoder is not safe for
// concurrent use.
type Decoder struct {
	inSysex bool
	run     byte // running status within the current packet, 0 = none
	msg     [3]byte
	n, need int
	sx      []byte
}

// Reset forgets all state (e.g. after a central disconnects).
func (d *Decoder) Reset() {
	d.inSysex, d.run, d.n, d.need = false, 0, 0, 0
	d.sx = d.sx[:0]
}

// Feed decodes one packet. emit is called with freshly allocated slices that
// the callee may keep: one complete channel/system/realtime message, or one
// piece of a SysEx (the first piece starts with F0, the last ends with F7).
// Timestamps are discarded.
func (d *Decoder) Feed(pkt []byte, emit func(midiBytes []byte)) {
	if len(pkt) == 0 || pkt[0]&0x80 == 0 {
		return // not a BLE-MIDI packet
	}
	// A partial channel message and running status never span packets.
	d.run, d.n, d.need = 0, 0, 0
	haveTS := false
	for _, b := range pkt[1:] {
		if b&0x80 != 0 {
			if !haveTS {
				haveTS = true
				continue
			}
			haveTS = false
			d.status(b, emit)
			continue
		}
		haveTS = false
		d.data(b, emit)
	}
	d.flushSysex(emit)
}

func (d *Decoder) flushSysex(emit func([]byte)) {
	if len(d.sx) > 0 {
		emit(append([]byte(nil), d.sx...))
		d.sx = d.sx[:0]
	}
}

func (d *Decoder) status(b byte, emit func([]byte)) {
	switch {
	case b >= 0xF8: // realtime: interrupts anything, disturbs nothing
		d.flushSysex(emit)
		emit([]byte{b})

	case b == 0xF0:
		if d.inSysex { // previous message was never terminated
			d.flushSysex(emit)
		}
		d.inSysex = true
		d.sx = append(d.sx[:0], 0xF0)
		d.run, d.n, d.need = 0, 0, 0

	case b == 0xF7:
		if d.inSysex {
			d.sx = append(d.sx, 0xF7)
			d.flushSysex(emit)
			d.inSysex = false
		}
		d.n, d.need = 0, 0

	default: // channel or system common
		if d.inSysex { // any other status byte ends a SysEx
			d.flushSysex(emit)
			d.inSysex = false
		}
		if b < 0xF0 {
			d.run = b
		}
		d.msg[0], d.n, d.need = b, 1, dataLen(b)
		if d.need == 0 {
			emit([]byte{b})
			d.n = 0
		}
	}
}

func (d *Decoder) data(b byte, emit func([]byte)) {
	if d.inSysex {
		d.sx = append(d.sx, b)
		return
	}
	if d.n == 0 { // no message under way: running status
		if d.run == 0 {
			return
		}
		d.msg[0], d.n, d.need = d.run, 1, dataLen(d.run)
	}
	d.msg[d.n] = b
	d.n++
	d.need--
	if d.need == 0 {
		emit(append([]byte(nil), d.msg[:d.n]...))
		d.n = 0
	}
}

// ---- encoder ---------------------------------------------------------------

// Encoder turns router chunks into BLE-MIDI packets.
//
// The input of Encode is a MIDI byte stream in router form: complete messages
// with explicit status bytes (running status is tolerated and expanded), or
// pieces of a SysEx (the first starts with F0, later ones are plain data
// bytes, the last ends with F7). The encoder keeps SysEx state across calls.
// It never uses running status on the wire, which the specification allows.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	// Clock supplies the time for timestamps (default time.Now). Only
	// millisecond differences matter; the value is reduced to 13 bits.
	Clock func() time.Time

	parser *midi.Parser
	start  time.Time
}

// NewEncoder returns an Encoder using the wall clock.
func NewEncoder() *Encoder {
	p := midi.NewParser()
	p.MaxChunk = 1 << 20 // do not re-chunk: pieces are packetised below
	return &Encoder{parser: p, start: time.Now()}
}

// Reset forgets partial SysEx state.
func (e *Encoder) Reset() { e.parser.Reset() }

func (e *Encoder) nowMs() uint16 {
	if e.Clock != nil {
		return uint16(e.Clock().UnixMilli() & timestampMask)
	}
	return uint16(time.Since(e.start).Milliseconds() & timestampMask)
}

// Encode packetises data for a link with the given ATT MTU. Every returned
// packet is at most PacketLimit(mtu) bytes. All messages of one call share one
// timestamp.
func (e *Encoder) Encode(data []byte, mtu int) [][]byte {
	b := &packetBuilder{limit: PacketLimit(mtu), ms: e.nowMs()}
	e.parser.Feed(data, func(ev midi.Event) {
		if !ev.SysEx {
			b.message(ev.Data)
			return
		}
		body := ev.Data
		end := false
		if ev.Last && len(body) > 0 && body[len(body)-1] == 0xF7 {
			body, end = body[:len(body)-1], true
		}
		if ev.First && len(body) > 0 { // body[0] == F0
			b.sysexStart()
			body = body[1:]
		}
		b.sysexData(body)
		if end {
			b.sysexEnd()
		}
	})
	return b.finish()
}

type packetBuilder struct {
	limit int
	ms    uint16
	pkts  [][]byte
	cur   []byte
}

func (b *packetBuilder) header() byte { return 0x80 | byte(b.ms>>7)&0x3F }
func (b *packetBuilder) stamp() byte  { return 0x80 | byte(b.ms&0x7F) }

func (b *packetBuilder) flush() {
	if len(b.cur) > 1 {
		b.pkts = append(b.pkts, b.cur)
	}
	b.cur = nil
}

// ensure makes room for n more bytes, starting a new packet if necessary.
func (b *packetBuilder) ensure(n int) {
	if b.cur != nil && len(b.cur)+n > b.limit {
		b.flush()
	}
	if b.cur == nil {
		b.cur = make([]byte, 1, b.limit)
		b.cur[0] = b.header()
	}
}

func (b *packetBuilder) message(msg []byte) {
	b.ensure(1 + len(msg))
	b.cur = append(b.cur, b.stamp())
	b.cur = append(b.cur, msg...)
}

func (b *packetBuilder) sysexStart() {
	b.ensure(2)
	b.cur = append(b.cur, b.stamp(), 0xF0)
}

// sysexData appends SysEx data bytes, spilling into continuation packets
// (header + data, no timestamp) as needed.
func (b *packetBuilder) sysexData(body []byte) {
	for len(body) > 0 {
		b.ensure(1)
		room := b.limit - len(b.cur)
		n := len(body)
		if n > room {
			n = room
		}
		b.cur = append(b.cur, body[:n]...)
		body = body[n:]
	}
}

func (b *packetBuilder) sysexEnd() {
	b.ensure(2)
	b.cur = append(b.cur, b.stamp(), 0xF7)
}

func (b *packetBuilder) finish() [][]byte {
	b.flush()
	return b.pkts
}
