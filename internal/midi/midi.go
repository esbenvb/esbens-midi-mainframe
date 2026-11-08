// Package midi holds the MIDI 1.0 byte-stream parser and the small amount of
// message knowledge the router needs (classification, channel handling,
// human-readable descriptions). It has no dependencies.
package midi

import "fmt"

// Class is the structural class of an Event.
type Class uint8

const (
	ClassChannel  Class = iota + 1 // 0x80..0xEF
	ClassCommon                    // 0xF1..0xF6 (system common)
	ClassRealtime                  // 0xF8..0xFF
	ClassSysEx                     // F0 ... F7, possibly delivered in chunks
)

// Category is the user-facing grouping used by filters and the monitor.
type Category uint8

const (
	CatNote        Category = iota // note on / note off
	CatPolyAT                      // polyphonic aftertouch
	CatCC                          // control change (incl. channel mode)
	CatProgram                     // program change
	CatChanAT                      // channel aftertouch
	CatBend                        // pitch bend
	CatSysEx                       // system exclusive
	CatClock                       // timing clock (F8)
	CatTransport                   // start / continue / stop
	CatActiveSense                 // active sensing (FE)
	CatReset                       // system reset (FF)
	CatCommon                      // MTC quarter frame, song position/select, tune request
	CatOther                       // undefined status bytes
	NumCategories
)

var categoryNames = [NumCategories]string{
	"note", "poly_at", "cc", "program", "chan_at", "bend", "sysex",
	"clock", "transport", "active_sense", "reset", "sys_common", "other",
}

func (c Category) String() string {
	if c < NumCategories {
		return categoryNames[c]
	}
	return "unknown"
}

// ParseCategory is the inverse of Category.String.
func ParseCategory(s string) (Category, bool) {
	for i, n := range categoryNames {
		if n == s {
			return Category(i), true
		}
	}
	return 0, false
}

// CategoryNames lists every filterable category name, in display order.
func CategoryNames() []string {
	out := make([]string, len(categoryNames))
	copy(out, categoryNames[:])
	return out
}

// Event is one unit flowing through the router: a complete channel / system
// common / realtime message (1..3 bytes, always with an explicit status byte),
// or one chunk of a system exclusive message.
//
// SysEx messages are streamed so that arbitrarily long dumps never have to be
// buffered whole: the first chunk has First set (Data starts with F0), the last
// has Last set (Data normally ends with F7; if the sender aborted the message
// with another status byte the final chunk simply has no F7).
type Event struct {
	Data        []byte
	SysEx       bool
	First, Last bool // only meaningful when SysEx
}

// Class returns the structural class of the event.
func (e Event) Class() Class {
	if e.SysEx {
		return ClassSysEx
	}
	if len(e.Data) == 0 {
		return 0
	}
	s := e.Data[0]
	switch {
	case s >= 0xF8:
		return ClassRealtime
	case s >= 0xF0:
		return ClassCommon
	default:
		return ClassChannel
	}
}

// Channel returns the 0-based channel for channel messages.
func (e Event) Channel() (int, bool) {
	if e.SysEx || len(e.Data) == 0 || e.Data[0] >= 0xF0 {
		return 0, false
	}
	return int(e.Data[0] & 0x0F), true
}

// Category returns the filter category of the event.
func (e Event) Category() Category {
	if e.SysEx {
		return CatSysEx
	}
	if len(e.Data) == 0 {
		return CatOther
	}
	s := e.Data[0]
	switch s & 0xF0 {
	case 0x80, 0x90:
		return CatNote
	case 0xA0:
		return CatPolyAT
	case 0xB0:
		return CatCC
	case 0xC0:
		return CatProgram
	case 0xD0:
		return CatChanAT
	case 0xE0:
		return CatBend
	}
	switch s {
	case 0xF1, 0xF2, 0xF3, 0xF6:
		return CatCommon
	case 0xF8:
		return CatClock
	case 0xFA, 0xFB, 0xFC:
		return CatTransport
	case 0xFE:
		return CatActiveSense
	case 0xFF:
		return CatReset
	}
	return CatOther
}

