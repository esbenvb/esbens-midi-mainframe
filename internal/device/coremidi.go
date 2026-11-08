package device

// Portable half of the macOS CoreMIDI backend: option/diagnostic types and the
// pure logic that turns CoreMIDI's object graph (devices → entities →
// endpoints, plus free-standing virtual endpoints) into router ports. Keeping
// this free of cgo lets it be unit-tested on any platform; the darwin-only
// file (coremidi_darwin.go) fills a cmGraph from the real CoreMIDI server.
//
// The backend exists so the whole daemon can be developed on a Mac against
// real USB synths. On the Pi, internal/device's rawmidi backend is used.
//
// Port IDs
//
//   - Hardware device entities:  usb:cm-<uniqueID>        (8 hex digits)
//     The CoreMIDI kMIDIPropertyUniqueID of the entity's source endpoint (or
//     of its destination endpoint when it has no source). The MIDI server
//     persists unique IDs per device, so they survive replugging and reboots.
//   - Software devices (IAC bus, network sessions): virtual:cm-<uniqueID>
//     Same derivation, but the prefix matches their router kind.
//   - Apps' virtual ports:       virtual:cm-<fnv32(name)> (8 hex digits)
//     These get a fresh unique ID on every launch, so the ID is derived from
//     the port name instead; identical names get a -2, -3, … suffix.
//
// Both match the router's [A-Za-z0-9:._-]+ rule.

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"

	"emm/internal/router"
)

// CoreMIDIOptions configures the macOS CoreMIDI backend.
type CoreMIDIOptions struct {
	Logger *slog.Logger
	// IncludeVirtual also exposes the IAC bus, network MIDI sessions and other
	// applications' virtual ports. Off by default: only real devices appear.
	IncludeVirtual bool
}

