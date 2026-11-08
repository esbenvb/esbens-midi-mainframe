package rtpmidi

import (
	"errors"
	"fmt"

	"emm/internal/midi"
)

// RTP-MIDI command section flags (RFC 6295 §3).
const (
	secB = 0x80 // 12-bit length
	secJ = 0x40 // recovery journal follows the command list
	secZ = 0x20 // the first command is preceded by a delta time
	secP = 0x10 // phantom status: first command uses running status from the previous packet

	// maxListBytes bounds one packet's MIDI command list, which keeps whole
	// UDP datagrams around 1.3 kB (RTP header 12 + section header 2 + list).
	maxListBytes = 1300
)

// maxSysexReassembly caps how much SysEx is buffered for one message. A
// variable only so that tests can lower it. On top of this, every byte of
// buffer is paid for from a shared ByteBudget (SysexBudget).
var maxSysexReassembly = 16 << 20

var errShort = errors.New("rtpmidi: truncated MIDI command section")

// appendVLQ appends v (< 2^28) as an RTP-MIDI delta time.
func appendVLQ(dst []byte, v uint32) []byte {
	if v > 0x0FFFFFFF {
		v = 0x0FFFFFFF
	}
	var tmp [4]byte
	n := 1
	tmp[0] = byte(v & 0x7F)
	for v >>= 7; v > 0; v >>= 7 {
		tmp[n] = byte(v&0x7F) | 0x80
		n++
	}
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, tmp[i])
	}
	return dst
}

