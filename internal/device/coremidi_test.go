package device

import (
	"regexp"
	"testing"

	"emm/internal/router"
)

var cmIDPattern = regexp.MustCompile(`^[A-Za-z0-9:._-]+$`)

func ep(ref uint32, uid int32, name string) cmEP { return cmEP{Ref: ref, UID: uid, Name: name} }

func specByName(specs []cmPortSpec, name string) (cmPortSpec, bool) {
	for _, s := range specs {
		if s.name == name {
			return s, true
		}
	}
	return cmPortSpec{}, false
}

func TestCoreMIDIBuildSpecsPairsEntityEndpoints(t *testing.T) {
	g := cmGraph{
		Devices: []cmDevice{{
			Name: "Digitakt", Manufacturer: "Elektron", Model: "Digitakt", Driver: "com.apple.AppleMIDIUSBDriver",
			Entities: []cmEntity{{
				Name:    "MIDI 1",
				Sources: []cmEP{ep(10, 0x1234, "Digitakt")},
				Dests:   []cmEP{ep(11, 0x1235, "Digitakt")},
			}},
		}},
		Sources: []cmEP{ep(10, 0x1234, "Digitakt")},
		Dests:   []cmEP{ep(11, 0x1235, "Digitakt")},
	}
	specs := buildSpecs(g, false)
	if len(specs) != 1 {
		t.Fatalf("want 1 port, got %d: %+v", len(specs), specs)
	}
	s := specs[0]
	if s.id != "usb:cm-00001234" || s.kind != router.KindUSB || s.src.Ref != 10 || s.dst.Ref != 11 {
		t.Fatalf("unexpected spec %+v", s)
	}
	if s.name != "Digitakt" || s.group != "Digitakt" {
		t.Fatalf("name/group %q/%q", s.name, s.group)
	}
	if s.detail != "CoreMIDI · Elektron Digitakt" {
		t.Fatalf("detail %q", s.detail)
	}
	info := s.info()
	if !info.Input || !info.Output {
		t.Fatal("expected a bidirectional port")
	}
	// Endpoints of a device entity must not resurface as virtual ports.
	if got := buildSpecs(g, true); len(got) != 1 {
		t.Fatalf("with virtual enabled: want 1 port, got %d: %+v", len(got), got)
	}
}

func TestCoreMIDIBuildSpecsMultiPortDevice(t *testing.T) {
	g := cmGraph{Devices: []cmDevice{{
		Name: "Interface 4x4",
		Entities: []cmEntity{
			{Name: "Port 1", Sources: []cmEP{ep(1, 101, "Interface Port 1")}, Dests: []cmEP{ep(2, 102, "Interface Port 1")}},
			{Name: "Port 2", Sources: []cmEP{ep(3, 103, "Interface Port 2")}, Dests: []cmEP{ep(4, 104, "Interface Port 2")}},
			// an output-only jack
			{Name: "Out", Dests: []cmEP{ep(5, 105, "Interface Out")}},
		},
	}}}
	specs := buildSpecs(g, false)
	if len(specs) != 3 {
		t.Fatalf("want 3 ports, got %+v", specs)
	}
	last, ok := specByName(specs, "Interface Out")
	if !ok || last.src.Ref != 0 || last.dst.Ref != 5 {
		t.Fatalf("output-only port wrong: %+v", last)
	}
	if last.info().Input || !last.info().Output {
		t.Fatal("output-only port must not be an input")
	}
	if last.id != "usb:cm-00000069" { // uid of the destination when there is no source
		t.Fatalf("id %q", last.id)
	}
	for _, s := range specs {
		if s.group != "Interface 4x4" {
			t.Fatalf("group %q", s.group)
		}
	}
}