// CoreMIDIEndpointInfo describes one port as the CoreMIDI backend would
// present it to the router. It is what ListCoreMIDI returns (diagnostics).
type CoreMIDIEndpointInfo struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Group  string          `json:"group,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Kind   router.PortKind `json:"kind"`
	Input  bool            `json:"input"`
	Output bool            `json:"output"`
}

// cmEP is one CoreMIDI endpoint (a source or a destination).
type cmEP struct {
	Ref  uint32 // MIDIEndpointRef; 0 = none
	UID  int32  // kMIDIPropertyUniqueID
	Name string // kMIDIPropertyDisplayName
}

type cmEntity struct {
	Name           string
	Sources, Dests []cmEP
}

type cmDevice struct {
	Name, Manufacturer, Model string
	Driver                    string // kMIDIPropertyDriverOwner, may be empty
	Entities                  []cmEntity
}

// cmGraph is a snapshot of the CoreMIDI object graph. The darwin reader has
// already dropped offline and private objects. Sources/Dests are the flat
// lists of every endpoint in the system, including ones that belong to a
// device entity.
type cmGraph struct {
	Devices        []cmDevice
	Sources, Dests []cmEP
}

// cmPortSpec is one router port the backend wants to exist.
type cmPortSpec struct {
	id, name, group, detail string
	kind                    router.PortKind
	src, dst                cmEP // Ref 0 = this direction does not exist
}

// same reports whether two specs describe an identical port (so an existing
// router port can be kept as it is).
func (s cmPortSpec) same(o cmPortSpec) bool {
	return s.id == o.id && s.name == o.name && s.group == o.group && s.kind == o.kind &&
		s.src.Ref == o.src.Ref && s.dst.Ref == o.dst.Ref
}

func (s cmPortSpec) info() CoreMIDIEndpointInfo {
	return CoreMIDIEndpointInfo{
		ID: s.id, Name: s.name, Group: s.group, Detail: s.detail, Kind: s.kind,
		Input: s.src.Ref != 0, Output: s.dst.Ref != 0,
	}
}

// cmIsVirtualDriver reports whether a device's driver owner marks it as
// software rather than hardware: the IAC bus and Apple's network (RTP) MIDI.
func cmIsVirtualDriver(driver string) bool {
	d := strings.ToLower(driver)
	return strings.Contains(d, "iac") || strings.Contains(d, "rtp") || strings.Contains(d, "network")
}

// cmSanitizeID maps anything outside [A-Za-z0-9:._-] to '-'.
func cmSanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ':', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func cmNameHash(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()
}

func cmFirstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// buildSpecs turns a CoreMIDI graph into the list of router ports to expose.
//
// Each device entity's k-th source and k-th destination are paired into one
// port, the way a synth with one MIDI in/out pair looks on Linux. Endpoints
// that belong to no entity are virtual: a source and a destination with the
// same name (what apps normally create) are paired into one port, the rest
// become input-only / output-only ports. Virtual ports and software devices
// (IAC, network sessions) are only included when includeVirtual is set.
func buildSpecs(g cmGraph, includeVirtual bool) []cmPortSpec {
	var specs []cmPortSpec
	seen := map[uint32]bool{}
	ids := map[string]int{}

	uniq := func(id string) string {
		ids[id]++
		if n := ids[id]; n > 1 {
			return fmt.Sprintf("%s-%d", id, n)
		}
		return id
	}

	for _, d := range g.Devices {
		kind, idPrefix := router.KindUSB, "usb"
		if cmIsVirtualDriver(d.Driver) {
			kind, idPrefix = router.KindVirtual, "virtual"
		}
		devLabel := cmFirstNonEmpty(d.Name, d.Model, d.Manufacturer, "MIDI device")
		for _, e := range d.Entities {
			n := len(e.Sources)
			if len(e.Dests) > n {
				n = len(e.Dests)
			}
			for k := 0; k < n; k++ {
				var src, dst cmEP
				if k < len(e.Sources) {
					src = e.Sources[k]
				}
				if k < len(e.Dests) {
					dst = e.Dests[k]
				}
				// Mark even when skipped, so a software device's endpoints never
				// resurface below as anonymous "virtual" ports.
				if src.Ref != 0 {
					seen[src.Ref] = true
				}
				if dst.Ref != 0 {
					seen[dst.Ref] = true
				}
				if kind == router.KindVirtual && !includeVirtual {
					continue
				}
				if src.Ref == 0 && dst.Ref == 0 {
					continue
				}
				uid := src.UID
				if src.Ref == 0 {
					uid = dst.UID
				}
				name := cmFirstNonEmpty(src.Name, dst.Name, e.Name, d.Name)
				detail := "CoreMIDI"
				if mm := strings.TrimSpace(cmFirstNonEmpty(d.Manufacturer) + " " + cmFirstNonEmpty(d.Model)); mm != "" {
					detail += " · " + mm
				}
				specs = append(specs, cmPortSpec{
					id:   uniq(cmSanitizeID(fmt.Sprintf("%s:cm-%08x", idPrefix, uint32(uid)))),
					name: name, group: devLabel, detail: detail, kind: kind,
					src: src, dst: dst,
				})
			}
		}
	}

	if !includeVirtual {
		return specs
	}

	// Free-standing endpoints: pair sources and destinations by name.
	type bucket struct{ srcs, dsts []cmEP }
	var order []string
	byName := map[string]*bucket{}
	add := func(ep cmEP, isSrc bool) {
		if ep.Ref == 0 || seen[ep.Ref] {
			return
		}
		b := byName[ep.Name]
		if b == nil {
			b = &bucket{}
			byName[ep.Name] = b
			order = append(order, ep.Name)
		}
		if isSrc {
			b.srcs = append(b.srcs, ep)
		} else {
			b.dsts = append(b.dsts, ep)
		}
	}
	for _, ep := range g.Sources {
		add(ep, true)
	}
	for _, ep := range g.Dests {
		add(ep, false)
	}
	for _, name := range order {
		b := byName[name]
		n := len(b.srcs)
		if len(b.dsts) > n {
			n = len(b.dsts)
		}
		for i := 0; i < n; i++ {
			var src, dst cmEP
			if i < len(b.srcs) {
				src = b.srcs[i]
			}
			if i < len(b.dsts) {
				dst = b.dsts[i]
			}
			label := name
			if strings.TrimSpace(label) == "" {
				label = "Virtual port"
			}
			specs = append(specs, cmPortSpec{
				id:   uniq(cmSanitizeID(fmt.Sprintf("virtual:cm-%08x", cmNameHash(name)))),
				name: label, group: "Virtual", detail: "CoreMIDI virtual port", kind: router.KindVirtual,
				src: src, dst: dst,
			})
		}
	}
	return specs
}
