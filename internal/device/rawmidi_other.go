//go:build !linux

package device

import (
	"context"

	"emm/internal/router"
)

// On non-Linux systems there is no ALSA raw MIDI. Discovery against a fake or
// mounted sysfs tree still works (it is plain file reading), which is what the
// unit tests rely on; opening devices does not.

// NewRawMIDI returns a backend that fails immediately: raw MIDI access is only
// available on Linux.
func NewRawMIDI(o RawMIDIOptions) router.Backend { return unsupportedRawBackend{} }

type unsupportedRawBackend struct{}

func (unsupportedRawBackend) Name() string { return "rawmidi" }
func (unsupportedRawBackend) Run(ctx context.Context, h router.Host) error {
	return errRawUnsupported
}

func defaultAccess(o RawMIDIOptions) rawAccess { return unavailableRawAccess{} }

type unavailableRawAccess struct{}

func (unavailableRawAccess) Probe(card, dev int) ([]rawSub, error) { return nil, errRawUnsupported }
func (unavailableRawAccess) Open(card, dev, sub int, in, out bool) (rawNode, error) {
	return nil, errRawUnsupported
}
