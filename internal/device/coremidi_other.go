//go:build !(darwin && cgo)

package device

import (
	"context"
	"errors"

	"emm/internal/router"
)

// errCoreMIDIUnavailable is returned wherever CoreMIDI would be used on a
// platform (or build, e.g. CGO_ENABLED=0) that does not have it.
var errCoreMIDIUnavailable = errors.New("coremidi: only available on macOS (darwin build with cgo)")

type coreMIDIUnavailable struct{}

// NewCoreMIDI returns a backend whose Run fails: CoreMIDI exists only on macOS.
func NewCoreMIDI(CoreMIDIOptions) router.Backend { return coreMIDIUnavailable{} }

func (coreMIDIUnavailable) Name() string { return "coremidi" }

func (coreMIDIUnavailable) Run(context.Context, router.Host) error { return errCoreMIDIUnavailable }

// ListCoreMIDI always fails off macOS.
func ListCoreMIDI() ([]CoreMIDIEndpointInfo, error) { return nil, errCoreMIDIUnavailable }
