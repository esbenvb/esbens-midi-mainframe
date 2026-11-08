package rtpmidi

import (
	"bytes"
	"testing"
)

func h(t *testing.T, s string) []byte {
	t.Helper()
	var out []byte
	var cur, n int
	for _, c := range s {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			continue
		}
		cur = cur<<4 | v
		n++
		if n == 2 {
			out = append(out, byte(cur))
			cur, n = 0, 0
		}
	}
	if n != 0 {
		t.Fatalf("odd number of hex digits in %q", s)
	}
	return out
}

func TestAppleMIDIVectors(t *testing.T) {
	cases := []struct {
		name string
		pkt  []byte
		want []byte
	}{
		{"IN", invitation{cmd: cmdIN, version: 2, token: 0x11223344, ssrc: 0xAABBCCDD, name: "EMM"}.marshal(),
			h(t, "FFFF 494E 00000002 11223344 AABBCCDD 454D4D00")},
		{"OK", invitation{cmd: cmdOK, version: 2, token: 0x11223344, ssrc: 0xAABBCCDD, name: "EMM"}.marshal(),
			h(t, "FFFF 4F4B 00000002 11223344 AABBCCDD 454D4D00")},
		{"NO", invitation{cmd: cmdNO, version: 2, token: 0x11223344, ssrc: 0xAABBCCDD}.marshal(),
			h(t, "FFFF 4E4F 00000002 11223344 AABBCCDD")},
		{"BY", invitation{cmd: cmdBY, version: 2, token: 0x11223344, ssrc: 0xAABBCCDD}.marshal(),
			h(t, "FFFF 4259 00000002 11223344 AABBCCDD")},
		{"CK1", clockSync{ssrc: 0xAABBCCDD, count: 1, ts: [3]uint64{1, 2, 0}}.marshal(),
			h(t, "FFFF 434B AABBCCDD 01 000000 0000000000000001 0000000000000002 0000000000000000")},
		{"CK2", clockSync{ssrc: 0xAABBCCDD, count: 2, ts: [3]uint64{0x0102030405060708, 2, 0xFFFFFFFFFFFFFFFF}}.marshal(),
			h(t, "FFFF 434B AABBCCDD 02 000000 0102030405060708 0000000000000002 FFFFFFFFFFFFFFFF")},
		{"RS", feedback{ssrc: 0xAABBCCDD, seq: 0x1234}.marshal(),
			h(t, "FFFF 5253 AABBCCDD 12340000")},
	}
	for _, c := range cases {
		if !bytes.Equal(c.pkt, c.want) {
			t.Errorf("%s: got  %x\n    want %x", c.name, c.pkt, c.want)
		}
	}
}

func TestAppleMIDIParse(t *testing.T) {
	p, err := parseApple(h(t, "FFFF 494E 00000002 11223344 AABBCCDD 4D61632D4D696469 00"))
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := p.(*invitation)
	if !ok || inv.cmd != cmdIN || inv.version != 2 || inv.token != 0x11223344 || inv.ssrc != 0xAABBCCDD || inv.name != "Mac-Midi" {
		t.Fatalf("bad IN: %+v", p)
	}
	// Name without terminator, as some implementations send it.
	p, _ = parseApple(h(t, "FFFF 4F4B 00000002 11223344 AABBCCDD 4142"))
	if p.(*invitation).name != "AB" {
		t.Fatalf("unterminated name: %+v", p)
	}
	// NO and BY without a name.
	p, _ = parseApple(h(t, "FFFF 4E4F 00000002 00000001 00000002"))
	if p.(*invitation).cmd != cmdNO {
		t.Fatal("NO")
	}
	p, _ = parseApple(h(t, "FFFF 4259 00000002 00000001 00000002"))
	if p.(*invitation).cmd != cmdBY {
		t.Fatal("BY")
	}
	p, err = parseApple(h(t, "FFFF 434B AABBCCDD 01 000000 0000000000000001 0000000000000002 0000000000000003"))
	if err != nil {
		t.Fatal(err)
	}
	ck := p.(*clockSync)
	if ck.count != 1 || ck.ssrc != 0xAABBCCDD || ck.ts != [3]uint64{1, 2, 3} {
		t.Fatalf("bad CK: %+v", ck)
	}
	// RS: both the upper-16-bit and the low-16-bit conventions parse to the same number.
	for _, s := range []string{"FFFF 5253 AABBCCDD 12340000", "FFFF 5253 AABBCCDD 00001234"} {
		p, err = parseApple(h(t, s))
		if err != nil || p.(*feedback).seq != 0x1234 {
			t.Fatalf("RS %s: %+v %v", s, p, err)
		}
	}
	for _, bad := range []string{"FFFF 494E 00000002", "FFFF 434B AABBCCDD 01", "FFFF 5253 AABB", "FFFF 434B AABBCCDD 07 000000 " + string(bytes.Repeat([]byte("00"), 24))} {
		if _, err := parseApple(h(t, bad)); err == nil {
			t.Errorf("accepted malformed packet %q", bad)
		}
	}
	if p, err := parseApple(h(t, "FFFF 5858 00000000")); p != nil || err != nil {
		t.Errorf("unknown command: %v %v", p, err)
	}
	if _, err := parseApple(h(t, "80E1 0000")); err != errNotApple {
		t.Errorf("RTP mistaken for AppleMIDI: %v", err)
	}
}

func TestRTPHeader(t *testing.T) {
	hd := rtpHeader{marker: true, pt: 97, seq: 0x0102, ts: 0x03040506, ssrc: 0x0708090A}
	b := hd.appendTo(nil)
	want := h(t, "80E1 0102 03040506 0708090A")
	if !bytes.Equal(b, want) {
		t.Fatalf("got %x want %x", b, want)
	}
	got, payload, err := parseRTP(append(b, 1, 2, 3))
	if err != nil || got != hd || !bytes.Equal(payload, []byte{1, 2, 3}) {
		t.Fatalf("parse: %+v %x %v", got, payload, err)
	}
	// Marker clear.
	if b := (rtpHeader{pt: 97}).appendTo(nil); b[1] != 0x61 {
		t.Fatalf("pt byte %02x", b[1])
	}
}

func TestParseRTPSkipsCSRCExtensionPadding(t *testing.T) {
	// V=2, P=1, X=1, CC=2; 2 CSRCs; extension of 1 word; payload AA BB; 3 bytes padding.
	pkt := h(t, "B2 61 0001 00000000 00000001"+
		"11111111 22222222"+ // CSRCs
		"BEDE 0001 99999999"+ // extension header (len=1 word) + word
		"AABB"+
		"000003") // padding count 3
	_, payload, err := parseRTP(pkt)
	if err != nil || !bytes.Equal(payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("payload %x err %v", payload, err)
	}
	for _, bad := range [][]byte{nil, make([]byte, 11), h(t, "40 61 0001 00000000 00000001")} {
		if _, _, err := parseRTP(bad); err == nil {
			t.Errorf("accepted %x", bad)
		}
	}
	// Extension claiming more words than present.
	if _, _, err := parseRTP(h(t, "90 61 0001 00000000 00000001 BEDE 0009")); err == nil {
		t.Error("accepted truncated extension")
	}
}