func TestCoreMIDIBuildSpecsSoftwareDevicesNeedIncludeVirtual(t *testing.T) {
	g := cmGraph{
		Devices: []cmDevice{
			{Name: "IAC Driver", Driver: "com.apple.AppleMIDIIACDriver",
				Entities: []cmEntity{{Name: "Bus 1", Sources: []cmEP{ep(20, 200, "IAC Driver Bus 1")}, Dests: []cmEP{ep(21, 201, "IAC Driver Bus 1")}}}},
			{Name: "Network", Driver: "com.apple.AppleMIDIRTPDriver",
				Entities: []cmEntity{{Name: "Session 1", Sources: []cmEP{ep(22, 202, "Network Session 1")}, Dests: []cmEP{ep(23, 203, "Network Session 1")}}}},
			{Name: "Synth", Driver: "com.apple.AppleMIDIUSBDriver",
				Entities: []cmEntity{{Name: "MIDI", Sources: []cmEP{ep(24, 204, "Synth")}, Dests: []cmEP{ep(25, 205, "Synth")}}}},
		},
		Sources: []cmEP{ep(20, 200, "IAC Driver Bus 1"), ep(22, 202, "Network Session 1"), ep(24, 204, "Synth")},
		Dests:   []cmEP{ep(21, 201, "IAC Driver Bus 1"), ep(23, 203, "Network Session 1"), ep(25, 205, "Synth")},
	}
	off := buildSpecs(g, false)
	if len(off) != 1 || off[0].name != "Synth" {
		t.Fatalf("without IncludeVirtual only the synth should appear: %+v", off)
	}
	on := buildSpecs(g, true)
	if len(on) != 3 {
		t.Fatalf("with IncludeVirtual want 3 ports: %+v", on)
	}
	iac, ok := specByName(on, "IAC Driver Bus 1")
	if !ok || iac.kind != router.KindVirtual || iac.id != "virtual:cm-000000c8" {
		t.Fatalf("IAC should be virtual with a virtual: id: %+v", iac)
	}
	syn, _ := specByName(on, "Synth")
	if syn.kind != router.KindUSB {
		t.Fatalf("synth should stay usb: %+v", syn)
	}
}

func TestCoreMIDIBuildSpecsVirtualPairingByName(t *testing.T) {
	g := cmGraph{
		Sources: []cmEP{ep(30, 1, "App A"), ep(31, 2, "Lonely source"), ep(32, 3, "App A")},
		Dests:   []cmEP{ep(40, 4, "App A"), ep(41, 5, "Lonely dest"), ep(42, 6, "App A")},
	}
	if got := buildSpecs(g, false); len(got) != 0 {
		t.Fatalf("virtual endpoints must be hidden by default, got %+v", got)
	}
	specs := buildSpecs(g, true)
	var appA []cmPortSpec
	for _, s := range specs {
		if s.name == "App A" {
			appA = append(appA, s)
		}
	}
	if len(appA) != 2 {
		t.Fatalf("two same-named pairs expected, got %+v", specs)
	}
	if appA[0].src.Ref != 30 || appA[0].dst.Ref != 40 || appA[1].src.Ref != 32 || appA[1].dst.Ref != 42 {
		t.Fatalf("pairing wrong: %+v", appA)
	}
	if appA[0].id == appA[1].id {
		t.Fatalf("duplicate names must get distinct ids: %q", appA[0].id)
	}
	src, _ := specByName(specs, "Lonely source")
	dst, _ := specByName(specs, "Lonely dest")
	if src.dst.Ref != 0 || src.src.Ref != 31 || dst.src.Ref != 0 || dst.dst.Ref != 41 {
		t.Fatalf("unpaired endpoints wrong: %+v / %+v", src, dst)
	}
	if !src.info().Input || src.info().Output || dst.info().Input || !dst.info().Output {
		t.Fatal("direction flags wrong for unpaired virtual endpoints")
	}
	for _, s := range specs {
		if s.kind != router.KindVirtual {
			t.Fatalf("%q should be virtual", s.name)
		}
	}
}

