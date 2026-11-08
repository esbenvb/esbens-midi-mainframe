package rtpmidi

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"emm/internal/midi"
)

func TestVLQ(t *testing.T) {
	cases := []struct {
		v    uint32
		want string
	}{
		{0, "00"}, {1, "01"}, {127, "7F"}, {128, "8100"}, {129, "8101"}, {0x3FFF, "FF7F"},
		{0x4000, "818000"}, {0x1FFFFF, "FFFF7F"}, {0x200000, "81808000"}, {0x0FFFFFFF, "FFFFFF7F"},
	}
	for _, c := range cases {
		got := appendVLQ(nil, c.v)
		if !bytes.Equal(got, h(t, c.want)) {
			t.Errorf("appendVLQ(%d) = %x want %s", c.v, got, c.want)
		}
		v, n, ok := readVLQ(append(got, 0x55))
		if !ok || v != c.v || n != len(got) {
			t.Errorf("readVLQ(%x) = %d,%d,%v", got, v, n, ok)
		}
	}
	if _, _, ok := readVLQ(h(t, "FFFFFFFF")); ok {
		t.Error("5-byte VLQ accepted")
	}
	if _, _, ok := readVLQ(h(t, "81")); ok {
		t.Error("truncated VLQ accepted")
	}
	if got := appendVLQ(nil, 0xFFFFFFFF); !bytes.Equal(got, h(t, "FFFFFF7F")) {
		t.Errorf("clamp: %x", got)
	}
}

