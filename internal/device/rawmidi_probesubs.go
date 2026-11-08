package device

import (
	"fmt"
	"syscall"
)

// Subdevice probing, separated from the ioctl itself so the logic that
// interprets the kernel's answers can be unit-tested on any OS (this file has
// no build tag; the Linux half supplies the real ioctl through rawmidiInfoFunc).

// rawmidiInfoFunc asks the kernel about one subdevice of one direction of one
// raw MIDI device, the way SNDRV_CTL_IOCTL_RAWMIDI_INFO does on the control
// node. Only the Info, SubdevicesCount and Subname fields of the result are
// used.
type rawmidiInfoFunc func(sub int, stream int32) (sndRawmidiInfo, syscall.Errno)

// isNoSubdevices reports whether the errno of an info query for subdevice 0
// means "this direction has no subdevices". sound/core/rawmidi.c
// (__snd_rawmidi_info_select) answers in this order: ENXIO if the raw MIDI
// device does not exist, EINVAL for a bad stream, ENOENT if the stream has no
// substreams at all, and only then ENXIO if the subdevice index is out of
// range. So an input-only or output-only device (zero substreams in one
// direction, which the USB MIDI driver creates freely) yields ENOENT; ENXIO
// for subdevice 0 means the device vanished, and is treated as empty too.
func isNoSubdevices(errno syscall.Errno) bool {
	return errno == syscall.ENOENT || errno == syscall.ENXIO
}

// probeDirection lists the subdevice names of one direction. A direction with
// no subdevices yields an empty list and no error.
func probeDirection(info rawmidiInfoFunc, stream int32) (names []string, err error) {
	first, errno := info(0, stream)
	if isNoSubdevices(errno) {
		return nil, nil
	}
	if errno != 0 {
		return nil, errno
	}
	n := int(first.SubdevicesCount)
	if n > 256 {
		n = 256
	}
	names = make([]string, 0, n)
	names = append(names, cString(first.Subname[:]))
	for i := 1; i < n; i++ {
		next, errno := info(i, stream)
		if errno != 0 {
			return names, errno
		}
		names = append(names, cString(next.Subname[:]))
	}
	return names, nil
}

// probeSubdevices probes both directions of raw MIDI device (card, dev) and
// merges them into one rawSub per cable: subdevice i is an input if the
// device has at least i+1 input subdevices, and an output likewise.
func probeSubdevices(card, dev int, info rawmidiInfoFunc) ([]rawSub, error) {
	outs, err := probeDirection(info, sndRawmidiStreamOutput)
	if err != nil {
		return nil, fmt.Errorf("probe outputs of card %d device %d: %w", card, dev, err)
	}
	ins, err := probeDirection(info, sndRawmidiStreamInput)
	if err != nil {
		return nil, fmt.Errorf("probe inputs of card %d device %d: %w", card, dev, err)
	}
	n := len(outs)
	if len(ins) > n {
		n = len(ins)
	}
	subs := make([]rawSub, 0, n)
	for i := 0; i < n; i++ {
		s := rawSub{Sub: i, Input: i < len(ins), Output: i < len(outs)}
		switch {
		case i < len(ins) && ins[i] != "":
			s.Name = ins[i]
		case i < len(outs):
			s.Name = outs[i]
		}
		subs = append(subs, s)
	}
	return subs, nil
}
