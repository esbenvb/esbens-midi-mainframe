package rtpmidi

import (
	"encoding/binary"
	"errors"
)

const (
	rtpHeaderLen = 12
	// payloadType is the dynamic payload type used by Apple, rtpMIDI and rtpmidid.
	payloadType = 97
)

var errBadRTP = errors.New("rtpmidi: malformed RTP packet")

type rtpHeader struct {
	marker bool
	pt     uint8
	seq    uint16
	ts     uint32
	ssrc   uint32
}

// appendTo appends the 12-byte RTP header (version 2, no padding, no
// extension, no CSRCs).
func (h rtpHeader) appendTo(dst []byte) []byte {
	b1 := h.pt & 0x7F
	if h.marker {
		b1 |= 0x80
	}
	dst = append(dst, 0x80, b1)
	dst = binary.BigEndian.AppendUint16(dst, h.seq)
	dst = binary.BigEndian.AppendUint32(dst, h.ts)
	dst = binary.BigEndian.AppendUint32(dst, h.ssrc)
	return dst
}

// parseRTP splits an RTP packet into header and payload. CSRC lists, header
// extensions and padding are skipped properly even though we never send them.
func parseRTP(b []byte) (rtpHeader, []byte, error) {
	var h rtpHeader
	if len(b) < rtpHeaderLen || b[0]>>6 != 2 {
		return h, nil, errBadRTP
	}
	cc := int(b[0] & 0x0F)
	h.marker = b[1]&0x80 != 0
	h.pt = b[1] & 0x7F
	h.seq = binary.BigEndian.Uint16(b[2:])
	h.ts = binary.BigEndian.Uint32(b[4:])
	h.ssrc = binary.BigEndian.Uint32(b[8:])
	i := rtpHeaderLen + 4*cc
	if b[0]&0x10 != 0 { // extension
		if len(b) < i+4 {
			return h, nil, errBadRTP
		}
		i += 4 + 4*int(binary.BigEndian.Uint16(b[i+2:]))
	}
	end := len(b)
	if b[0]&0x20 != 0 && end > 0 { // padding
		end -= int(b[end-1])
	}
	if i > end {
		return h, nil, errBadRTP
	}
	return h, b[i:end], nil
}