func TestSectionHeaderForms(t *testing.T) {
	// Short form: LEN in the low nibble of the first octet.
	if got := buildSection(h(t, "903C64"), nil); !bytes.Equal(got, h(t, "03 903C64")) {
		t.Errorf("short form: %x", got)
	}
	if got := buildSection(nil, nil); !bytes.Equal(got, h(t, "00")) {
		t.Errorf("empty: %x", got)
	}
	fifteen := bytes.Repeat([]byte{0xF8}, 15)
	if got := buildSection(fifteen, nil); got[0] != 0x0F || len(got) != 16 {
		t.Errorf("15-byte list must still use the short form: %x", got[:2])
	}
	// Long form (B=1), 12-bit length.
	sixteen := bytes.Repeat([]byte{0xF8}, 16)
	if got := buildSection(sixteen, nil); got[0] != 0x80 || got[1] != 0x10 || len(got) != 18 {
		t.Errorf("16-byte list: %x", got[:3])
	}
	three := bytes.Repeat([]byte{0xF8}, 300) // 0x12C
	if got := buildSection(three, nil); got[0] != 0x81 || got[1] != 0x2C {
		t.Errorf("300-byte list: %x", got[:3])
	}
	// J flag when a journal is attached; the journal follows the list.
	got := buildSection(h(t, "903C64"), h(t, "AABB"))
	if !bytes.Equal(got, h(t, "43 903C64 AABB")) {
		t.Errorf("with journal: %x", got)
	}

	for _, s := range []string{"03 903C64", "80 10" + string(bytes.Repeat([]byte("F8"), 16)), "81 2C" + string(bytes.Repeat([]byte("F8"), 300))} {
		b := h(t, s)
		sec, err := parseSection(b)
		if err != nil || len(sec.list) != len(b)-map[bool]int{true: 2, false: 1}[b[0]&0x80 != 0] || sec.journal {
			t.Errorf("parseSection(%s…): %+v %v", s[:6], sec, err)
		}
	}
	sec, err := parseSection(h(t, "63 903C64 DEADBEEF"))
	if err != nil || !sec.z || !sec.journal || sec.p || !bytes.Equal(sec.list, h(t, "903C64")) {
		t.Errorf("flags: %+v %v", sec, err)
	}
	if sec, _ := parseSection(h(t, "13 3E64 00")); !sec.p {
		t.Error("P flag")
	}
	for _, bad := range []string{"", "05 903C", "80", "80 10 F8"} {
		if _, err := parseSection(h(t, bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func decodeAll(t *testing.T, d *listDecoder, list string, z bool) []byte {
	t.Helper()
	out, err := d.decode(h(t, list), z, nil)
	if err != nil {
		t.Fatalf("decode %s: %v", list, err)
	}
	return out
}

func TestDecodeBasics(t *testing.T) {
	var d listDecoder
	// note on, then (delta 0) running-status note on, then (delta 0) note off
	if got := decodeAll(t, &d, "903C64 00 3E64 00 803C40", false); !bytes.Equal(got, h(t, "903C64 903E64 803C40")) {
		t.Errorf("running status: %x", got)
	}
	// Z=1: the first command is preceded by a delta time.
	d = listDecoder{}
	if got := decodeAll(t, &d, "05 903C64", true); !bytes.Equal(got, h(t, "903C64")) {
		t.Errorf("Z flag: %x", got)
	}
	// Multi-byte delta times.
	d = listDecoder{}
	if got := decodeAll(t, &d, "903C64 8100 803C00 FFFF7F C000", false); !bytes.Equal(got, h(t, "903C64 803C00 C000")) {
		t.Errorf("multi-byte delta: %x", got)
	}
	// Program change (1 data byte) with running status.
	d = listDecoder{}
	if got := decodeAll(t, &d, "C105 00 06 00 07", false); !bytes.Equal(got, h(t, "C105 C106 C107")) {
		t.Errorf("1-byte running status: %x", got)
	}
	// Realtime in between does not disturb running status.
	d = listDecoder{}
	if got := decodeAll(t, &d, "903C64 00 F8 00 3E64", false); !bytes.Equal(got, h(t, "903C64 F8 903E64")) {
		t.Errorf("realtime: %x", got)
	}
	// System common clears running status; a following status works.
	d = listDecoder{}
	if got := decodeAll(t, &d, "F20102 00 F6 00 F301 00 903C64", false); !bytes.Equal(got, h(t, "F20102 F6 F301 903C64")) {
		t.Errorf("system common: %x", got)
	}
	if d.running != 0x90 {
		t.Errorf("running status %02x", d.running)
	}
}

func TestDecodeRunningStatusAcrossPackets(t *testing.T) {
	var d listDecoder
	decodeAll(t, &d, "903C64", false)
	// Next packet has P=1: its first command has no status byte.
	if got := decodeAll(t, &d, "3E64", false); !bytes.Equal(got, h(t, "903E64")) {
		t.Errorf("phantom status: %x", got)
	}
	d.reset() // sequence gap: running status is no longer trustworthy
	if _, err := d.decode(h(t, "3E64"), false, nil); err == nil {
		t.Error("data byte accepted without running status after reset")
	}
}

func TestDecodeMalformed(t *testing.T) {
	bad := []string{
		"3C64",               // data without running status
		"90 3C",              // truncated
		"903C64 00",          // delta without command
		"903C64 81",          // truncated delta
		"903C64 FFFFFFFF 90", // over-long delta
		"F0 01 02",           // unterminated SysEx
		"F0 01 90 02 F7",     // status byte inside SysEx
		"90 3C 90",           // status inside data
	}
	for _, s := range bad {
		var d listDecoder
		if _, err := d.decode(h(t, s), false, nil); err == nil {
			t.Errorf("decode(%q) should fail", s)
		}
	}
	// What was decoded before the error is still delivered.
	var d listDecoder
	out, err := d.decode(h(t, "903C64 00 80"), false, nil)
	if err == nil || !bytes.Equal(out, h(t, "903C64")) {
		t.Errorf("partial: %x %v", out, err)
	}
}

func TestDecodeSysEx(t *testing.T) {
	var d listDecoder
	// Complete in one segment.
	if got := decodeAll(t, &d, "F0 7E 7F F7", false); !bytes.Equal(got, h(t, "F07E7FF7")) {
		t.Errorf("complete: %x", got)
	}
	// Three segments across packets, with other traffic in between.
	if got := decodeAll(t, &d, "F0 01 02 F0", false); len(got) != 0 {
		t.Errorf("first segment must not emit: %x", got)
	}
	if got := decodeAll(t, &d, "F8 00 903C64", false); !bytes.Equal(got, h(t, "F8 903C64")) {
		t.Errorf("interleaved traffic: %x", got)
	}
	if got := decodeAll(t, &d, "F7 03 04 F0", false); len(got) != 0 {
		t.Errorf("middle segment must not emit: %x", got)
	}
	if got := decodeAll(t, &d, "F7 05 F7", false); !bytes.Equal(got, h(t, "F0 01 02 03 04 05 F7")) {
		t.Errorf("reassembled: %x", got)
	}
	// Cancel.
	decodeAll(t, &d, "F0 01 F0", false)
	if got := decodeAll(t, &d, "F7 F4", false); len(got) != 0 {
		t.Errorf("cancel emitted: %x", got)
	}
	if d.inSx {
		t.Error("still in SysEx after cancel")
	}
	// A last segment without a start is dropped, as is a middle one.
	before := d.dropped
	if got := decodeAll(t, &d, "F7 01 F7", false); len(got) != 0 {
		t.Errorf("orphan last emitted: %x", got)
	}
	if got := decodeAll(t, &d, "F7 01 F0", false); len(got) != 0 {
		t.Errorf("orphan middle emitted: %x", got)
	}
	if d.dropped != before+2 {
		t.Errorf("dropped=%d want %d", d.dropped, before+2)
	}
	// A new first segment replaces an unfinished message.
	decodeAll(t, &d, "F0 01 F0", false)
	if got := decodeAll(t, &d, "F0 09 F7", false); !bytes.Equal(got, h(t, "F009F7")) {
		t.Errorf("restart: %x", got)
	}
	if d.inSx {
		t.Error("stale partial kept")
	}
	// Running status is cancelled by SysEx.
	decodeAll(t, &d, "903C64", false)
	decodeAll(t, &d, "00 F0 01 F7", true)
	if _, err := d.decode(h(t, "3E64"), false, nil); err == nil {
		t.Error("running status survived SysEx")
	}
}

func TestEncoderCommands(t *testing.T) {
	e := newEncoder()
	cmds := e.encode(h(t, "903C64 F8 3E64 C005"))
	want := [][]byte{h(t, "903C64"), h(t, "F8"), h(t, "903E64"), h(t, "C005")}
	if !equalCmds(cmds, want) {
		t.Fatalf("got %x want %x", cmds, want)
	}
	// Complete SysEx in one call.
	if got := e.encode(h(t, "F0 7E 7F F7")); !equalCmds(got, [][]byte{h(t, "F07E7FF7")}) {
		t.Fatalf("complete sysex: %x", got)
	}
	// Chunked SysEx across calls.
	var got [][]byte
	got = append(got, e.encode(h(t, "F0 01 02"))...)
	got = append(got, e.encode(h(t, "03 04"))...)
	got = append(got, e.encode(h(t, "05 F7"))...)
	want = [][]byte{h(t, "F0 01 02 F0"), h(t, "F7 03 04 F0"), h(t, "F7 05 F7")}
	if !equalCmds(got, want) {
		t.Fatalf("chunked sysex: got %x want %x", got, want)
	}
	// Chunk boundary right after F0, and a final chunk that is just F7. The
	// first segment may not be empty (RFC 6295 §3.2), so the F0 waits for the
	// next piece instead of going out alone as "F0 F0".
	got = nil
	got = append(got, e.encode(h(t, "F0"))...)
	got = append(got, e.encode(h(t, "01"))...)
	got = append(got, e.encode(h(t, "F7"))...)
	want = [][]byte{h(t, "F0 01 F0"), h(t, "F7 F7")}
	if !equalCmds(got, want) {
		t.Fatalf("boundary sysex: got %x want %x", got, want)
	}
	// Realtime inside a SysEx dump.
	got = nil
	got = append(got, e.encode(h(t, "F0 01 F8 02 F7"))...)
	want = [][]byte{h(t, "F0 01 F0"), h(t, "F8"), h(t, "F7 02 F7")}
	if !equalCmds(got, want) {
		t.Fatalf("realtime in sysex: got %x want %x", got, want)
	}
}

func TestEncoderCancelsAbortedSysEx(t *testing.T) {
	e := newEncoder()
	got := e.encode(h(t, "F0 01 02"))
	got = append(got, e.encode(h(t, "903C64"))...) // status byte aborts the dump
	want := [][]byte{h(t, "F0 01 02 F0"), h(t, "F7 F4"), h(t, "903C64")}
	if !equalCmds(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	// Aborted within a single call: nothing was sent, so nothing to cancel.
	if got := e.encode(h(t, "F0 01 02 903C64")); !equalCmds(got, [][]byte{h(t, "903C64")}) {
		t.Fatalf("single-call abort: %x", got)
	}
	// A continuation of a message whose start was never sent is ignored.
	e2 := newEncoder()
	if got := e2.sysex(midi.Event{Data: h(t, "01 02 F7"), SysEx: true, Last: true}, nil); len(got) != 0 {
		t.Fatalf("orphan continuation produced %x", got)
	}
}

func equalCmds(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestPackLists(t *testing.T) {
	lists := packLists([][]byte{h(t, "903C64"), h(t, "F8"), h(t, "803C00")})
	if len(lists) != 1 || !bytes.Equal(lists[0], h(t, "903C64 00 F8 00 803C00")) {
		t.Fatalf("one list: %x", lists)
	}
	if packLists(nil) != nil {
		t.Error("nil in, nil out")
	}
	// Splits at the size limit.
	var cmds [][]byte
	for i := 0; i < 1000; i++ {
		cmds = append(cmds, h(t, "903C64"))
	}
	lists = packLists(cmds)
	if len(lists) < 2 {
		t.Fatal("expected several lists")
	}
	var total int
	for _, l := range lists {
		if len(l) > maxListBytes {
			t.Fatalf("list of %d bytes exceeds the limit", len(l))
		}
		total += len(l)
	}
	// Reassemble through the decoder: every command must survive.
	var d listDecoder
	var out []byte
	for _, l := range lists {
		var err error
		if out, err = d.decode(l, false, out); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(out, bytes.Repeat(h(t, "903C64"), 1000)) {
		t.Fatal("round trip mismatch")
	}
}

// wire pushes raw MIDI through encoder -> RTP packets -> parse -> decoder.
func wire(t *testing.T, chunks [][]byte, drop func(i int) bool) (out []byte, pkts [][]byte, rcv *link) {
	t.Helper()
	enc := newEncoder()
	snd := newLink("snd", 1, 1, nil, nil, false)
	rcv = newLink("rcv", 2, 1, nil, nil, false)
	i := 0
	for _, c := range chunks {
		lists := packLists(enc.encode(c))
		for _, p := range snd.packetsFor(lists, 1, 0) {
			pkts = append(pkts, p)
			if drop != nil && drop(i) {
				i++
				continue
			}
			i++
			hd, payload, err := parseRTP(p)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, rcv.receive(hd, payload, quietLogger())...)
		}
	}
	return out, pkts, rcv
}

func randomSysEx(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	msg := make([]byte, 0, n+2)
	msg = append(msg, 0xF0)
	for i := 0; i < n; i++ {
		msg = append(msg, byte(r.Intn(128)))
	}
	return append(msg, 0xF7)
}

func split(b []byte, size int) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n := size
		if n > len(b) {
			n = len(b)
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

func TestHugeSysExRoundTrip(t *testing.T) {
	msg := randomSysEx(100*1024, 1)
	// The router delivers 256-byte chunks.
	out, pkts, rcv := wire(t, split(msg, 256), nil)
	if !bytes.Equal(out, msg) {
		t.Fatalf("100 KB SysEx corrupted: got %d bytes want %d", len(out), len(msg))
	}
	for i, p := range pkts {
		if len(p) > 1400 {
			t.Fatalf("packet %d is %d bytes", i, len(p))
		}
	}
	if rcv.rx.dec.dropped != 0 || rcv.rx.lost != 0 {
		t.Fatalf("dropped=%d lost=%d", rcv.rx.dec.dropped, rcv.rx.lost)
	}

	// The same as one single giant Send: the encoder must split it across packets.
	out, pkts, _ = wire(t, [][]byte{msg}, nil)
	if !bytes.Equal(out, msg) {
		t.Fatalf("single-Send SysEx corrupted: got %d bytes", len(out))
	}
	if len(pkts) < 70 {
		t.Fatalf("expected ~80 packets, got %d", len(pkts))
	}
	for i, p := range pkts {
		if len(p) > 1400 {
			t.Fatalf("packet %d is %d bytes", i, len(p))
		}
	}
	// Odd chunk sizes, mixed with notes between dumps.
	for _, size := range []int{1, 3, 7, 255, 1299, 1300, 1301, 5000} {
		small := randomSysEx(20000, int64(size))
		chunks := append([][]byte{h(t, "903C64")}, split(small, size)...)
		chunks = append(chunks, h(t, "803C00"))
		out, _, _ = wire(t, chunks, nil)
		want := append(append(h(t, "903C64"), small...), h(t, "803C00")...)
		if !bytes.Equal(out, want) {
			t.Fatalf("chunk size %d: corrupted (got %d bytes want %d)", size, len(out), len(want))
		}
	}
}

func TestSysExWithRealtimeAndNotesBetweenChunks(t *testing.T) {
	msg := randomSysEx(5000, 7)
	chunks := [][]byte{msg[:1000], h(t, "F8"), msg[1000:3000], h(t, "F8 F8"), msg[3000:]}
	out, _, _ := wire(t, chunks, nil)
	// Reassembly emits the realtime bytes first and the SysEx when complete.
	want := append(h(t, "F8 F8 F8"), msg...)
	if !bytes.Equal(out, want) {
		t.Fatalf("got %d bytes want %d", len(out), len(want))
	}
}

func TestPacketLossNeverDeliversCorruptSysEx(t *testing.T) {
	msg := randomSysEx(20000, 3)
	chunks := append(split(msg, 256), h(t, "903C64"))
	out, pkts, rcv := wire(t, chunks, func(i int) bool { return i == 4 }) // lose one packet in the middle
	if len(pkts) < 10 {
		t.Fatal("test needs several packets")
	}
	if bytes.Contains(out, msg[:100]) {
		t.Fatal("a truncated SysEx was forwarded")
	}
	if !bytes.HasSuffix(out, h(t, "903C64")) {
		t.Fatalf("traffic after the loss must still flow: %x", out[len(out)-6:])
	}
	if rcv.rx.lost != 1 || rcv.rx.dec.dropped == 0 {
		t.Fatalf("lost=%d dropped=%d", rcv.rx.lost, rcv.rx.dec.dropped)
	}
}

func TestDuplicateAndLatePacketsIgnored(t *testing.T) {
	enc := newEncoder()
	snd := newLink("s", 1, 1, nil, nil, false)
	rcv := newLink("r", 2, 1, nil, nil, false)
	var pk [][]byte
	for _, m := range []string{"903C64", "803C00", "903E64"} {
		pk = append(pk, snd.packetsFor(packLists(enc.encode(h(t, m))), 1, 0)...)
	}
	feed := func(p []byte) []byte {
		hd, pl, _ := parseRTP(p)
		return rcv.receive(hd, pl, quietLogger())
	}
	var out []byte
	out = append(out, feed(pk[0])...)
	out = append(out, feed(pk[1])...)
	out = append(out, feed(pk[1])...) // duplicate
	out = append(out, feed(pk[0])...) // late
	out = append(out, feed(pk[2])...)
	if !bytes.Equal(out, h(t, "903C64 803C00 903E64")) {
		t.Fatalf("got %x", out)
	}
}

func TestSequenceWraparound(t *testing.T) {
	snd := newLink("s", 1, 1, nil, nil, false)
	snd.txSeq = 65534
	rcv := newLink("r", 2, 1, nil, nil, false)
	enc := newEncoder()
	var out []byte
	for i := 0; i < 5; i++ {
		for _, p := range snd.packetsFor(packLists(enc.encode(h(t, "C005"))), 1, 0) {
			hd, pl, _ := parseRTP(p)
			out = append(out, rcv.receive(hd, pl, quietLogger())...)
		}
	}
	if !bytes.Equal(out, bytes.Repeat(h(t, "C005"), 5)) || rcv.rx.lost != 0 {
		t.Fatalf("wraparound broke the stream: %x lost=%d", out, rcv.rx.lost)
	}
}

func TestReceiverIgnoresJournal(t *testing.T) {
	// A command list followed by a (junk) journal must decode exactly the list.
	rcv := newLink("r", 2, 1, nil, nil, false)
	sec := buildSection(h(t, "903C64 00 803C00"), h(t, "A0 00 12 34 56 78 FF FF FF"))
	pkt := rtpHeader{marker: true, pt: 97, seq: 10, ts: 1, ssrc: 9}.appendTo(nil)
	hd, pl, err := parseRTP(append(pkt, sec...))
	if err != nil {
		t.Fatal(err)
	}
	if out := rcv.receive(hd, pl, quietLogger()); !bytes.Equal(out, h(t, "903C64 803C00")) {
		t.Fatalf("got %x", out)
	}
}

// wellFormed reports whether b is a stream a downstream parser can trust: every
// message has an explicit status byte and exactly its data octets (all below
// 0x80), SysEx is F0 data… F7 and nothing else sits inside it.
func wellFormed(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80, c == 0xF7:
			return false
		case c >= 0xF8:
			i++
		case c == 0xF0:
			j := i + 1
			for j < len(b) && b[j] < 0x80 {
				j++
			}
			if j >= len(b) || b[j] != 0xF7 {
				return false
			}
			i = j + 1
		default:
			n := dataLen(c)
			if i+n >= len(b) {
				return false
			}
			for k := 1; k <= n; k++ {
				if b[i+k] >= 0x80 {
					return false
				}
			}
			i += 1 + n
		}
	}
	return true
}

func TestRunningStatusDataOctetsAreValidated(t *testing.T) {
	cases := []struct{ name, list, wantOut string }{
		{"F0 as data", "903C40 00 3C F0", "903C40"},
		{"F7 as data", "903C40 00 3C F7", "903C40"},
		{"realtime as data", "903C40 00 3C F8", "903C40"},
		{"active sensing as data", "903C40 00 3C FE", "903C40"},
		{"status as data", "903C40 00 3C 80", "903C40"},
		{"after other commands", "C005 00 F8 00 903C40 00 3C F0", "C005 F8 903C40"},
		{"rest of the list is dropped", "903C40 00 3C F0 00 3E 40 00 F8", "903C40"},
		{"controller", "B00764 00 07 FA", "B00764"},
	}
	for _, c := range cases {
		var d listDecoder
		out, err := d.decode(h(t, c.list), false, nil)
		if !errors.Is(err, errBadList) {
			t.Errorf("%s: error %v", c.name, err)
		}
		if !bytes.Equal(out, h(t, c.wantOut)) {
			t.Errorf("%s: out %x, want %s", c.name, out, c.wantOut)
		}
		if bytes.Contains(out, []byte{0xF0}) && !strings.Contains(c.wantOut, "F0") {
			t.Errorf("%s: F0 leaked into the output", c.name)
		}
		if d.running != 0 {
			t.Errorf("%s: running status %02X survived the error", c.name, d.running)
		}
		// The decoder is usable afterwards.
		if got := decodeAll(t, &d, "903C64", false); !bytes.Equal(got, h(t, "903C64")) {
			t.Errorf("%s: next list: %x", c.name, got)
		}
	}
	// A one-data-octet running status has nothing more to check: the octet is
	// below 0x80 by the time it is taken for running status.
	var d listDecoder
	if got := decodeAll(t, &d, "C005 00 06 00 F8", false); !bytes.Equal(got, h(t, "C005 C006 F8")) {
		t.Errorf("program change running status: %x", got)
	}
}

func TestRunningStatusViolationThroughTheLink(t *testing.T) {
	rcv := newLink("r", 2, 1, nil, nil, false)
	rcv.releaseOnLoss = true
	pkt := rtpHeader{marker: true, pt: 97, seq: 1, ts: 1, ssrc: 1}.appendTo(nil)
	pkt = append(pkt, buildSection(h(t, "903C40 00 3C F0 00 F8"), nil)...)
	hd, pl, err := parseRTP(pkt)
	if err != nil {
		t.Fatal(err)
	}
	out := rcv.receive(hd, pl, quietLogger())
	if !bytes.Equal(out, h(t, "903C40")) {
		t.Fatalf("out %x", out)
	}
	// The held-note tracker saw only the valid command.
	if offs := rcv.held.release(); !bytes.Equal(offs, h(t, "803C00")) {
		t.Fatalf("held notes: %x", offs)
	}
}

func TestDecoderOutputIsAlwaysWellFormed(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	b := NewByteBudget(1 << 20)
	d := listDecoder{budget: b}
	for i := 0; i < 30000; i++ {
		list := make([]byte, 1+r.Intn(24))
		for j := range list {
			switch r.Intn(7) {
			case 0:
				list[j] = byte(0x80 + r.Intn(128))
			case 1:
				list[j] = 0xF0
			case 2:
				list[j] = 0xF7
			case 3:
				list[j] = 0xF8 + byte(r.Intn(8))
			default:
				list[j] = byte(r.Intn(128))
			}
		}
		out, _ := d.decode(list, r.Intn(2) == 0, nil)
		if !wellFormed(out) {
			t.Fatalf("list %x produced a malformed stream %x", list, out)
		}
		if r.Intn(40) == 0 {
			d.reset()
		}
	}
	d.discard()
	if b.InUse() != 0 {
		t.Fatalf("budget not returned: %d", b.InUse())
	}
}

func TestWellFormedHelper(t *testing.T) {
	for _, s := range []string{"903C40", "903C40 F8 903E40", "F0 01 02 F7", "C005", "F8", "F2 01 02", ""} {
		if !wellFormed(h(t, s)) {
			t.Errorf("%q should be well formed", s)
		}
	}
	for _, s := range []string{"903C", "903C F0", "3C40", "F0 01", "F7", "90 3C F8 40", "F0 01 90 F7"} {
		if wellFormed(h(t, s)) {
			t.Errorf("%q should be malformed", s)
		}
	}
}

func TestEncoderNeverEmitsEmptyFirstSegment(t *testing.T) {
	// F0 alone, then a realtime byte, then the rest.
	e := newEncoder()
	if got := e.encode(h(t, "F0")); len(got) != 0 {
		t.Fatalf("a lone F0 produced %x", got)
	}
	if got := e.encode(h(t, "F8")); !equalCmds(got, [][]byte{h(t, "F8")}) {
		t.Fatalf("realtime after F0: %x", got)
	}
	if got := e.encode(h(t, "43 10")); !equalCmds(got, [][]byte{h(t, "F0 43 10 F0")}) {
		t.Fatalf("first piece: %x", got)
	}
	if got := e.encode(h(t, "7F F7")); !equalCmds(got, [][]byte{h(t, "F7 7F F7")}) {
		t.Fatalf("last piece: %x", got)
	}

	// Realtime byte right behind the F0 inside one Feed.
	e = newEncoder()
	if got := e.encode(h(t, "F0 F8 01 F7")); !equalCmds(got, [][]byte{h(t, "F8"), h(t, "F0 01 F7")}) {
		t.Fatalf("F0 F8 01 F7: %x", got)
	}

	// The message is empty: F0 and F7 arrive separately.
	e = newEncoder()
	var got [][]byte
	got = append(got, e.encode(h(t, "F0"))...)
	got = append(got, e.encode(h(t, "F7"))...)
	if !equalCmds(got, [][]byte{h(t, "F0 F7")}) {
		t.Fatalf("empty message: %x", got)
	}

	// Aborted before any data: the peer never heard of the message, so there is
	// nothing to cancel and no stray segment.
	e = newEncoder()
	got = e.encode(h(t, "F0"))
	got = append(got, e.encode(h(t, "903C64"))...)
	if !equalCmds(got, [][]byte{h(t, "903C64")}) {
		t.Fatalf("aborted F0: %x", got)
	}
	// ... and the encoder is clean afterwards: the next message starts normally.
	if got := e.encode(h(t, "F0 09 F7")); !equalCmds(got, [][]byte{h(t, "F0 09 F7")}) {
		t.Fatalf("message after abort: %x", got)
	}
	// A new F0 supersedes a held one.
	e = newEncoder()
	e.encode(h(t, "F0"))
	if got := e.encode(h(t, "F0 05 06 F7")); !equalCmds(got, [][]byte{h(t, "F0 05 06 F7")}) {
		t.Fatalf("restart: %x", got)
	}

	// Every way of chopping a message up gives legal segments (none empty except
	// the last) that the decoder puts back together.
	msg := append(append([]byte{0xF0}, bytes.Repeat([]byte{0x2A, 0x15, 0x01}, 7)...), 0xF7)
	for size := 1; size <= len(msg); size++ {
		for _, rt := range []bool{false, true} {
			e := newEncoder()
			var cmds [][]byte
			chunks := split(msg, size)
			for i, ch := range chunks {
				cmds = append(cmds, e.encode(ch)...)
				if rt && i < len(chunks)-1 { // a realtime byte between the chunks
					cmds = append(cmds, e.encode([]byte{0xF8})...)
				}
			}
			var d listDecoder
			var out []byte
			for _, c := range cmds {
				if len(c) == 2 && c[1] == 0xF0 {
					t.Fatalf("size %d: empty segment %x in %x", size, c, cmds)
				}
				var err error
				if out, err = d.decode(c, false, out); err != nil {
					t.Fatalf("size %d: %v", size, err)
				}
			}
			want := msg
			if rt {
				want = append(bytes.Repeat([]byte{0xF8}, len(cmds)-countSegs(cmds)), msg...)
			}
			if !bytes.Equal(out, want) {
				t.Fatalf("size %d rt=%v: round trip %x, want %x", size, rt, out, want)
			}
		}
	}
}

// countSegs counts the SysEx segments among cmds (everything that is not a lone F8).
func countSegs(cmds [][]byte) int {
	n := 0
	for _, c := range cmds {
		if !(len(c) == 1 && c[0] == 0xF8) {
			n++
		}
	}
	return n
}
