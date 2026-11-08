//go:build linux

package device

// The kernel-facing half of the raw MIDI backend. UNVERIFIED ON HARDWARE: it
// is compile-checked for linux/arm64 and linux/amd64 and vetted, and the
// ioctl numbers and struct size are checked against asound.h, but it has not
// been run against a real ALSA raw MIDI device.
//
// Two kernel behaviours matter here, both taken from sound/core/rawmidi.c and
// sound/core/control.c, and both are the places to look first if a real device
// misbehaves:
//
//  1. Subdevice selection is *per thread*. SNDRV_CTL_IOCTL_RAWMIDI_PREFER_SUBDEVICE
//     records the choice on the control file, and the kernel applies it to a
//     raw MIDI open() made by the thread that opened that control file
//     (snd_ctl_get_preferred_subdevice() compares task_pid(current)). Go
//     goroutines migrate between OS threads, so Open locks the goroutine to its
//     thread for the control-open / ioctl / node-open sequence. After the open
//     the chosen subdevice is read back with SNDRV_RAWMIDI_IOCTL_INFO and the
//     open is refused if the kernel picked another one.
//  2. A blocking open of a busy subdevice can sleep inside the kernel, which
//     would stall the hot-plug loop. The node is therefore opened O_NONBLOCK,
//     so a busy port fails at once with EBUSY and is retried with backoff.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"emm/internal/router"
)

// NewRawMIDI returns the Linux raw MIDI backend: it exposes every USB MIDI
// port (and optionally other raw MIDI devices) to the router and keeps the set
// up to date as devices are plugged and unplugged.
func NewRawMIDI(o RawMIDIOptions) router.Backend {
	o = o.withDefaults()
	return newRawBackend(o, defaultAccess(o))
}

func defaultAccess(o RawMIDIOptions) rawAccess {
	return &linuxAccess{devRoot: o.DevRoot, log: o.Logger}
}

// linuxAccess talks to /dev/snd.
type linuxAccess struct {
	devRoot string
	log     *slog.Logger
}

func ioctlPtr(fd uintptr, req uint32, p unsafe.Pointer) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(p))
	return errno
}

func (a *linuxAccess) openControl(card int) (int, error) {
	path := filepath.Join(a.devRoot, fmt.Sprintf("controlC%d", card))
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return fd, nil
}

// query asks the control node about one subdevice of one direction.
func queryRawmidi(ctl int, dev, sub int, stream int32) (sndRawmidiInfo, syscall.Errno) {
	info := sndRawmidiInfo{Device: uint32(dev), Subdevice: uint32(sub), Stream: stream}
	errno := ioctlPtr(uintptr(ctl), sndCtlIoctlRawmidiInfo, unsafe.Pointer(&info))
	return info, errno
}

// Probe implements rawAccess. The interpretation of the kernel's answers
// (which errno means "no subdevices in this direction") lives in
// probeSubdevices, which is shared with the tests.
func (a *linuxAccess) Probe(card, dev int) ([]rawSub, error) {
	ctl, err := a.openControl(card)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(ctl)

	return probeSubdevices(card, dev, func(sub int, stream int32) (sndRawmidiInfo, syscall.Errno) {
		return queryRawmidi(ctl, dev, sub, stream)
	})
}

// Open implements rawAccess. See the file comment for why it locks the thread.
func (a *linuxAccess) Open(card, dev, sub int, in, out bool) (rawNode, error) {
	var flag int
	switch {
	case in && out:
		flag = os.O_RDWR
	case in:
		flag = os.O_RDONLY
	case out:
		flag = os.O_WRONLY
	default:
		return nil, fmt.Errorf("port has no usable direction")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ctl, err := a.openControl(card)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(ctl) // must outlive the node open below

	pref := int32(sub)
	if errno := ioctlPtr(uintptr(ctl), sndCtlIoctlRawmidiPreferSubdevice, unsafe.Pointer(&pref)); errno != 0 {
		return nil, fmt.Errorf("select subdevice %d on card %d: %w", sub, card, errno)
	}

	path := filepath.Join(a.devRoot, fmt.Sprintf("midiC%dD%d", card, dev))
	f, err := os.OpenFile(path, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := a.verifySubdevice(f, sub, in, out); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// verifySubdevice reads back which subdevice the kernel actually opened. A
// failing info ioctl is not fatal (nothing to compare), a different
// subdevice is: routing MIDI to the wrong cable would be worse than an error.
func (a *linuxAccess) verifySubdevice(f *os.File, want int, in, out bool) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil
	}
	check := func(stream int32) error {
		info := sndRawmidiInfo{Stream: stream}
		var errno syscall.Errno
		if cerr := rc.Control(func(fd uintptr) {
			errno = ioctlPtr(fd, sndRawmidiIoctlInfo, unsafe.Pointer(&info))
		}); cerr != nil {
			return nil
		}
		if errno != 0 {
			a.log.Debug("rawmidi info ioctl failed; cannot verify subdevice", "err", errno)
			return nil
		}
		if int(info.Subdevice) != want {
			return fmt.Errorf("kernel opened subdevice %d instead of %d", info.Subdevice, want)
		}
		return nil
	}
	if out {
		if err := check(sndRawmidiStreamOutput); err != nil {
			return err
		}
	}
	if in {
		if err := check(sndRawmidiStreamInput); err != nil {
			return err
		}
	}
	return nil
}
