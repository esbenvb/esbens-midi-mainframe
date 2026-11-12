package midi

import "fmt"

// Validate checks that data is a sequence of complete, well-formed MIDI
// messages: every message has its status byte (no running status), the right
// number of data bytes, and data bytes below 0x80. A SysEx must run from F0 to
// F7 with only data bytes between. Undefined status bytes (F4, F5, F9, FD) and a
// stray F7 are refused. It is what the console and the SysEx upload use before
// anything is sent to a device, so a typo cannot leave a device in the middle of
// a message.
func Validate(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("nothing to send")
	}
	for i := 0; i < len(data); {
		s := data[i]
		var n int // bytes in the message, status included
		switch {
		case s < 0x80:
			return fmt.Errorf("byte %d (0x%02X) is a data byte without a status byte before it", i+1, s)
		case s == 0xF0:
			end := i + 1
			for end < len(data) && data[end] < 0x80 {
				end++
			}
			if end >= len(data) || data[end] != 0xF7 {
				return fmt.Errorf("the SysEx starting at byte %d does not end with F7", i+1)
			}
			i = end + 1
			continue
		case s < 0xC0, s >= 0xE0 && s < 0xF0:
			n = 3 // note off/on, poly AT, CC, pitch bend
		case s < 0xE0:
			n = 2 // program change, channel AT
		case s == 0xF1 || s == 0xF3:
			n = 2
		case s == 0xF2:
			n = 3
		case s == 0xF6, s >= 0xF8 && s != 0xF9 && s != 0xFD:
			n = 1
		default:
			return fmt.Errorf("byte %d (0x%02X) is not a MIDI status byte that exists", i+1, s)
		}
		if i+n > len(data) {
			return fmt.Errorf("the message starting at byte %d (0x%02X) is missing data bytes", i+1, s)
		}
		for j := 1; j < n; j++ {
			if data[i+j] >= 0x80 {
				return fmt.Errorf("byte %d (0x%02X) should be a data byte (0-127) but starts a new message", i+j+1, data[i+j])
			}
		}
		i += n
	}
	return nil
}

// SplitSysEx cuts data into its SysEx messages (each F0 ... F7) and refuses
// anything else in it: a .syx file holds nothing but SysEx messages.
func SplitSysEx(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("the file is empty")
	}
	var out [][]byte
	for i := 0; i < len(data); {
		if data[i] != 0xF0 {
			return nil, fmt.Errorf("byte %d (0x%02X): a SysEx file must consist of messages starting with F0", i+1, data[i])
		}
		end := i + 1
		for end < len(data) && data[end] < 0x80 {
			end++
		}
		if end >= len(data) || data[end] != 0xF7 {
			return nil, fmt.Errorf("the message starting at byte %d does not end with F7 (the file may be cut off)", i+1)
		}
		out = append(out, data[i:end+1])
		i = end + 1
	}
	return out, nil
}
