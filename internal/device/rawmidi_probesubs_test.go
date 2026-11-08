package device

import (
	"errors"
	"fmt"
	"reflect"
	"syscall"
	"testing"
)

// rawFakeKernel answers SNDRV_CTL_IOCTL_RAWMIDI_INFO queries the way
// __snd_rawmidi_info_select() in sound/core/rawmidi.c does, errno for errno:
//
//	no such raw MIDI device                -> ENXIO
//	stream is neither output nor input     -> EINVAL
//	the stream has no substreams at all    -> ENOENT   (not ENXIO!)
//	subdevice >= substream count           -> ENXIO
//
// The USB MIDI driver sizes the two directions independently, so a device
// with zero inputs or zero outputs is normal.
type rawFakeKernel struct {
	// outs and ins are the substream counts of each raw MIDI device number.
	outs, ins map[int]int
}

func (k *rawFakeKernel) info(dev int) rawmidiInfoFunc {
	return func(sub int, stream int32) (sndRawmidiInfo, syscall.Errno) {
		_, outOK := k.outs[dev]
		_, inOK := k.ins[dev]
		if !outOK && !inOK {
			return sndRawmidiInfo{}, syscall.ENXIO
		}
		var count int
		var dir string
		switch stream {
		case sndRawmidiStreamOutput:
			count, dir = k.outs[dev], "out"
		case sndRawmidiStreamInput:
			count, dir = k.ins[dev], "in"
		default:
			return sndRawmidiInfo{}, syscall.EINVAL
		}
		if count == 0 {
			return sndRawmidiInfo{}, syscall.ENOENT
		}
		if sub >= count {
			return sndRawmidiInfo{}, syscall.ENXIO
		}
		info := sndRawmidiInfo{Device: uint32(dev), Subdevice: uint32(sub), Stream: stream, SubdevicesCount: uint32(count)}
		copy(info.Subname[:], fmt.Sprintf("%s %d", dir, sub))
		return info, 0
	}
}

