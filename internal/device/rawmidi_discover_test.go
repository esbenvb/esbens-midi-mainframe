package device

import (
	"encoding/json"
	"errors"
	"testing"
	"unsafe"

	"emm/internal/router"
)

func TestIoctlNumbersMatchKernelHeaders(t *testing.T) {
	// Values of the macros in include/uapi/sound/asound.h on arm/arm64/x86:
	//   SNDRV_CTL_IOCTL_RAWMIDI_INFO             _IOWR('U', 0x41, struct snd_rawmidi_info)
	//   SNDRV_CTL_IOCTL_RAWMIDI_PREFER_SUBDEVICE _IOW ('U', 0x42, int)
	//   SNDRV_RAWMIDI_IOCTL_INFO                 _IOR ('W', 0x01, struct snd_rawmidi_info)
	if got := unsafe.Sizeof(sndRawmidiInfo{}); got != sndRawmidiInfoSize {
		t.Fatalf("sizeof(snd_rawmidi_info) = %d, want %d", got, sndRawmidiInfoSize)
	}
	cases := []struct {
		name      string
		got, want uint32
	}{
		{"CTL_RAWMIDI_INFO", sndCtlIoctlRawmidiInfo, 0xC10C5541},
		{"CTL_RAWMIDI_PREFER_SUBDEVICE", sndCtlIoctlRawmidiPreferSubdevice, 0x40045542},
		{"RAWMIDI_IOCTL_INFO", sndRawmidiIoctlInfo, 0x810C5701},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
	// cross-check against the runtime encoder
	if got := ioc(iocRead|iocWrite, 'U', 0x41, sndRawmidiInfoSize); got != sndCtlIoctlRawmidiInfo {
		t.Errorf("ioc() disagrees: %#x", got)
	}
	// field offsets that the kernel ABI fixes
	var i sndRawmidiInfo
	if off := unsafe.Offsetof(i.ID); off != 20 {
		t.Errorf("id offset %d", off)
	}
	if off := unsafe.Offsetof(i.Subname); off != 20+64+80 {
		t.Errorf("subname offset %d", off)
	}
	if off := unsafe.Offsetof(i.SubdevicesCount); off != 20+64+80+32 {
		t.Errorf("subdevices_count offset %d", off)
	}
}

func TestCString(t *testing.T) {
	if got := cString([]byte("Digitakt MIDI 1\x00garbage")); got != "Digitakt MIDI 1" {
		t.Fatal(got)
	}
	if got := cString([]byte("full")); got != "full" {
		t.Fatal(got)
	}
}

func discover(t *testing.T, s *rawFakeSys, acc rawAccess, includeNonUSB bool, taint map[string]bool) []FoundPort {
	t.Helper()
	o := s.opts()
	o.IncludeNonUSB = includeNonUSB
	got, err := rawDiscover(o, acc, rawDiscardLogger(), taint)
	rawMust(t, err)
	return got
}

func TestDiscoverSingleDevice(t *testing.T) {
	s := newRawFakeSys(t)
	dir := s.usb("1-1.2", rawUSBAttrs{vid: "1935", pid: "0014", manufacturer: "Elektron Music Machines",
		product: "Digitakt", serial: "A1B2C3D4", busnum: "1", devnum: "7"})
	s.midi(dir, 1, 0)

	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 port, got %+v", got)
	}
	p := got[0]
	want := FoundPort{
		ID: "usb:1935:0014:A1B2C3D4:0", Name: "Digitakt", Group: "Elektron Music Machines Digitakt",
		Kind: router.KindUSB, Detail: "USB 1-1.2 · 1935:0014 · hw:1,0,0", Input: true, Output: true,
		Card: 1, Device: 0, Sub: 0, Hw: "hw:1,0,0", Node: "/dev/snd/midiC1D0",
		VendorID: "1935", ProductID: "0014", Manufacturer: "Elektron Music Machines", Product: "Digitakt",
		Serial: "A1B2C3D4", USBPath: "1-1.2", Gen: "1/7", SerialUsed: true,
	}
	if p != want {
		t.Fatalf("got  %+v\nwant %+v", p, want)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverLowercasesHexIDs(t *testing.T) {
	s := newRawFakeSys(t)
	dir := s.usb("1-1", rawUSBAttrs{vid: "04B3", pid: "4B19", product: "Thing", serial: "XYZ99"})
	s.midi(dir, 0, 0)
	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 1 || got[0].ID != "usb:04b3:4b19:XYZ99:0" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverMultiCableDevice(t *testing.T) {
	s := newRawFakeSys(t)
	dir := s.usb("1-1.4", rawUSBAttrs{vid: "1235", pid: "0010", manufacturer: "Focusrite", product: "Interface 3x3", serial: "SERIAL77"})
	s.midi(dir, 2, 0)
	acc := newRawFakeAccess(t)
	acc.setSubs(2, 0,
		rawSub{Sub: 0, Name: "Interface 3x3 MIDI 1", Input: true, Output: true},
		rawSub{Sub: 1, Name: "Interface 3x3 MIDI 2", Input: true, Output: true},
		rawSub{Sub: 2, Name: "DIN OUT", Input: false, Output: true},
	)
	got := discover(t, s, acc, false, nil)
	if len(got) != 3 {
		t.Fatalf("want 3 ports, got %+v", got)
	}
	ids := []string{"usb:1235:0010:SERIAL77:0", "usb:1235:0010:SERIAL77:1", "usb:1235:0010:SERIAL77:2"}
	names := []string{"Interface 3x3 MIDI 1", "Interface 3x3 MIDI 2", "Interface 3x3 DIN OUT"}
	for i, p := range got {
		if p.ID != ids[i] || p.Name != names[i] || p.Sub != i || p.Hw != rawHwName(2, 0, i) {
			t.Errorf("port %d: %+v", i, p)
		}
		if p.Group != "Focusrite Interface 3x3" {
			t.Errorf("group %q", p.Group)
		}
	}
	if !got[2].Output || got[2].Input {
		t.Fatalf("directions of port 2: in=%v out=%v", got[2].Input, got[2].Output)
	}
	if !got[0].Input || !got[0].Output {
		t.Fatal("port 0 should be bidirectional")
	}
}

func TestDiscoverUnnamedSubdevicesGetNumbered(t *testing.T) {
	s := newRawFakeSys(t)
	dir := s.usb("1-2", rawUSBAttrs{vid: "aaaa", pid: "bbbb", product: "Box", serial: "S-1234"})
	s.midi(dir, 3, 0)
	acc := newRawFakeAccess(t)
	acc.setSubs(3, 0, rawSub{Sub: 0, Input: true, Output: true}, rawSub{Sub: 1, Input: true, Output: true})
	got := discover(t, s, acc, false, nil)
	if len(got) != 2 || got[0].Name != "Box 1" || got[1].Name != "Box 2" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverSecondRawMIDIDeviceOnSameCard(t *testing.T) {
	s := newRawFakeSys(t)
	dir := s.usb("1-3", rawUSBAttrs{vid: "cccc", pid: "dddd", product: "Two", serial: "S-9999"})
	s.midi(dir, 4, 0)
	s.midi(dir, 4, 1)
	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 2 || got[0].ID != "usb:cccc:dddd:S-9999:0" || got[1].ID != "usb:cccc:dddd:S-9999:1-0" {
		t.Fatalf("%+v", got)
	}
	// Both ports belong to one USB device, so they are named as a multi-port device.
	if got[0].Name != "Two 1" || got[1].Name != "Two 2" {
		t.Fatalf("names %q %q", got[0].Name, got[1].Name)
	}
}

func TestDiscoverIdenticalDevicesWithIdenticalSerials(t *testing.T) {
	s := newRawFakeSys(t)
	same := rawUSBAttrs{vid: "1234", pid: "abcd", manufacturer: "Acme", product: "Synth", serial: "SN0001"}
	a := same
	a.devnum = "5"
	b := same
	b.devnum = "6"
	s.midi(s.usb("1-1.2", a), 1, 0)
	s.midi(s.usb("1-1.3", b), 2, 0)

	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("IDs collide: %q", got[0].ID)
	}
	if got[0].ID != "usb:1234:abcd:1-1.2:0" || got[1].ID != "usb:1234:abcd:1-1.3:0" {
		t.Fatalf("expected port-path IDs, got %q and %q", got[0].ID, got[1].ID)
	}
	if got[0].SerialUsed || got[1].SerialUsed {
		t.Fatal("serial must not be used when it is shared")
	}
}

func TestDiscoverIdenticalDevicesWithDistinctSerials(t *testing.T) {
	s := newRawFakeSys(t)
	s.midi(s.usb("1-1.2", rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "SN0001"}), 1, 0)
	s.midi(s.usb("1-1.3", rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "SN0002"}), 2, 0)
	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 2 || got[0].ID != "usb:1234:abcd:SN0001:0" || got[1].ID != "usb:1234:abcd:SN0002:0" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverSameSerialOnDifferentModelsIsFine(t *testing.T) {
	s := newRawFakeSys(t)
	s.midi(s.usb("1-1.2", rawUSBAttrs{vid: "1111", pid: "0001", product: "A", serial: "ABCD1234"}), 1, 0)
	s.midi(s.usb("1-1.3", rawUSBAttrs{vid: "2222", pid: "0002", product: "B", serial: "ABCD1234"}), 2, 0)
	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 2 || !got[0].SerialUsed || !got[1].SerialUsed {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverDuplicateSerialStaysTaintedAfterOneUnplug(t *testing.T) {
	s := newRawFakeSys(t)
	same := rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "SN0001"}
	d1 := s.usb("1-1.2", same)
	d2 := s.usb("1-1.3", same)
	s.midi(d1, 1, 0)
	s.midi(d2, 2, 0)

	taint := map[string]bool{}
	acc := newRawFakeAccess(t)
	first := discover(t, s, acc, false, taint)
	if len(first) != 2 {
		t.Fatalf("%+v", first)
	}
	s.unplug(d2, 2, 0)
	second := discover(t, s, acc, false, taint)
	if len(second) != 1 || second[0].ID != first[0].ID || second[0].SerialUsed {
		t.Fatalf("remaining device changed identity: before %q, after %+v", first[0].ID, second)
	}
	// Without the memory the remaining device would flip to a serial-based ID.
	fresh := discover(t, s, acc, false, nil)
	if len(fresh) != 1 || !fresh[0].SerialUsed {
		t.Fatalf("control case failed: %+v", fresh)
	}
}

func TestDiscoverBogusSerialsFallBackToPath(t *testing.T) {
	for _, serial := range []string{"", "0", "00000000", "FFFFFFFF", "123456789ABC", "0123456789ABCDEF", "Default string", "x"} {
		s := newRawFakeSys(t)
		s.midi(s.usb("1-1.5", rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: serial}), 1, 0)
		got := discover(t, s, newRawFakeAccess(t), false, nil)
		if len(got) != 1 || got[0].ID != "usb:1234:abcd:1-1.5:0" || got[0].SerialUsed {
			t.Errorf("serial %q: %+v", serial, got)
		}
	}
}

func TestDiscoverSanitisesIDs(t *testing.T) {
	s := newRawFakeSys(t)
	s.midi(s.usb("1-1.5", rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "AB 12/34:56"}), 1, 0)
	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 1 || got[0].ID != "usb:1234:abcd:AB_12_34_56:0" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverNonUSB(t *testing.T) {
	s := newRawFakeSys(t)
	s.nonUSB(2, 0, "VirMIDI")
	s.midi(s.usb("1-1.2", rawUSBAttrs{vid: "1234", pid: "abcd", product: "Synth", serial: "SN0001"}), 1, 0)

	got := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(got) != 1 || got[0].Kind != router.KindUSB {
		t.Fatalf("non-USB card must be skipped by default: %+v", got)
	}
	got = discover(t, s, newRawFakeAccess(t), true, nil)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	var virt FoundPort
	for _, p := range got {
		if p.Kind == router.KindVirtual {
			virt = p
		}
	}
	if virt.ID != "alsa:VirMIDI:0" || virt.Name != "VirMIDI" || virt.Detail != "hw:2,0,0" || virt.USBPath != "" {
		t.Fatalf("%+v", virt)
	}
}

func TestDiscoverUnpluggedBetweenPolls(t *testing.T) {
	s := newRawFakeSys(t)
	d1 := s.usb("1-1.2", rawUSBAttrs{vid: "1234", pid: "abcd", product: "A", serial: "SN-A1"})
	d2 := s.usb("1-1.3", rawUSBAttrs{vid: "1234", pid: "abcd", product: "B", serial: "SN-B2"})
	s.midi(d1, 1, 0)
	s.midi(d2, 2, 0)
	acc := newRawFakeAccess(t)
	if got := discover(t, s, acc, false, nil); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	s.unplug(d1, 1, 0)
	got := discover(t, s, acc, false, nil)
	if len(got) != 1 || got[0].Name != "B" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverReplugChangesGeneration(t *testing.T) {
	s := newRawFakeSys(t)
	attrs := rawUSBAttrs{vid: "1234", pid: "abcd", product: "A", serial: "SN-A1", devnum: "4"}
	d := s.usb("1-1.2", attrs)
	s.midi(d, 1, 0)
	first := discover(t, s, newRawFakeAccess(t), false, nil)
	s.unplug(d, 1, 0)
	attrs.devnum = "9"
	d = s.usb("1-1.2", attrs)
	s.midi(d, 1, 0)
	second := discover(t, s, newRawFakeAccess(t), false, nil)
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("identity must survive a replug: %+v %+v", first, second)
	}
	if rawKey(first[0]) == rawKey(second[0]) {
		t.Fatal("a replug must change the tracking key")
	}
}

func TestDiscoverProbeFailureDegradesToOnePort(t *testing.T) {
	s := newRawFakeSys(t)
	s.midi(s.usb("1-1.2", rawUSBAttrs{vid: "1234", pid: "abcd", product: "A", serial: "SN-A1"}), 1, 0)
	acc := newRawFakeAccess(t)
	acc.probeErr[[2]int{1, 0}] = errors.New("permission denied")
	got := discover(t, s, acc, false, nil)
	if len(got) != 1 || !got[0].Input || !got[0].Output || got[0].Name != "A" {
		t.Fatalf("%+v", got)
	}
}

func TestDiscoverMissingSysfsIsNotAnError(t *testing.T) {
	o := RawMIDIOptions{SysRoot: t.TempDir()}.withDefaults()
	got, err := rawDiscover(o, newRawFakeAccess(t), rawDiscardLogger(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDiscoverIgnoresOtherClassEntries(t *testing.T) {
	s := newRawFakeSys(t)
	for _, n := range []string{"card0", "controlC0", "pcmC0D0p", "timer", "seq", "midiXD0", "midiC1", "midiCxD0"} {
		rawMust(t, rawMkdirAll(s.root, "class", "sound", n))
	}
	if got := discover(t, s, newRawFakeAccess(t), true, nil); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestListRawMIDI(t *testing.T) {
	s := newRawFakeSys(t)
	s.midi(s.usb("1-1.2", rawUSBAttrs{vid: "1234", pid: "abcd", product: "A", serial: "SN-A1"}), 1, 0)
	o := s.opts()
	got, err := ListRawMIDI(o)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back []FoundPort
	if err := json.Unmarshal(b, &back); err != nil || back[0].ID != got[0].ID {
		t.Fatalf("json round trip: %v %s", err, b)
	}
}

func TestUsableSerial(t *testing.T) {
	good := []string{"A1B2C3D4", "SN0001", "0000-1234", "ABC123", "20230415X"}
	bad := []string{"", " ", "0", "00", "ffff", "123456789ABC", "0123456789abcdef", "123456", "abcdef",
		"None", "Serial", "Default string", "N/A", "unknown", "1111111111"}
	for _, s := range good {
		if !usableSerial(s) {
			t.Errorf("%q should be usable", s)
		}
	}
	for _, s := range bad {
		if usableSerial(s) {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"abc-1.2_x": "abc-1.2_x",
		"a b":       "a_b",
		"a:b/c":     "a_b_c",
		"é":         "_",
		"":          "_",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupAndPortNames(t *testing.T) {
	if g := groupName("Elektron", "Elektron Digitakt"); g != "Elektron Digitakt" {
		t.Error(g)
	}
	if g := groupName("Korg", "volca sample"); g != "Korg volca sample" {
		t.Error(g)
	}
	if g := groupName("", " X "); g != "X" {
		t.Error(g)
	}
	if g := groupName("M", ""); g != "M" {
		t.Error(g)
	}
	if n := portName("Box", "Box MIDI 2", 1, true); n != "Box MIDI 2" {
		t.Error(n)
	}
	if n := portName("Box", "", 1, true); n != "Box 2" {
		t.Error(n)
	}
	if n := portName("Box", "Port B", 1, true); n != "Box Port B" {
		t.Error(n)
	}
	if n := portName("Box", "anything", 0, false); n != "Box" {
		t.Error(n)
	}
}

func TestParseMidiName(t *testing.T) {
	for name, want := range map[string][2]int{"midiC1D0": {1, 0}, "midiC12D3": {12, 3}} {
		c, d, ok := parseMidiName(name)
		if !ok || c != want[0] || d != want[1] {
			t.Errorf("%s: %d %d %v", name, c, d, ok)
		}
	}
	for _, name := range []string{"midi", "midiC", "midiC1", "midiC1D", "midiCxD0", "midiC1Dx", "midiC-1D0", "card1"} {
		if _, _, ok := parseMidiName(name); ok {
			t.Errorf("%q must not parse", name)
		}
	}
}
