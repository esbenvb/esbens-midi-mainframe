package app

import (
	"strings"
	"testing"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

const (
	hydraA = "usb:0a67:5021:1-1.2.3.4.2:0"
	hydraB = "usb:0a67:5021:1-1.1.2:0"
	hydraC = "usb:0a67:5021:1-1.1.3.4.4:0"
)

func TestParsePathUSBID(t *testing.T) {
	for _, c := range []struct {
		id string
		ok bool
	}{
		{"usb:0a67:5021:1-1.2.3.4.2:0", true},
		{"usb:0a67:5021:1-1:1-2", true},
		{"usb:0a67:5021:SN12345:0", false}, // serial number
		{"usb:0a67:5021:1.1:0", false},     // not a kernel path
		{"usb:cm-1234", false},             // CoreMIDI
		{"alsa:card0:0", false},
		{"rtp:usb:0a67:5021:1-1:0", false}, // the endpoint, not the port
	} {
		if _, ok := parsePathUSBID(c.id); ok != c.ok {
			t.Errorf("parsePathUSBID(%q) ok=%v, want %v", c.id, ok, c.ok)
		}
	}
}

// plugged simulates a synth being plugged in and seen by the reconciler.
func (h *harness) plugged(id, name string) {
	h.r.AddPort(usbPort(id, name, true, true))
	h.rec.Reconcile()
}

func (h *harness) unplugged(id string) {
	h.r.RemovePort(id)
	h.rec.Reconcile()
}

func TestMovedDeviceKeepsItsRoutingAndSettings(t *testing.T) {
	h := newHarness(t)
	h.plugged("usb:aaaa:0001:1-1.1:0", "Keys")
	h.plugged(hydraB, "Hydrasynth Deluxe")
	f := router.Filter{Block: []string{"clock"}, Transpose: 5}
	if err := h.r.Connect("usb:aaaa:0001:1-1.1:0", hydraB, f); err != nil {
		t.Fatal(err)
	}
	h.st.Update(func(st *config.State) {
		ps := st.Ports[hydraB]
		ps.Alias, ps.Hidden = "Hydra", true
		st.Ports[hydraB] = ps
	})
	oldPort := h.st.Snapshot().Ports[hydraB].RTPPort
	if oldPort == 0 {
		t.Fatal("the device should have been given a network port")
	}

	h.unplugged(hydraB)
	h.plugged(hydraA, "Hydrasynth Deluxe") // same synth, another hub port

	conns := h.conns()
	if !conns["usb:aaaa:0001:1-1.1:0 > "+hydraA] {
		t.Errorf("the keyboard route did not follow the synth: %v", conns)
	}
	if !conns[hydraA+" > rtp:"+hydraA] || !conns["rtp:"+hydraA+" > "+hydraA] {
		t.Errorf("the network pair did not follow the synth: %v", conns)
	}
	for c := range conns {
		if strings.Contains(c, hydraB) {
			t.Errorf("a connection to the old ID is left: %s", c)
		}
	}
	for _, c := range h.r.Connections() {
		if c.From == "usb:aaaa:0001:1-1.1:0" && c.To == hydraA {
			if len(c.Filter.Block) != 1 || c.Filter.Block[0] != "clock" || c.Filter.Transpose != 5 {
				t.Errorf("filter was not kept: %+v", c.Filter)
			}
		}
	}

	st := h.st.Snapshot()
	if st.Ports[hydraA].Alias != "Hydra" || !st.Ports[hydraA].Hidden {
		t.Errorf("name and hidden flag did not move: %+v", st.Ports[hydraA])
	}
	if st.Ports[hydraA].RTPPort == 0 {
		t.Errorf("the network port did not move: %+v", st.Ports[hydraA])
	}
	if _, ok := st.Ports[hydraB]; ok {
		t.Error("settings of the old ID are still there")
	}
	if _, ok := st.Known[hydraB]; ok {
		t.Error("the old ID is still remembered")
	}
}

func TestMovingTwiceLeavesNoDuplicates(t *testing.T) {
	h := newHarness(t)
	h.plugged(hydraB, "Hydrasynth Deluxe")
	h.unplugged(hydraB)
	h.plugged(hydraC, "Hydrasynth Deluxe")
	h.unplugged(hydraC)
	h.plugged(hydraA, "Hydrasynth Deluxe")

	n := 0
	for id := range h.st.Snapshot().Known {
		if strings.HasPrefix(id, "usb:0a67:5021:") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the synth is remembered %d times, want 1: %v", n, h.st.Snapshot().Known)
	}
	for c := range h.conns() {
		if strings.Contains(c, "1-1.1.") {
			t.Errorf("stale connection %s", c)
		}
	}
}

func TestTwoIdenticalDevicesAreNeverMerged(t *testing.T) {
	h := newHarness(t)
	h.plugged(hydraB, "Hydrasynth Deluxe")
	h.plugged(hydraC, "Hydrasynth Deluxe")
	h.unplugged(hydraB)
	h.plugged(hydraA, "Hydrasynth Deluxe") // C is still attached: A's identity is unknown

	if !h.conns()[hydraB+" > rtp:"+hydraB] {
		t.Error("the absent unit's connections were taken over although two identical units are attached")
	}
	if _, ok := h.st.Snapshot().Known[hydraB]; !ok {
		t.Error("the absent unit was forgotten")
	}
}

func TestSeveralAbsentCandidatesAreLeftAlone(t *testing.T) {
	// What an earlier version leaves behind: one synth that moved around, remembered
	// once per hub port. Nothing says which of them the attached synth is.
	h := newHarness(t)
	h.st.Update(func(st *config.State) {
		for _, id := range []string{hydraB, hydraC} {
			st.Known[id] = config.KnownPort{Name: "Hydrasynth Deluxe", Kind: router.KindUSB, Input: true, Output: true, LastSeen: time.Now().Unix()}
		}
	})
	for _, id := range []string{hydraB, hydraC} {
		if err := h.r.Connect(id, "rtp:"+id, router.DefaultFilter()); err != nil {
			t.Fatal(err)
		}
	}
	h.plugged(hydraA, "Hydrasynth Deluxe")

	st := h.st.Snapshot()
	for _, id := range []string{hydraA, hydraB, hydraC} {
		if _, ok := st.Known[id]; !ok {
			t.Errorf("%s was dropped although it is ambiguous which one the live device is", id)
		}
	}
	if !h.conns()[hydraB+" > rtp:"+hydraB] || !h.conns()[hydraC+" > rtp:"+hydraC] {
		t.Errorf("connections of an ambiguous candidate were moved: %v", h.conns())
	}
}

func TestDevicesWithADifferentCableLayoutAreNotMerged(t *testing.T) {
	h := newHarness(t)
	h.plugged(hydraB, "Hydrasynth Deluxe")
	h.unplugged(hydraB)
	h.r.AddPort(usbPort("usb:0a67:5021:1-1.2:0", "Hydrasynth Deluxe", true, true))
	h.r.AddPort(usbPort("usb:0a67:5021:1-1.2:1", "Hydrasynth Deluxe 2", true, true))
	h.rec.Reconcile()
	if _, ok := h.st.Snapshot().Known[hydraB]; !ok {
		t.Error("a unit with one cable was merged into a device with two")
	}
}

func TestSerialNumberDevicesAndOtherModelsAreUntouched(t *testing.T) {
	h := newHarness(t)
	h.plugged("usb:0a67:5021:SN123456:0", "Hydrasynth Deluxe")
	h.unplugged("usb:0a67:5021:SN123456:0")
	h.plugged(hydraA, "Hydrasynth Deluxe")
	if _, ok := h.st.Snapshot().Known["usb:0a67:5021:SN123456:0"]; !ok {
		t.Error("a device identified by its serial number was merged")
	}

	h2 := newHarness(t)
	h2.plugged("usb:1111:0001:1-1.1:0", "Synth One")
	h2.unplugged("usb:1111:0001:1-1.1:0")
	h2.plugged("usb:2222:0002:1-1.2:0", "Synth Two")
	if _, ok := h2.st.Snapshot().Known["usb:1111:0001:1-1.1:0"]; !ok {
		t.Error("a different model was merged")
	}
}

func TestAdoptionIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.plugged(hydraB, "Hydrasynth Deluxe")
	h.unplugged(hydraB)
	h.plugged(hydraA, "Hydrasynth Deluxe")
	before := len(h.r.Connections())
	for i := 0; i < 3; i++ {
		h.rec.Reconcile()
	}
	if after := len(h.r.Connections()); after != before {
		t.Errorf("connections changed from %d to %d on later passes", before, after)
	}
}
