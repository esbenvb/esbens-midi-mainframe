package blemidi

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// decodeAll feeds packets to a fresh Decoder and returns every emitted piece.
func decodeAll(d *Decoder, pkts ...[]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		d.Feed(p, func(b []byte) { out = append(out, b) })
	}
	return out
}

func joined(parts [][]byte) []byte {
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

func fixedClock(ms int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(ms) }
}

// ---- decoder: vectors derived from MMA/AMEI RP-052 --------------------------

func TestDecodeSingleNoteOn(t *testing.T) {
	// header 0x80 (ts high 0), timestamp 0x80 (ts low 0), Note On ch1 C4 vel 100
	got := decodeAll(new(Decoder), []byte{0x80, 0x80, 0x90, 0x3C, 0x64})
	want := [][]byte{{0x90, 0x3C, 0x64}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeIgnoresTimestampValues(t *testing.T) {
	got := decodeAll(new(Decoder), []byte{0xBF, 0xFF, 0xC5, 0x07})
	want := [][]byte{{0xC5, 0x07}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeSeveralMessagesInOnePacket(t *testing.T) {
	pkt := []byte{0x80,
		0x81, 0x90, 60, 100, // note on
		0x82, 0xB0, 7, 99, // cc
		0x83, 0xF8, // clock
		0x84, 0xC1, 5, // program
	}
	got := decodeAll(new(Decoder), pkt)
	want := [][]byte{{0x90, 60, 100}, {0xB0, 7, 99}, {0xF8}, {0xC1, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeRunningStatusWithoutTimestamp(t *testing.T) {
	// "If a Running Status MIDI message is not preceded by a timestamp byte,
	// the timestamp of the most recently preceding message is used."
	got := decodeAll(new(Decoder), []byte{0x80, 0x80, 0x90, 0x3C, 0x64, 0x3E, 0x50, 0x40, 0x7F})
	want := [][]byte{{0x90, 0x3C, 0x64}, {0x90, 0x3E, 0x50}, {0x90, 0x40, 0x7F}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeRunningStatusWithTimestamp(t *testing.T) {
	got := decodeAll(new(Decoder), []byte{0x80, 0x80, 0x90, 0x3C, 0x64, 0x81, 0x3E, 0x50, 0x82, 0x40, 0x7F})
	want := [][]byte{{0x90, 0x3C, 0x64}, {0x90, 0x3E, 0x50}, {0x90, 0x40, 0x7F}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeRunningStatusSurvivesCommonAndRealtime(t *testing.T) {
	// "System Common and System Real-Time messages do not cancel Running
	// Status if interspersed ... However, a timestamp byte must precede the
	// Running Status MIDI message that follows."
	pkt := []byte{0x80,
		0x80, 0x90, 0x3C, 0x64,
		0x81, 0xF8,
		0x82, 0xF3, 0x05,
		0x83, 0x3E, 0x50, // running status, with its own timestamp
	}
	got := decodeAll(new(Decoder), pkt)
	want := [][]byte{{0x90, 0x3C, 0x64}, {0xF8}, {0xF3, 0x05}, {0x90, 0x3E, 0x50}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeRunningStatusEndsWithPacket(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		[]byte{0x80, 0x80, 0x90, 0x3C, 0x64},
		[]byte{0x80, 0x81, 0x3E, 0x50}, // running status would continue, but the packet ended
	)
	want := [][]byte{{0x90, 0x3C, 0x64}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeSysExSinglePacket(t *testing.T) {
	got := decodeAll(new(Decoder), []byte{0x80, 0x80, 0xF0, 0x7E, 0x7F, 0x06, 0x01, 0x81, 0xF7})
	want := [][]byte{{0xF0, 0x7E, 0x7F, 0x06, 0x01, 0xF7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeSysExAcrossPackets(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		[]byte{0x80, 0x80, 0xF0, 0x01, 0x02}, // start
		[]byte{0x80, 0x03, 0x04},             // continuation: header + data, no timestamp
		[]byte{0x80, 0x05, 0x81, 0xF7},       // last: timestamp before EOX
	)
	if !bytes.Equal(joined(got), []byte{0xF0, 1, 2, 3, 4, 5, 0xF7}) {
		t.Fatalf("got %x", got)
	}
	// Pieces: start piece begins with F0, last ends with F7.
	if got[0][0] != 0xF0 || got[len(got)-1][len(got[len(got)-1])-1] != 0xF7 {
		t.Fatalf("pieces %x", got)
	}
}

func TestDecodeEndPacketOnlyTimestampAndEOX(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		[]byte{0x80, 0x80, 0xF0, 0x01},
		[]byte{0x80, 0x81, 0xF7}, // F7 alone in a new packet
	)
	if !bytes.Equal(joined(got), []byte{0xF0, 1, 0xF7}) {
		t.Fatalf("got %x", got)
	}
}

func TestDecodeRealtimeInsideSysEx(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		// F0 01 02 [ts F8] 03 [ts F7]: continuation bytes directly after a realtime byte
		[]byte{0x80, 0x80, 0xF0, 0x01, 0x02, 0x81, 0xF8, 0x03, 0x82, 0xF7},
	)
	want := [][]byte{{0xF0, 1, 2}, {0xF8}, {3, 0xF7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeRealtimeBetweenSysExPackets(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		[]byte{0x80, 0x80, 0xF0, 0x01},
		[]byte{0x80, 0x81, 0xF8}, // realtime-only packet in the middle of a SysEx
		[]byte{0x80, 0x02, 0x82, 0xF7},
	)
	want := [][]byte{{0xF0, 1}, {0xF8}, {2, 0xF7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestDecodeSysExAbortedByStatus(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d,
		[]byte{0x80, 0x80, 0xF0, 0x01, 0x02, 0x81, 0x90, 0x3C, 0x64},
	)
	want := [][]byte{{0xF0, 1, 2}, {0x90, 0x3C, 0x64}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	// A later "continuation" packet must not resurrect the SysEx.
	more := decodeAll(d, []byte{0x80, 0x05, 0x06})
	if len(more) != 0 {
		t.Fatalf("stray continuation produced %x", more)
	}
}

func TestDecodeMalformedInput(t *testing.T) {
	d := new(Decoder)
	for _, pkt := range [][]byte{
		nil,
		{},
		{0x00},                               // header without bit 7
		{0x40, 0x80, 0xF8},                   // bit 7 clear in header
		{0x80},                               // header only
		{0x80, 0x80},                         // header + timestamp only
		{0x80, 0x80, 0x90},                   // truncated message
		{0x80, 0x80, 0x90, 0x3C},             // truncated message
		{0x80, 0x01, 0x02, 0x03},             // data with no status
		{0x80, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, // timestamps and realtime
	} {
		d.Feed(pkt, func([]byte) {})
	}
	// The decoder still works afterwards.
	got := decodeAll(d, []byte{0x80, 0x80, 0x90, 0x3C, 0x64})
	if !reflect.DeepEqual(got, [][]byte{{0x90, 0x3C, 0x64}}) {
		t.Fatalf("decoder wedged: %x", got)
	}
}

func TestDecodeEmitsIndependentSlices(t *testing.T) {
	d := new(Decoder)
	got := decodeAll(d, []byte{0x80, 0x80, 0x90, 1, 2, 0x80, 0x90, 3, 4})
	if !reflect.DeepEqual(got, [][]byte{{0x90, 1, 2}, {0x90, 3, 4}}) {
		t.Fatalf("slices alias each other: %x", got)
	}
}

// ---- encoder ----------------------------------------------------------------

func TestEncodeNoteOnVector(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	got := e.Encode([]byte{0x90, 0x3C, 0x64}, 23)
	want := [][]byte{{0x80, 0x80, 0x90, 0x3C, 0x64}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestEncodeTimestampSplit(t *testing.T) {
	e := NewEncoder()
	// 1000 ms = 0b0001111_1101000: high 6 bits 7, low 7 bits 0x68.
	e.Clock = fixedClock(1000)
	got := e.Encode([]byte{0xF8}, 23)
	want := [][]byte{{0x87, 0xE8, 0xF8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	// Timestamps are 13 bits: 8192 wraps to 0.
	e.Clock = fixedClock(8192 + 5)
	got = e.Encode([]byte{0xF8}, 23)
	want = [][]byte{{0x80, 0x85, 0xF8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	e.Clock = fixedClock(8191)
	got = e.Encode([]byte{0xF8}, 23)
	want = [][]byte{{0xBF, 0xFF, 0xF8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestEncodeSeveralMessagesShareAPacket(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	got := e.Encode([]byte{0x90, 60, 100, 0xB0, 7, 9, 0xF8}, 185)
	want := [][]byte{{0x80, 0x80, 0x90, 60, 100, 0x80, 0xB0, 7, 9, 0x80, 0xF8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestEncodeSplitsPacketsAtMTU(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	// MTU 23 -> 20-byte packets: header + 4 five-byte... messages are 4 bytes
	// (ts + 3), so four fit (1+16=17), the fifth starts a new packet.
	var in []byte
	for i := 0; i < 5; i++ {
		in = append(in, 0x90, byte(60+i), 100)
	}
	got := e.Encode(in, 23)
	if len(got) != 2 {
		t.Fatalf("want 2 packets, got %d: %x", len(got), got)
	}
	for _, p := range got {
		if len(p) > 20 {
			t.Fatalf("packet %x exceeds 20 bytes", p)
		}
	}
	d := new(Decoder)
	if !bytes.Equal(joined(decodeAll(d, got...)), in) {
		t.Fatal("round trip mismatch")
	}
}

func TestEncodeSysExVectors(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	// MTU 23: 20-byte packets. 30 data bytes cannot fit in one.
	data := make([]byte, 30)
	for i := range data {
		data[i] = byte(i + 1)
	}
	msg := append(append([]byte{0xF0}, data...), 0xF7)
	pkts := e.Encode(msg, 23)
	if len(pkts) < 2 {
		t.Fatalf("expected a multi-packet SysEx, got %d packets", len(pkts))
	}
	// First packet: header, timestamp, F0, data...
	if p := pkts[0]; p[0] != 0x80 || p[1] != 0x80 || p[2] != 0xF0 {
		t.Fatalf("first packet %x", p)
	}
	// Continuation packets: header followed by a data byte, no timestamp.
	for _, p := range pkts[1:] {
		if p[0]&0xC0 != 0x80 {
			t.Fatalf("bad header in %x", p)
		}
	}
	for _, p := range pkts[1 : len(pkts)-1] {
		if p[1]&0x80 != 0 {
			t.Fatalf("continuation packet %x starts with a status/timestamp byte", p)
		}
	}
	// The last packet ends with timestamp + F7.
	last := pkts[len(pkts)-1]
	if last[len(last)-1] != 0xF7 || last[len(last)-2]&0x80 == 0 {
		t.Fatalf("last packet %x must end timestamp,F7", last)
	}
	for _, p := range pkts {
		if len(p) > 20 {
			t.Fatalf("packet %x too long", p)
		}
	}
	if got := joined(decodeAll(new(Decoder), pkts...)); !bytes.Equal(got, msg) {
		t.Fatalf("round trip: got %x want %x", got, msg)
	}
}

func TestEncodeSysExPieceByPieceKeepsState(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	d := new(Decoder)
	var all [][]byte
	for _, piece := range [][]byte{
		{0xF0, 1, 2, 3},
		{4, 5, 6},
		{0xF8}, // realtime between pieces, as the router does
		{7, 8, 0xF7},
	} {
		all = append(all, e.Encode(piece, 23)...)
	}
	want := []byte{0xF0, 1, 2, 3, 4, 5, 6, 0xF8, 7, 8, 0xF7}
	if got := joined(decodeAll(d, all...)); !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	// The continuation after the first piece is a header+data packet.
	if all[1][0]&0xC0 != 0x80 || all[1][1]&0x80 != 0 {
		t.Fatalf("second packet %x should be a continuation", all[1])
	}
}

func TestEncodeSysExEndNeedsTwoBytes(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	// 20-byte packet: header + ts + F0 + 17 data bytes = 20, so ts+F7 must go
	// into a fresh packet.
	data := bytes.Repeat([]byte{0x11}, 17)
	pkts := e.Encode(append(append([]byte{0xF0}, data...), 0xF7), 23)
	if len(pkts) != 2 || len(pkts[0]) != 20 {
		t.Fatalf("got %x", pkts)
	}
	if !bytes.Equal(pkts[1], []byte{0x80, 0x80, 0xF7}) {
		t.Fatalf("last packet %x", pkts[1])
	}
	if got := joined(decodeAll(new(Decoder), pkts...)); !bytes.Equal(got, append(append([]byte{0xF0}, data...), 0xF7)) {
		t.Fatalf("round trip %x", got)
	}
}

func TestEncodeExpandsRunningStatusInput(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	pkts := e.Encode([]byte{0x90, 60, 100, 62, 90}, 185)
	want := []byte{0x90, 60, 100, 0x90, 62, 90}
	if got := joined(decodeAll(new(Decoder), pkts...)); !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestEncodeAbortedSysEx(t *testing.T) {
	e := NewEncoder()
	e.Clock = fixedClock(0)
	pkts := e.Encode([]byte{0xF0, 1, 2}, 23)
	pkts = append(pkts, e.Encode([]byte{0x90, 60, 100}, 23)...)
	pkts = append(pkts, e.Encode([]byte{0x05}, 23)...) // stray, SysEx is over
	got := decodeAll(new(Decoder), pkts...)
	if !bytes.Equal(joined(got), []byte{0xF0, 1, 2, 0x90, 60, 100}) {
		t.Fatalf("got %x", got)
	}
}

func TestPacketLimit(t *testing.T) {
	for mtu, want := range map[int]int{0: 20, 22: 20, 23: 20, 100: 97, 185: 182, 517: 512, 600: 512} {
		if got := PacketLimit(mtu); got != want {
			t.Errorf("PacketLimit(%d) = %d, want %d", mtu, got, want)
		}
	}
}

// ---- property tests ---------------------------------------------------------

// genStream builds a random valid MIDI byte stream (explicit status bytes, no
// running status) plus the router-style chunking of it.
func genStream(r *rand.Rand, nItems int) (stream []byte, chunks [][]byte) {
	add := func(b ...byte) {
		stream = append(stream, b...)
		chunks = append(chunks, append([]byte(nil), b...))
	}
	rt := []byte{0xF8, 0xFA, 0xFB, 0xFC, 0xFE, 0xFF}
	for i := 0; i < nItems; i++ {
		switch r.Intn(8) {
		case 0, 1, 2: // two-data-byte channel messages
			st := []byte{0x80, 0x90, 0xA0, 0xB0, 0xE0}[r.Intn(5)] | byte(r.Intn(16))
			add(st, byte(r.Intn(128)), byte(r.Intn(128)))
		case 3: // one-data-byte channel messages
			st := []byte{0xC0, 0xD0}[r.Intn(2)] | byte(r.Intn(16))
			add(st, byte(r.Intn(128)))
		case 4:
			add(rt[r.Intn(len(rt))])
		case 5: // system common
			switch r.Intn(4) {
			case 0:
				add(0xF1, byte(r.Intn(128)))
			case 1:
				add(0xF2, byte(r.Intn(128)), byte(r.Intn(128)))
			case 2:
				add(0xF3, byte(r.Intn(128)))
			case 3:
				add(0xF6)
			}
		default: // SysEx delivered in random pieces with realtime in between
			n := r.Intn(400)
			body := make([]byte, n)
			for j := range body {
				body[j] = byte(r.Intn(128))
			}
			full := append(append([]byte{0xF0}, body...), 0xF7)
			for len(full) > 0 {
				k := 1 + r.Intn(300)
				if k > len(full) {
					k = len(full)
				}
				add(full[:k]...)
				full = full[k:]
				if len(full) > 0 && r.Intn(4) == 0 {
					add(rt[r.Intn(len(rt))])
				}
			}
		}
	}
	return stream, chunks
}

func TestRoundTripRandomStreams(t *testing.T) {
	for _, mtu := range []int{23, 100, 185, 517} {
		for seed := int64(0); seed < 40; seed++ {
			r := rand.New(rand.NewSource(seed*7919 + int64(mtu)))
			stream, chunks := genStream(r, 60)
			e := NewEncoder()
			e.Clock = fixedClock(seed * 37)
			d := new(Decoder)
			var out []byte
			for _, c := range chunks {
				for _, p := range e.Encode(c, mtu) {
					if len(p) > PacketLimit(mtu) {
						t.Fatalf("mtu %d: packet of %d bytes exceeds limit %d", mtu, len(p), PacketLimit(mtu))
					}
					if p[0]&0xC0 != 0x80 {
						t.Fatalf("bad header byte %02x", p[0])
					}
					d.Feed(p, func(b []byte) { out = append(out, b...) })
				}
			}
			if !bytes.Equal(out, stream) {
				t.Fatalf("mtu %d seed %d: round trip mismatch (%d vs %d bytes)", mtu, seed, len(out), len(stream))
			}
		}
	}
}

func TestRoundTripWholeStreamInOneCall(t *testing.T) {
	// A single Encode call with many messages and a complete SysEx inside.
	r := rand.New(rand.NewSource(99))
	stream, _ := genStream(r, 80)
	for _, mtu := range []int{23, 185, 517} {
		e := NewEncoder()
		e.Clock = fixedClock(1234)
		d := new(Decoder)
		var out []byte
		for _, p := range e.Encode(stream, mtu) {
			if len(p) > PacketLimit(mtu) {
				t.Fatalf("packet too long: %d", len(p))
			}
			d.Feed(p, func(b []byte) { out = append(out, b...) })
		}
		if !bytes.Equal(out, stream) {
			t.Fatalf("mtu %d: mismatch", mtu)
		}
	}
}

func TestRoundTripLargeSysEx(t *testing.T) {
	const size = 200 * 1024
	r := rand.New(rand.NewSource(1))
	msg := make([]byte, 0, size+2)
	msg = append(msg, 0xF0)
	for i := 0; i < size; i++ {
		msg = append(msg, byte(r.Intn(128)))
	}
	msg = append(msg, 0xF7)

	for _, mtu := range []int{23, 100, 185, 517} {
		e := NewEncoder()
		e.Clock = fixedClock(500)
		d := new(Decoder)
		var out []byte
		packets := 0
		// Feed in 256-byte router chunks, like the router does.
		for off := 0; off < len(msg); off += 256 {
			end := off + 256
			if end > len(msg) {
				end = len(msg)
			}
			for _, p := range e.Encode(msg[off:end], mtu) {
				packets++
				if len(p) > PacketLimit(mtu) {
					t.Fatalf("mtu %d: packet of %d bytes", mtu, len(p))
				}
				d.Feed(p, func(b []byte) { out = append(out, b...) })
			}
		}
		if !bytes.Equal(out, msg) {
			t.Fatalf("mtu %d: 200 KB SysEx corrupted (%d bytes back, want %d)", mtu, len(out), len(msg))
		}
		t.Logf("mtu %3d: %d packets for %d bytes (%.1f%% payload)", mtu, packets, len(msg),
			100*float64(len(msg))/float64(len(msg)+packets*2))
	}
}

func TestDecoderRandomBytesNeverPanic(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	d := new(Decoder)
	for i := 0; i < 20000; i++ {
		pkt := make([]byte, r.Intn(64))
		for j := range pkt {
			pkt[j] = byte(r.Intn(256))
		}
		if len(pkt) > 0 && r.Intn(3) != 0 {
			pkt[0] = 0x80 | byte(r.Intn(64)) // mostly valid headers
		}
		d.Feed(pkt, func(b []byte) {
			if len(b) == 0 {
				t.Fatal("empty emit")
			}
		})
	}
}

func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0x80, 0x80, 0x90, 0x3C, 0x64})
	f.Add([]byte{0x80, 0x80, 0xF0, 0x01, 0x02, 0x81, 0xF8, 0x03, 0x82, 0xF7})
	f.Add([]byte{0x80, 0x03, 0x04})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		d := new(Decoder)
		var out []byte
		d.Feed(pkt, func(b []byte) {
			if len(b) == 0 {
				t.Fatal("empty emit")
			}
			out = append(out, b...)
		})
		// Everything emitted must be well-formed enough for the router's
		// parser: at most as many bytes as went in.
		if len(out) > len(pkt)*3 {
			t.Fatalf("emitted %d bytes from %d", len(out), len(pkt))
		}
		d.Feed(pkt, func([]byte) {}) // state carried over must not break anything
	})
}

func FuzzEncodeDecode(f *testing.F) {
	f.Add([]byte{0x90, 0x3C, 0x64}, 23)
	f.Add([]byte{0xF0, 1, 2, 3, 0xF7}, 23)
	f.Fuzz(func(t *testing.T, data []byte, mtu int) {
		if mtu < 0 || mtu > 1000 {
			return
		}
		e := NewEncoder()
		for _, p := range e.Encode(data, mtu) {
			if len(p) > PacketLimit(mtu) || p[0]&0xC0 != 0x80 {
				t.Fatalf("bad packet %x for mtu %d", p, mtu)
			}
			new(Decoder).Feed(p, func([]byte) {})
		}
	})
}
