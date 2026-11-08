package device

import "bytes"

// ALSA raw MIDI control ioctls, computed with the generic Linux _IOC encoding
// (dir<<30 | size<<16 | type<<8 | nr), which is what arm, arm64 and x86 use.
// Values are checked against include/uapi/sound/asound.h in the tests.
//
// This file is deliberately free of build tags and of any syscall use, so the
// numbers and the struct layout can be unit-tested on any OS.

const (
	iocWrite = 1
	iocRead  = 2

	// sizeof(struct snd_rawmidi_info). The kernel struct is
	//   u32 device, u32 subdevice, i32 stream, i32 card, u32 flags,
	//   u8 id[64], u8 name[80], u8 subname[32],
	//   u32 subdevices_count, u32 subdevices_avail,
	//   i32 tied_device (older kernels: part of reserved[]), u8 reserved[60]
	sndRawmidiInfoSize = 268

	// enum snd_rawmidi_stream
	sndRawmidiStreamOutput = 0
	sndRawmidiStreamInput  = 1
)

func ioc(dir, typ, nr, size uint32) uint32 {
	return dir<<30 | size<<16 | typ<<8 | nr
}

const (
	// _IOWR('U', 0x41, struct snd_rawmidi_info) on /dev/snd/controlC<card>
	sndCtlIoctlRawmidiInfo = uint32(iocRead|iocWrite)<<30 | sndRawmidiInfoSize<<16 | 'U'<<8 | 0x41
	// _IOW('U', 0x42, int) on /dev/snd/controlC<card>
	sndCtlIoctlRawmidiPreferSubdevice = uint32(iocWrite)<<30 | 4<<16 | 'U'<<8 | 0x42
	// _IOR('W', 0x01, struct snd_rawmidi_info) on /dev/snd/midiC<card>D<dev>
	sndRawmidiIoctlInfo = uint32(iocRead)<<30 | sndRawmidiInfoSize<<16 | 'W'<<8 | 0x01
)

// sndRawmidiInfo mirrors struct snd_rawmidi_info.
type sndRawmidiInfo struct {
	Device          uint32
	Subdevice       uint32
	Stream          int32
	Card            int32
	Flags           uint32
	ID              [64]byte
	Name            [80]byte
	Subname         [32]byte
	SubdevicesCount uint32
	SubdevicesAvail uint32
	TiedDevice      int32
	Reserved        [60]byte
}

// cString returns b up to the first NUL.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
