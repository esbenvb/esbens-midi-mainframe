// Package router is the heart of EMM: a patchbay that moves MIDI between
// ports (USB devices, network sessions, Bluetooth, virtual ports).
//
// Ports come and go (hot-plug). Connections ("from → to", with an optional
// filter) are independent of that: a connection whose port is missing is simply
// dormant and wakes up when the port returns.
package router

import (
	"context"

	"emm/internal/midi"
)

// PortKind says what a port is, mostly for display and grouping.
type PortKind string

const (
	KindUSB     PortKind = "usb"     // USB MIDI device (one port per cable)
	KindRTP     PortKind = "rtp"     // RTP-MIDI / AppleMIDI network port
	KindBLE     PortKind = "ble"     // Bluetooth LE MIDI
	KindVirtual PortKind = "virtual" // internal bus
	KindSim     PortKind = "sim"     // simulated device (development)
)

// IsDevice reports whether ports of this kind stand for a physical (or
// simulated) device, as opposed to a network, Bluetooth or virtual port. Only
// device ports get a paired network MIDI endpoint.
func (k PortKind) IsDevice() bool { return k == KindUSB || k == KindSim }

// PortInfo is the static description of a port.
type PortInfo struct {
	// ID is stable across reconnects and reboots; connections are keyed by it.
	ID string `json:"id"`
	// Name is the default display name (users may alias it).
	Name string   `json:"name"`
	Kind PortKind `json:"kind"`
	// Group ties ports of one physical device together (e.g. the three ports
	// of one interface), purely for display.
	Group string `json:"group,omitempty"`
	// Input: the port produces MIDI (it is a source in the matrix).
	// Output: the port accepts MIDI (it is a destination in the matrix).
	Input  bool `json:"input"`
	Output bool `json:"output"`
	// Detail is free text for the UI, e.g. "USB 1.2 · 04b3:4b19".
	Detail string `json:"detail,omitempty"`
}

// Port is the thing a backend hands to the router.
type Port interface {
	Info() PortInfo
	// Write sends one chunk of raw MIDI bytes (a complete message, or a piece of
	// a SysEx). It should block until the bytes are accepted by the device, so
	// flow control reaches the router. It is called from a single goroutine.
	Write(p []byte) error
	// Close releases the port. Called once, after the router stops writing.
	Close() error
}

// Host is what backends talk to; *Router implements it.
type Host interface {
	// AddPort registers p. The returned handle is how the port feeds received
	// bytes into the router. Adding an ID that already exists replaces it.
	AddPort(p Port) *Handle
	// RemovePort unregisters the port with that ID (and closes it).
	RemovePort(id string)
}

// Backend discovers ports and keeps the Host up to date until ctx ends.
type Backend interface {
	Name() string
	// Run blocks until ctx is cancelled, adding and removing ports on h.
	Run(ctx context.Context, h Host) error
}

// Filter limits and rewrites what flows through one connection.
type Filter struct {
	// Block lists categories to drop: note, poly_at, cc, program, chan_at,
	// bend, sysex, clock, transport, active_sense, reset, sys_common, other.
	Block []string `json:"block,omitempty"`
	// Channels is the allow-list of channels (1..16). Empty means all.
	Channels []int `json:"channels,omitempty"`
	// MapChannel (1..16) forces every channel message onto that channel.
	MapChannel int `json:"map_channel,omitempty"`
	// Transpose shifts notes by this many semitones (-48..48).
	Transpose int `json:"transpose,omitempty"`
}

// DefaultFilter is what a new connection gets: pass everything except Active
// Sensing, which most hardware (and routers) are happier without.
func DefaultFilter() Filter {
	return Filter{Block: []string{midi.CatActiveSense.String()}}
}

// ConnSpec is the persistable form of a connection.
type ConnSpec struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Filter Filter `json:"filter"`
}