func TestProbeSubdevicesDirections(t *testing.T) {
	tests := []struct {
		name string
		outs int
		ins  int
		want []rawSub
	}{
		{"bidirectional", 1, 1, []rawSub{{Sub: 0, Name: "in 0", Input: true, Output: true}}},
		{"input only (keyboard, pad controller)", 0, 1, []rawSub{{Sub: 0, Name: "in 0", Input: true}}},
		{"output only (sound module)", 1, 0, []rawSub{{Sub: 0, Name: "out 0", Output: true}}},
		{"two inputs, no output", 0, 2, []rawSub{
			{Sub: 0, Name: "in 0", Input: true}, {Sub: 1, Name: "in 1", Input: true}}},
		{"three outputs, one input", 3, 1, []rawSub{
			{Sub: 0, Name: "in 0", Input: true, Output: true},
			{Sub: 1, Name: "out 1", Output: true},
			{Sub: 2, Name: "out 2", Output: true}}},
		{"neither direction", 0, 0, []rawSub{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := &rawFakeKernel{outs: map[int]int{0: tc.outs}, ins: map[int]int{0: tc.ins}}
			got, err := probeSubdevices(1, 0, k.info(0))
			if err != nil {
				t.Fatalf("Probe failed (the empty direction must not be an error): %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestProbeDirectionEmptyIsNotAnError(t *testing.T) {
	k := &rawFakeKernel{outs: map[int]int{0: 0}, ins: map[int]int{0: 1}}
	names, err := probeDirection(k.info(0), sndRawmidiStreamOutput)
	if err != nil || len(names) != 0 {
		t.Fatalf("empty output direction: names=%v err=%v", names, err)
	}
	names, err = probeDirection(k.info(0), sndRawmidiStreamInput)
	if err != nil || len(names) != 1 {
		t.Fatalf("input direction: names=%v err=%v", names, err)
	}
}

// A device that vanishes between discovery and the probe answers ENXIO; that
// must not be reported as a probe error either (the caller then sees an empty
// device and the next scan settles it).
func TestProbeSubdevicesVanishedDevice(t *testing.T) {
	k := &rawFakeKernel{outs: map[int]int{}, ins: map[int]int{}}
	got, err := probeSubdevices(1, 7, k.info(7))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// Real failures still surface: only the two "none" errnos are swallowed.
func TestProbeDirectionOtherErrorsPropagate(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.EPERM, syscall.EIO, syscall.ENODEV, syscall.EACCES} {
		info := func(sub int, stream int32) (sndRawmidiInfo, syscall.Errno) { return sndRawmidiInfo{}, errno }
		if _, err := probeDirection(info, sndRawmidiStreamOutput); !errors.Is(err, errno) {
			t.Errorf("errno %v: got %v", errno, err)
		}
		_, err := probeSubdevices(2, 3, info)
		if !errors.Is(err, errno) {
			t.Errorf("probeSubdevices errno %v: got %v", errno, err)
		}
	}
}

// ENXIO for a subdevice above 0 is a real inconsistency (the count said there
// were more), not "none": it must propagate.
func TestProbeDirectionMidListFailurePropagates(t *testing.T) {
	info := func(sub int, stream int32) (sndRawmidiInfo, syscall.Errno) {
		if sub >= 1 {
			return sndRawmidiInfo{}, syscall.ENXIO
		}
		return sndRawmidiInfo{SubdevicesCount: 3}, 0
	}
	if _, err := probeDirection(info, sndRawmidiStreamInput); !errors.Is(err, syscall.ENXIO) {
		t.Fatalf("got %v", err)
	}
}

func TestIsNoSubdevices(t *testing.T) {
	for errno, want := range map[syscall.Errno]bool{
		syscall.ENOENT: true, syscall.ENXIO: true,
		syscall.EINVAL: false, syscall.EPERM: false, syscall.EBUSY: false, syscall.ENODEV: false, 0: false,
	} {
		if got := isNoSubdevices(errno); got != want {
			t.Errorf("isNoSubdevices(%v) = %v, want %v", errno, got, want)
		}
	}
}

// ---- through discovery ------------------------------------------------------

// rawKernelAccess is a rawAccess whose Probe goes through the kernel emulation
// above (and the same probeSubdevices the Linux backend uses), with one
// emulated kernel per card.
type rawKernelAccess struct {
	*rawFakeAccess
	kernels map[int]*rawFakeKernel
}

func (a rawKernelAccess) Probe(card, dev int) ([]rawSub, error) {
	return probeSubdevices(card, dev, a.kernels[card].info(dev))
}

// The reported failure: an input-only USB device must come up as an
// input-only port, not as a bidirectional stand-in that can never be opened
// (and an output-only one likewise).
func TestDiscoverInputOnlyAndOutputOnlyDevices(t *testing.T) {
	sys := newRawFakeSys(t)
	keys := sys.usb("1-1.1", rawUSBAttrs{vid: "0a0a", pid: "0001", manufacturer: "Acme", product: "Keys", serial: "KEYS1"})
	sys.midi(keys, 1, 0)
	mod := sys.usb("1-1.2", rawUSBAttrs{vid: "0a0a", pid: "0002", manufacturer: "Acme", product: "Module", serial: "MOD1", devnum: "3"})
	sys.midi(mod, 2, 0)

	acc := rawKernelAccess{rawFakeAccess: newRawFakeAccess(t), kernels: map[int]*rawFakeKernel{
		1: {outs: map[int]int{0: 0}, ins: map[int]int{0: 1}}, // one MIDI IN, no MIDI OUT
		2: {outs: map[int]int{0: 1}, ins: map[int]int{0: 0}}, // one MIDI OUT, no MIDI IN
	}}

	got, err := rawDiscover(sys.opts(), acc, rawDiscardLogger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 ports, got %+v", got)
	}
	byCard := map[int]FoundPort{}
	for _, p := range got {
		byCard[p.Card] = p
	}
	if p := byCard[1]; !p.Input || p.Output {
		t.Errorf("input-only keyboard discovered as input=%v output=%v", p.Input, p.Output)
	}
	if p := byCard[2]; p.Input || !p.Output {
		t.Errorf("output-only module discovered as input=%v output=%v", p.Input, p.Output)
	}
}
