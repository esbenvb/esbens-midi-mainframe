// Package rtpmidi implements RTP-MIDI (RFC 6295) with the AppleMIDI session
// protocol, so that macOS / iOS (CoreMIDI network sessions), rtpMIDI on
// Windows and rtpmidid on Linux can talk to an EMM network MIDI endpoint.
//
// It contains a responder (Server: one endpoint = one UDP control/data port
// pair), an initiator (Client), the wire formats, SysEx segmentation, mDNS
// announcement helpers and a Manager that bridges Servers to the router.
//
// # Verification status
//
// Everything here is covered by unit tests and by loopback tests in which
// Server and Client talk to each other. That proves the code agrees with
// itself and with hand-computed byte vectors from the specs; it does NOT prove
// interoperability. The following are UNVERIFIED against real peers
// (Apple CoreMIDI, rtpMIDI for Windows, rtpmidid):
//
//   - the exact interpretation of the 32-bit sequence number in RS (receiver
//     feedback) packets: we send it in the upper 16 bits and accept both forms;
//   - clock-synchronisation timing details (the Apple initiator's CK burst
//     pattern); we answer every CK0 with CK1 and accept CK2 whenever it comes;
//   - nothing about the recovery journal: it is not implemented (see journal.go).
//
// The receive path never interprets a peer's recovery journal: it always
// follows the MIDI command list inside the packet, so it is skipped by
// construction. Lost packets are therefore not recovered. A gap in the RTP
// sequence discards a half-received SysEx (a corrupted dump is never forwarded)
// and the running status, and (Config.ReleaseNotesOnLoss) releases the notes
// the peer was holding so that a lost Note Off cannot leave a note stuck.
//
// # Hostile networks
//
// AppleMIDI has no authentication; anyone who can reach the UDP ports can open
// a session. The responder therefore keeps what a stranger can cost it small:
// at most maxPeers established sessions per endpoint, a few half-open
// invitations (the oldest make room, nothing is refused), packets that name an
// established session (BY, CK, RS, RTP, a new IN) only count when they come from
// that session's IP address, SysEx reassembly and reassembled SysEx waiting for
// the router are paid for from one process-wide ByteBudget (SysexBudget; a
// message that cannot get budget is dropped), and names and log lines are
// capped and rate limited. None of this authenticates a peer: an attacker who
// can spoof an address on the LAN is not stopped.
package rtpmidi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// AppleMIDI session protocol constants.
const (
	appleVersion = 2

	cmdIN cmdKind = 'I'<<8 | 'N' // invitation
	cmdOK cmdKind = 'O'<<8 | 'K' // invitation accepted
	cmdNO cmdKind = 'N'<<8 | 'O' // invitation rejected
	cmdBY cmdKind = 'B'<<8 | 'Y' // end session
	cmdCK cmdKind = 'C'<<8 | 'K' // clock synchronisation
	cmdRS cmdKind = 'R'<<8 | 'S' // receiver feedback
)

type cmdKind uint16

func (c cmdKind) String() string { return string([]byte{byte(c >> 8), byte(c)}) }

var errNotApple = errors.New("rtpmidi: not an AppleMIDI packet")

// isAppleMIDI reports whether b is an AppleMIDI command packet (as opposed to
// an RTP packet): those start with 0xFF 0xFF.
func isAppleMIDI(b []byte) bool { return len(b) >= 4 && b[0] == 0xFF && b[1] == 0xFF }

// invitation is IN, OK, NO or BY.
type invitation struct {
	cmd     cmdKind
	version uint32
	token   uint32 // initiator token
	ssrc    uint32
	name    string // IN and OK only
}

func (p invitation) marshal() []byte {
	b := make([]byte, 16, 16+len(p.name)+1)
	b[0], b[1] = 0xFF, 0xFF
	binary.BigEndian.PutUint16(b[2:], uint16(p.cmd))
	binary.BigEndian.PutUint32(b[4:], p.version)
	binary.BigEndian.PutUint32(b[8:], p.token)
	binary.BigEndian.PutUint32(b[12:], p.ssrc)
	if p.cmd == cmdIN || p.cmd == cmdOK {
		b = append(b, p.name...)
		b = append(b, 0)
	}
	return b
}

// clockSync is a CK packet. Timestamps are in units of 100 µs (10 kHz).
type clockSync struct {
	ssrc  uint32
	count uint8 // 0, 1 or 2
	ts    [3]uint64
}

func (p clockSync) marshal() []byte {
	b := make([]byte, 36)
	b[0], b[1] = 0xFF, 0xFF
	binary.BigEndian.PutUint16(b[2:], uint16(cmdCK))
	binary.BigEndian.PutUint32(b[4:], p.ssrc)
	b[8] = p.count
	for i, t := range p.ts {
		binary.BigEndian.PutUint64(b[12+8*i:], t)
	}
	return b
}

// feedback is an RS packet: the sender of the RS has received everything up to seq.
type feedback struct {
	ssrc uint32
	seq  uint16
}

func (p feedback) marshal() []byte {
	b := make([]byte, 12)
	b[0], b[1] = 0xFF, 0xFF
	binary.BigEndian.PutUint16(b[2:], uint16(cmdRS))
	binary.BigEndian.PutUint32(b[4:], p.ssrc)
	// UNVERIFIED: the sequence number lives in the upper 16 bits.
	binary.BigEndian.PutUint32(b[8:], uint32(p.seq)<<16)
	return b
}

// parseApple decodes an AppleMIDI command packet into *invitation,
// *clockSync or *feedback. Unknown commands return (nil, nil).
func parseApple(b []byte) (any, error) {
	if !isAppleMIDI(b) {
		return nil, errNotApple
	}
	cmd := cmdKind(binary.BigEndian.Uint16(b[2:]))
	switch cmd {
	case cmdIN, cmdOK, cmdNO, cmdBY:
		if len(b) < 16 {
			return nil, fmt.Errorf("rtpmidi: short %s packet (%d bytes)", cmd, len(b))
		}
		p := &invitation{
			cmd:     cmd,
			version: binary.BigEndian.Uint32(b[4:]),
			token:   binary.BigEndian.Uint32(b[8:]),
			ssrc:    binary.BigEndian.Uint32(b[12:]),
		}
		if len(b) > 16 && (cmd == cmdIN || cmd == cmdOK || cmd == cmdNO) {
			name := b[16:]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			p.name = cleanPeerName(name)
		}
		return p, nil
	case cmdCK:
		if len(b) < 36 {
			return nil, fmt.Errorf("rtpmidi: short CK packet (%d bytes)", len(b))
		}
		p := &clockSync{ssrc: binary.BigEndian.Uint32(b[4:]), count: b[8]}
		for i := range p.ts {
			p.ts[i] = binary.BigEndian.Uint64(b[12+8*i:])
		}
		if p.count > 2 {
			return nil, fmt.Errorf("rtpmidi: bad CK count %d", p.count)
		}
		return p, nil
	case cmdRS:
		if len(b) < 12 {
			return nil, fmt.Errorf("rtpmidi: short RS packet (%d bytes)", len(b))
		}
		v := binary.BigEndian.Uint32(b[8:])
		seq := uint16(v >> 16)
		if v>>16 == 0 { // peer uses the low 16 bits
			seq = uint16(v)
		}
		return &feedback{ssrc: binary.BigEndian.Uint32(b[4:]), seq: seq}, nil
	}
	return nil, nil
}