func TestCoreMIDIVirtualIDsAreStableAcrossRelaunch(t *testing.T) {
	// The CoreMIDI unique ID of an app's virtual port changes every launch; the
	// router ID must not, or saved connections would never come back.
	a := buildSpecs(cmGraph{Sources: []cmEP{ep(1, 111, "Some App")}}, true)
	b := buildSpecs(cmGraph{Sources: []cmEP{ep(9, 999, "Some App")}}, true)
	if len(a) != 1 || len(b) != 1 || a[0].id != b[0].id {
		t.Fatalf("ids differ: %+v vs %+v", a, b)
	}
}

func TestCoreMIDIEntityIDsUseUniqueID(t *testing.T) {
	// Same device after a replug: endpoint refs change, unique ID does not.
	mk := func(ref uint32) cmGraph {
		return cmGraph{Devices: []cmDevice{{Name: "Synth", Driver: "x",
			Entities: []cmEntity{{Sources: []cmEP{ep(ref, 0x7fff0001, "Synth")}, Dests: []cmEP{ep(ref+1, 0x7fff0002, "Synth")}}}}}}
	}
	a, b := buildSpecs(mk(10), false), buildSpecs(mk(50), false)
	if a[0].id != b[0].id {
		t.Fatalf("id must not depend on endpoint refs: %q vs %q", a[0].id, b[0].id)
	}
	if a[0].same(b[0]) {
		t.Fatal("different endpoint refs must make specs differ, so the port is re-created")
	}
	if !a[0].same(a[0]) {
		t.Fatal("same must be reflexive")
	}
	// Negative unique IDs (CoreMIDI uses SInt32) still give a valid, positive-looking ID.
	n := buildSpecs(cmGraph{Devices: []cmDevice{{Name: "N", Entities: []cmEntity{{Sources: []cmEP{ep(1, -5, "N")}}}}}}, false)
	if n[0].id != "usb:cm-fffffffb" {
		t.Fatalf("negative uid id %q", n[0].id)
	}
}

func TestCoreMIDIIDsAreRouterSafe(t *testing.T) {
	g := cmGraph{
		Devices: []cmDevice{{Name: "Weird / Name ✓", Entities: []cmEntity{{Sources: []cmEP{ep(1, 1, "Weird / Name ✓")}}}}},
		Sources: []cmEP{ep(2, 2, "Näme with spaces/and:slashes")},
	}
	for _, s := range buildSpecs(g, true) {
		if !cmIDPattern.MatchString(s.id) {
			t.Fatalf("id %q has characters the router forbids", s.id)
		}
	}
	if got := cmSanitizeID("a b/c✓:d.e_f-g"); got != "a-b-c-:d.e_f-g" {
		t.Fatalf("sanitize: %q", got)
	}
}

func TestCoreMIDINameFallbacks(t *testing.T) {
	g := cmGraph{Devices: []cmDevice{{
		Name: "Dev", Entities: []cmEntity{{Name: "Ent", Sources: []cmEP{ep(1, 1, "")}}},
	}}}
	s := buildSpecs(g, false)
	if len(s) != 1 || s[0].name != "Ent" {
		t.Fatalf("name should fall back to the entity name: %+v", s)
	}
	g.Devices[0].Entities[0].Name = ""
	s = buildSpecs(g, false)
	if s[0].name != "Dev" {
		t.Fatalf("name should fall back to the device name: %+v", s)
	}
}

func TestCoreMIDIIsVirtualDriver(t *testing.T) {
	for drv, want := range map[string]bool{
		"com.apple.AppleMIDIIACDriver": true,
		"com.apple.AppleMIDIRTPDriver": true,
		"com.apple.AppleMIDIUSBDriver": false,
		"":                             false,
		"com.vendor.SomeDriver":        false,
	} {
		if got := cmIsVirtualDriver(drv); got != want {
			t.Errorf("%q: got %v want %v", drv, got, want)
		}
	}
}

func TestCoreMIDIEmptyGraph(t *testing.T) {
	if got := buildSpecs(cmGraph{}, true); len(got) != 0 {
		t.Fatalf("empty graph produced %+v", got)
	}
}
