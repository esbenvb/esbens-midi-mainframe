package device

// ListRawMIDI is a diagnostic: it runs discovery once and returns every raw
// MIDI port it would expose, without opening anything. The daemon prints it as
// JSON for `emmd --list-devices`.
//
// On Linux the control nodes are queried for subdevices and directions; where
// that is impossible (not Linux, no permission) each device is reported as a
// single bidirectional port.
func ListRawMIDI(o RawMIDIOptions) ([]FoundPort, error) {
	o = o.withDefaults()
	return rawDiscover(o, defaultAccess(o), o.Logger, nil)
}
