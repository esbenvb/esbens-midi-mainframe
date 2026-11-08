package blemidi

import (
	"log/slog"
	"time"
)

// PortID is the router port ID of the (first) Bluetooth adapter.
const PortID = "ble:0"

// Options configures a Server.
type Options struct {
	// Name is the local name advertised and shown to centrals (default "EMM").
	// Longer names are shortened by BlueZ in the advertisement; the 128-bit
	// service UUID leaves room for about eight characters in the 31-byte
	// advertising packet.
	Name string
	// Adapter is the BlueZ adapter, e.g. "hci0" (default).
	Adapter string
	Logger  *slog.Logger
	// OnChange is called (outside any lock) when Status changes.
	OnChange func()
	// Encrypt makes the characteristic require an encrypted link (the spec
	// recommends it). Centrals then pair on first connect ("Just Works").
	// Default false for the widest compatibility.
	Encrypt bool
	// PacketInterval is the minimum gap between notifications handed to
	// BlueZ (default 3 ms), so that a bulk transfer cannot flood it.
	PacketInterval time.Duration
}

func (o Options) withDefaults() Options {
	if o.Name == "" {
		o.Name = "EMM"
	}
	if o.Adapter == "" {
		o.Adapter = "hci0"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PacketInterval <= 0 {
		o.PacketInterval = 3 * time.Millisecond
	}
	return o
}

// Status describes the Bluetooth side for the web UI.
type Status struct {
	// Available: BlueZ and the adapter are present and the MIDI service is registered.
	Available bool `json:"available"`
	// Advertising: the LE advertisement is active, so centrals can find us.
	Advertising bool `json:"advertising"`
	// Connected is the number of centrals currently using the MIDI service.
	Connected int    `json:"connected"`
	Name      string `json:"name"`
	// Error is the last reason the service is not available ("" when fine).
	Error string `json:"error,omitempty"`
}