// readVLQ reads a delta time, returning the value and the bytes consumed.
func readVLQ(b []byte) (uint32, int, bool) {
	var v uint32
	for i := 0; i < len(b) && i < 4; i++ {
		v = v<<7 | uint32(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

// buildSection wraps a command list (and optional recovery journal) in the
// RTP-MIDI MIDI command section header. The Z and P flags are never set by us.
func buildSection(list, journal []byte) []byte {
	var flags byte
	if len(journal) > 0 {
		flags |= secJ
	}
	n := len(list)
	out := make([]byte, 0, 2+n+len(journal))
	if n <= 15 {
		out = append(out, flags|byte(n))
	} else {
		out = append(out, flags|secB|byte(n>>8)&0x0F, byte(n))
	}
	out = append(out, list...)
	out = append(out, journal...)
	return out
}

type section struct {
	list    []byte
	journal bool // the packet carries a journal after the list (we never read it)
	z, p    bool
}

// parseSection reads the MIDI command section header. Anything after the
// command list is the recovery journal and is deliberately ignored.
func parseSection(b []byte) (section, error) {
	if len(b) < 1 {
		return section{}, errShort
	}
	h := b[0]
	n := int(h & 0x0F)
	i := 1
	if h&secB != 0 {
		if len(b) < 2 {
			return section{}, errShort
		}
		n = n<<8 | int(b[1])
		i = 2
	}
	if len(b) < i+n {
		return section{}, errShort
	}
	return section{list: b[i : i+n], journal: h&secJ != 0, z: h&secZ != 0, p: h&secP != 0}, nil
}

// dataLen returns the number of data bytes following a status byte.
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

// listDecoder decodes MIDI command lists of one peer into a plain MIDI byte
// stream (explicit status bytes, SysEx reassembled to F0…F7). It keeps the
// running status and half-received SysEx between packets.
type listDecoder struct {
	running byte
	sx      []byte
	inSx    bool
	// Dropped counts SysEx messages discarded (cancelled, orphaned, too big or
	// out of budget).
	dropped uint64

	// budget pays for sx (its capacity); nil means unmetered, which only
	// decoders used on their own in tests are. held is what is charged now.
	budget *ByteBudget
	held   int64
	// starved is set when a message was dropped for lack of budget; the owner
	// reads and clears it (takeStarved) to log that, rate limited.
	starved bool
}

// reset forgets the running status and any half-received SysEx (a sequence
// gap: nothing received so far can be trusted).
func (d *listDecoder) reset() {
	d.running = 0
	if d.inSx {
		d.dropped++
	}
	d.dropSx()
}

// discard throws everything away and gives the budget back: the session is
// over. It is not a drop of a message the peer finished sending.
func (d *listDecoder) discard() {
	d.running = 0
	d.dropSx()
}

// dropSx abandons the SysEx in progress and returns its memory to the budget.
func (d *listDecoder) dropSx() {
	if d.budget != nil && d.held > 0 {
		d.budget.release(d.held)
	}
	d.held = 0
	d.sx, d.inSx = nil, false
}

// takeStarved reports (once) that a message was dropped for lack of budget.
func (d *listDecoder) takeStarved() bool {
	s := d.starved
	d.starved = false
	return s
}

// grow makes room for n more bytes in sx. The budget is asked before the
// allocation, for the new buffer in full: while it is copied the old one is
// still alive. It reports false, with nothing changed, when the message is
// too big or the budget is used up.
func (d *listDecoder) grow(n int) bool {
	need := len(d.sx) + n
	if need > maxSysexReassembly {
		return false
	}
	if need <= cap(d.sx) {
		return true
	}
	c := cap(d.sx) + cap(d.sx)/2
	if c < need {
		c = need
	}
	if c < sxChunk {
		c = sxChunk
	}
	c = (c + sxChunk - 1) &^ (sxChunk - 1)
	if c > maxSysexReassembly {
		c = maxSysexReassembly
	}
	if c < need { // maxSysexReassembly is not a multiple of sxChunk
		c = need
	}
	if d.budget != nil {
		if !d.budget.reserve(int64(c)) {
			d.starved = true
			return false
		}
		d.budget.release(d.held)
		d.held = int64(c)
	}
	ns := make([]byte, len(d.sx), c)
	copy(ns, d.sx)
	d.sx = ns
	return true
}

// sxChunk is the granularity of the reassembly buffer (a power of two).
const sxChunk = 4096

// appendSx adds b to the SysEx in progress, if there is room for it.
func (d *listDecoder) appendSx(b []byte) bool {
	if !d.grow(len(b)) {
		return false
	}
	d.sx = append(d.sx, b...)
	return true
}

var errBadList = errors.New("rtpmidi: malformed MIDI command list")

// allData reports whether every octet of b is a data octet (MSB clear).
func allData(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// decode appends the plain MIDI bytes for one packet's command list to out.
// On error the bytes decoded so far are still returned.
func (d *listDecoder) decode(list []byte, z bool, out []byte) ([]byte, error) {
	i := 0
	for first := true; i < len(list); first = false {
		if !first || z {
			_, n, ok := readVLQ(list[i:])
			if !ok {
				d.running = 0
				return out, fmt.Errorf("%w: bad delta time", errBadList)
			}
			i += n
			if i >= len(list) {
				d.running = 0
				return out, fmt.Errorf("%w: delta time without command", errBadList)
			}
		}
		c := list[i]
		switch {
		case c < 0x80: // running status
			if d.running == 0 {
				return out, fmt.Errorf("%w: data byte without running status", errBadList)
			}
			need := dataLen(d.running)
			if i+need > len(list) {
				d.running = 0
				return out, fmt.Errorf("%w: truncated command", errBadList)
			}
			// The data octets of a running-status command are checked like those
			// of an explicit one: an F0 or a realtime byte smuggled in here would
			// get past SysEx reassembly. (list[i] is below 0x80 already.)
			if !allData(list[i : i+need]) {
				d.running = 0
				return out, fmt.Errorf("%w: status byte inside command", errBadList)
			}
			out = append(out, d.running)
			out = append(out, list[i:i+need]...)
			i += need

		case c >= 0xF8: // realtime: does not touch running status
			out = append(out, c)
			i++

		case c == 0xF0 || c == 0xF7:
			j := i + 1
			for j < len(list) && list[j] < 0x80 {
				j++
			}
			if j >= len(list) {
				d.running = 0
				return out, fmt.Errorf("%w: unterminated SysEx segment", errBadList)
			}
			if t := list[j]; t != 0xF0 && t != 0xF7 && t != 0xF4 {
				d.running = 0
				return out, fmt.Errorf("%w: SysEx segment ends with 0x%02X", errBadList, t)
			}
			out = d.segment(list[i:j+1], out)
			d.running = 0
			i = j + 1

		default: // channel message or system common
			need := dataLen(c)
			if i+1+need > len(list) {
				d.running = 0
				return out, fmt.Errorf("%w: truncated command", errBadList)
			}
			for _, b := range list[i+1 : i+1+need] {
				if b >= 0x80 {
					d.running = 0
					return out, fmt.Errorf("%w: status byte inside command", errBadList)
				}
			}
			if c < 0xF0 {
				d.running = c
			} else {
				d.running = 0
			}
			out = append(out, list[i:i+1+need]...)
			i += 1 + need
		}
	}
	return out, nil
}

// segment handles one SysEx command segment (RFC 6295 §3.2):
//
//	complete F0 … F7      first  F0 … F0
//	middle   F7 … F0      last   F7 … F7      cancel F7 … F4
func (d *listDecoder) segment(seg, out []byte) []byte {
	first, last := seg[0], seg[len(seg)-1]
	body := seg[1:]
	switch {
	case first == 0xF0 && last == 0xF7:
		if d.inSx {
			d.dropped++
			d.dropSx()
		}
		return append(out, seg...)

	case first == 0xF0 && last == 0xF0 && len(seg) >= 2:
		if d.inSx {
			d.dropped++
		}
		d.dropSx()
		if !d.appendSx(seg[:1]) || !d.appendSx(body[:len(body)-1]) {
			d.refuse()
			return out
		}
		d.inSx = true

	case first == 0xF7 && last == 0xF0 && len(seg) >= 2:
		if !d.inSx {
			d.dropped++ // continuation of something we never saw the start of
			return out
		}
		if !d.appendSx(body[:len(body)-1]) {
			d.refuse()
		}

	case first == 0xF7 && last == 0xF7 && len(seg) >= 2:
		if !d.inSx {
			d.dropped++
			return out
		}
		if !d.appendSx(body) {
			d.refuse()
			return out
		}
		out = append(out, d.sx...)
		d.dropSx() // the copy in out is paid for by whoever delivers it
		return out

	case first == 0xF7 && last == 0xF4:
		if d.inSx {
			d.dropped++
		}
		d.dropSx()

	default: // "F0 F4", a lone octet, … nothing sensible to do
		if d.inSx {
			d.dropped++
		}
		d.dropSx()
	}
	return out
}

// refuse drops the message in progress: it grew past the per-message cap or
// the budget is used up. The peer's remaining segments of it are orphans and
// are counted and ignored like any continuation without a start.
func (d *listDecoder) refuse() {
	d.dropped++
	d.dropSx()
}

// encoder turns the router's byte stream into RTP-MIDI commands. It is
// stateful across calls because SysEx arrives in chunks: a chunk that does not
// end the message becomes a segment ending in F0, and the next chunk a segment
// starting with F7.
type encoder struct {
	parser *midi.Parser
	sxOpen bool // a first/middle segment has been emitted; the message is unfinished
	// sxHead: the message has begun (an F0 was seen) but nothing was emitted,
	// because a first segment must carry at least one data octet (RFC 6295
	// §3.2). The F0 goes out in front of the next piece of the message.
	sxHead bool
}

func newEncoder() *encoder {
	p := midi.NewParser()
	p.MaxChunk = 1 << 20 // chunking is done by the segment splitter below
	return &encoder{parser: p}
}

// maxSegBody is the largest SysEx body one segment carries: it must fit in a
// list together with its two terminator octets and a delta time.
const maxSegBody = maxListBytes - 3

// encode feeds raw MIDI bytes and returns complete RTP-MIDI commands.
func (e *encoder) encode(data []byte) [][]byte {
	var cmds [][]byte
	e.parser.Feed(data, func(ev midi.Event) {
		if !ev.SysEx {
			cmds = append(cmds, append([]byte(nil), ev.Data...))
			return
		}
		cmds = e.sysex(ev, cmds)
	})
	return cmds
}

func (e *encoder) sysex(ev midi.Event, cmds [][]byte) [][]byte {
	d := ev.Data
	if ev.First {
		if e.sxOpen { // cannot happen with the parser, but never leave a peer hanging
			cmds = append(cmds, []byte{0xF7, 0xF4})
		}
		e.sxOpen, e.sxHead = false, false
		d = d[1:] // the F0 is implied by the segment type
	} else if !e.sxOpen && !e.sxHead {
		return cmds // continuation of a message whose start we never sent
	}
	complete := ev.Last && len(ev.Data) > 0 && ev.Data[len(ev.Data)-1] == 0xF7
	if complete {
		d = d[:len(d)-1]
	}
	if ev.Last && !complete { // sender aborted the message
		if e.sxOpen { // a peer that never saw the start has nothing to cancel
			cmds = append(cmds, []byte{0xF7, 0xF4})
		}
		e.sxOpen, e.sxHead = false, false
		return cmds
	}
	if len(d) == 0 && !complete {
		// Nothing to carry: just the F0 so far (a realtime byte or the end of a
		// read came right after it). Emitting it as a segment of its own would be
		// the illegal "F0 F0", so hold it back.
		if !e.sxOpen {
			e.sxHead = true
		}
		return cmds
	}
	for {
		start := byte(0xF7)
		if !e.sxOpen {
			start = 0xF0
		}
		piece := d
		more := false
		if len(piece) > maxSegBody {
			piece, more = piece[:maxSegBody], true
		}
		end := byte(0xF0)
		if complete && !more {
			end = 0xF7
		}
		seg := make([]byte, 0, len(piece)+2)
		seg = append(seg, start)
		seg = append(seg, piece...)
		seg = append(seg, end)
		cmds = append(cmds, seg)
		e.sxOpen, e.sxHead = end == 0xF0, false
		if !more {
			return cmds
		}
		d = d[len(piece):]
	}
}

// packLists groups commands into command lists no longer than maxListBytes.
// Within a list every command after the first is preceded by a zero delta time
// (the first command needs none: Z=0).
func packLists(cmds [][]byte) [][]byte {
	var lists [][]byte
	var cur []byte
	for _, c := range cmds {
		add := len(c)
		if len(cur) > 0 {
			add++
		}
		if len(cur) > 0 && len(cur)+add > maxListBytes {
			lists = append(lists, cur)
			cur = nil
		}
		if len(cur) > 0 {
			cur = append(cur, 0x00) // delta time 0
		}
		cur = append(cur, c...)
	}
	if len(cur) > 0 {
		lists = append(lists, cur)
	}
	return lists
}