// dataLen returns how many data bytes follow a status byte.
func dataLen(status byte) int {
	switch {
	case status < 0x80:
		return 0
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
	return 0 // F4, F5, F6 and realtime
}

// DefaultSysExChunk is the default maximum size of a SysEx chunk.
const DefaultSysExChunk = 256

// Parser turns a raw MIDI byte stream into Events. It handles running status,
// realtime bytes interleaved inside other messages, and chunked SysEx. A Parser
// must only be used from one goroutine.
type Parser struct {
	// MaxChunk caps SysEx chunk size (default DefaultSysExChunk).
	MaxChunk int

	running byte // running status, 0 when none
	need    int  // data bytes still expected for the message in progress
	msg     [3]byte
	n       int // bytes of msg filled (including status)

	inSysex bool
	first   bool // next sysex chunk is the first of its message
	sx      []byte
}

// NewParser returns a Parser with default settings.
func NewParser() *Parser { return &Parser{MaxChunk: DefaultSysExChunk} }

// Reset drops all partial state (e.g. after a device reconnect).
func (p *Parser) Reset() {
	p.running, p.need, p.n = 0, 0, 0
	p.inSysex, p.first = false, false
	p.sx = p.sx[:0]
}

func (p *Parser) max() int {
	if p.MaxChunk > 0 {
		return p.MaxChunk
	}
	return DefaultSysExChunk
}

func (p *Parser) flushSysex(emit func(Event), last bool) {
	if len(p.sx) == 0 && !last {
		return
	}
	emit(Event{Data: append([]byte(nil), p.sx...), SysEx: true, First: p.first, Last: last})
	p.sx = p.sx[:0]
	p.first = false
}

// Feed parses b and calls emit for every complete Event. A SysEx in progress
// at the end of b is flushed as a non-final chunk so that data is never held
// back waiting for more input.
func (p *Parser) Feed(b []byte, emit func(Event)) {
	for _, c := range b {
		switch {
		case c >= 0xF8: // realtime: legal anywhere, never disturbs running status
			if p.inSysex {
				p.flushSysex(emit, false)
			}
			emit(Event{Data: []byte{c}})

		case c == 0xF0:
			if p.inSysex { // previous one never got its F7
				p.flushSysex(emit, true)
			}
			p.running, p.need, p.n = 0, 0, 0
			p.inSysex, p.first = true, true
			p.sx = append(p.sx[:0], 0xF0)

		case c == 0xF7:
			if p.inSysex {
				p.sx = append(p.sx, 0xF7)
				p.flushSysex(emit, true)
				p.inSysex = false
			} // stray F7: ignore
			p.running, p.need, p.n = 0, 0, 0

		case c >= 0x80: // channel or system common status
			if p.inSysex {
				p.flushSysex(emit, true)
				p.inSysex = false
			}
			if c < 0xF0 {
				p.running = c
			} else {
				p.running = 0
			}
			p.msg[0], p.n, p.need = c, 1, dataLen(c)
			if p.need == 0 {
				emit(Event{Data: []byte{c}})
				p.n = 0
			}

		default: // data byte
			if p.inSysex {
				p.sx = append(p.sx, c)
				if len(p.sx) >= p.max() {
					p.flushSysex(emit, false)
				}
				continue
			}
			if p.need == 0 && p.n == 0 {
				if p.running == 0 {
					continue // stray data byte
				}
				p.msg[0], p.n, p.need = p.running, 1, dataLen(p.running)
			}
			p.msg[p.n] = c
			p.n++
			p.need--
			if p.need == 0 {
				emit(Event{Data: append([]byte(nil), p.msg[:p.n]...)})
				p.n = 0
			}
		}
	}
	if p.inSysex && len(p.sx) > 0 {
		p.flushSysex(emit, false)
	}
}

var noteNames = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

// NoteName returns e.g. "C4" for MIDI note 60 (middle C = C4).
func NoteName(n byte) string {
	return fmt.Sprintf("%s%d", noteNames[n%12], int(n)/12-1)
}

// Describe returns a short human-readable description, for the monitor.
func Describe(e Event) string {
	if e.SysEx {
		switch {
		case e.First && e.Last:
			return fmt.Sprintf("SysEx %d bytes", len(e.Data))
		case e.First:
			return fmt.Sprintf("SysEx start (%d bytes so far)", len(e.Data))
		case e.Last:
			return fmt.Sprintf("SysEx end (+%d bytes)", len(e.Data))
		default:
			return fmt.Sprintf("SysEx data (+%d bytes)", len(e.Data))
		}
	}
	d := e.Data
	if len(d) == 0 {
		return "empty"
	}
	s := d[0]
	if s < 0xF0 {
		ch := int(s&0x0F) + 1
		switch s & 0xF0 {
		case 0x80:
			return fmt.Sprintf("Note Off ch%d %s vel %d", ch, NoteName(d[1]), d[2])
		case 0x90:
			if d[2] == 0 {
				return fmt.Sprintf("Note Off ch%d %s", ch, NoteName(d[1]))
			}
			return fmt.Sprintf("Note On ch%d %s vel %d", ch, NoteName(d[1]), d[2])
		case 0xA0:
			return fmt.Sprintf("Poly AT ch%d %s %d", ch, NoteName(d[1]), d[2])
		case 0xB0:
			return fmt.Sprintf("CC ch%d #%d = %d", ch, d[1], d[2])
		case 0xC0:
			return fmt.Sprintf("Program ch%d %d", ch, d[1])
		case 0xD0:
			return fmt.Sprintf("Chan AT ch%d %d", ch, d[1])
		case 0xE0:
			return fmt.Sprintf("Pitch Bend ch%d %d", ch, (int(d[1])|int(d[2])<<7)-8192)
		}
	}
	switch s {
	case 0xF1:
		return "MTC Quarter Frame"
	case 0xF2:
		return fmt.Sprintf("Song Position %d", int(d[1])|int(d[2])<<7)
	case 0xF3:
		return fmt.Sprintf("Song Select %d", d[1])
	case 0xF6:
		return "Tune Request"
	case 0xF8:
		return "Clock"
	case 0xFA:
		return "Start"
	case 0xFB:
		return "Continue"
	case 0xFC:
		return "Stop"
	case 0xFE:
		return "Active Sensing"
	case 0xFF:
		return "System Reset"
	}
	return fmt.Sprintf("0x%02X", s)
}
