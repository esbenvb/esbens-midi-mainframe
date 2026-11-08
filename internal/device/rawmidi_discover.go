package device

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"emm/internal/router"
)

// FoundPort is one MIDI port (one USB cable / ALSA raw MIDI subdevice) found
// by discovery. It is JSON-serialisable, so `emmd --list-devices` can print it.
type FoundPort struct {
	// ID is the stable router port ID, see docs/ARCHITECTURE.md.
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Group  string          `json:"group,omitempty"`
	Kind   router.PortKind `json:"kind"`
	Detail string          `json:"detail,omitempty"`
	Input  bool            `json:"input"`
	Output bool            `json:"output"`

	Card   int    `json:"card"`
	Device int    `json:"device"`
	Sub    int    `json:"sub"`
	Hw     string `json:"hw"`   // ALSA name, e.g. "hw:1,0,2"
	Node   string `json:"node"` // device node, e.g. "/dev/snd/midiC1D0"

	// USB identity (empty for non-USB ports).
	VendorID     string `json:"vendor_id,omitempty"`
	ProductID    string `json:"product_id,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Serial       string `json:"serial,omitempty"`
	// USBPath is the kernel's name for the device's place in the bus tree,
	// "<bus>-<port.port...>" (e.g. "1-1.2"): the physical hub port.
	USBPath string `json:"usb_path,omitempty"`
	// Gen changes whenever the device is re-enumerated (unplugged and plugged
	// in again), even if it lands on the same card number and USB port.
	Gen string `json:"gen,omitempty"`

	// SerialUsed tells whether ID is built from the serial number (true) or
	// from the USB port path (false).
	SerialUsed bool `json:"serial_used,omitempty"`
}

// rawSub describes one subdevice (cable) of a raw MIDI device.
type rawSub struct {
	Sub    int
	Name   string
	Input  bool // the host can read from it (device -> host)
	Output bool // the host can write to it (host -> device)
}

// rawNode is an open raw MIDI subdevice. *os.File satisfies it.
type rawNode interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// rawAccess is the part that talks to the kernel; faked in tests.
type rawAccess interface {
	// Probe lists the subdevices of raw MIDI device (card, dev) with their
	// directions and names.
	Probe(card, dev int) ([]rawSub, error)
	// Open opens subdevice sub of (card, dev). in/out select the directions.
	// Writes must block until accepted; Close must wake blocked Read/Write.
	Open(card, dev, sub int, in, out bool) (rawNode, error)
}

// rawDev is a raw MIDI device node found in sysfs, before subdevice expansion.
type rawDev struct {
	card, dev int
	usb       *usbInfo // nil for non-USB cards
}

type usbInfo struct {
	dir          string // resolved sysfs directory of the USB device
	path         string // base name, e.g. "1-1.2"
	vid, pid     string
	manufacturer string
	product      string
	serial       string
	busnum       string
	devnum       string
}

// rawDiscover scans <SysRoot>/class/sound for raw MIDI devices and expands
// them into ports. A missing sysfs sound class is not an error (no sound
// subsystem loaded: nothing to find).
//
// taint, when not nil, remembers (vendor:product:serial) triples that have been
// seen on two USB devices at once. Once tainted, a triple keeps using
// port-path IDs for as long as the map lives, so unplugging one of two
// identical synths does not make the other one's ID flip from path-based to
// serial-based (which would orphan its connections). The map is per daemon
// run: after a restart with only one of the pair attached, the serial is
// trusted again.
func rawDiscover(o RawMIDIOptions, acc rawAccess, log *slog.Logger, taint map[string]bool) ([]FoundPort, error) {
	if log == nil {
		log = slog.Default()
	}
	classDir := filepath.Join(o.SysRoot, "class", "sound")
	ents, err := os.ReadDir(classDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var devs []rawDev
	for _, e := range ents {
		card, dev, ok := parseMidiName(e.Name())
		if !ok {
			continue
		}
		d := rawDev{card: card, dev: dev, usb: findUSBAncestor(filepath.Join(classDir, e.Name()))}
		if d.usb == nil && !o.IncludeNonUSB {
			continue
		}
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].card != devs[j].card {
			return devs[i].card < devs[j].card
		}
		return devs[i].dev < devs[j].dev
	})

	// Probe subdevices of every device up front: the number of cables across a
	// whole USB device decides how ports are named.
	subs := make([][]rawSub, len(devs))
	portsPerUSB := map[string]int{}
	for i, d := range devs {
		s, perr := acc.Probe(d.card, d.dev)
		if perr != nil || len(s) == 0 {
			log.Debug("raw MIDI probe failed, assuming a single bidirectional port",
				"card", d.card, "dev", d.dev, "err", perr)
			s = []rawSub{{Sub: 0, Input: true, Output: true}}
		}
		subs[i] = s
		if d.usb != nil {
			portsPerUSB[d.usb.dir] += len(s)
		}
	}

	// Serial numbers are only trusted when they look real and are unique among
	// the USB devices present right now (two identical synths can report the
	// same, or a placeholder, serial).
	serialCount := map[string]map[string]bool{}
	for _, d := range devs {
		if d.usb == nil {
			continue
		}
		k := d.usb.vid + ":" + d.usb.pid + ":" + d.usb.serial
		if serialCount[k] == nil {
			serialCount[k] = map[string]bool{}
		}
		serialCount[k][d.usb.dir] = true
	}
	if taint != nil {
		for k, dirs := range serialCount {
			if len(dirs) > 1 {
				taint[k] = true
			}
		}
	}
	serialOK := func(u *usbInfo) bool {
		k := u.vid + ":" + u.pid + ":" + u.serial
		return usableSerial(u.serial) && len(serialCount[k]) == 1 && !taint[k]
	}

	var out []FoundPort
	seen := map[string]bool{}
	ordinal := map[string]int{} // ports named so far per USB device / card
	for i, d := range devs {
		for _, s := range subs[i] {
			fp := FoundPort{
				Card: d.card, Device: d.dev, Sub: s.Sub,
				Hw:    fmt.Sprintf("hw:%d,%d,%d", d.card, d.dev, s.Sub),
				Node:  filepath.Join(o.DevRoot, fmt.Sprintf("midiC%dD%d", d.card, d.dev)),
				Input: s.Input, Output: s.Output,
			}
			cable := cableID(d.dev, s.Sub)
			if u := d.usb; u != nil {
				fp.Kind = router.KindUSB
				fp.VendorID, fp.ProductID = u.vid, u.pid
				fp.Manufacturer, fp.Product, fp.Serial = u.manufacturer, u.product, u.serial
				fp.USBPath = u.path
				fp.Gen = u.busnum + "/" + u.devnum
				id := u.path
				if serialOK(u) {
					id, fp.SerialUsed = u.serial, true
				}
				fp.ID = "usb:" + sanitizeID(u.vid) + ":" + sanitizeID(u.pid) + ":" + sanitizeID(id) + ":" + cable
				fp.Group = groupName(u.manufacturer, u.product)
				product := u.product
				if product == "" {
					product = "USB MIDI " + u.vid + ":" + u.pid
				}
				if fp.Group == "" {
					fp.Group = product
				}
				fp.Name = portName(product, s.Name, ordinal[u.dir], portsPerUSB[u.dir] > 1)
				ordinal[u.dir]++
				fp.Detail = fmt.Sprintf("USB %s · %s:%s · %s", u.path, u.vid, u.pid, fp.Hw)
			} else {
				cardID := readAttr(filepath.Join(classDir, fmt.Sprintf("card%d", d.card)), "id")
				if cardID == "" {
					cardID = "card" + strconv.Itoa(d.card)
				}
				fp.Kind = router.KindVirtual
				fp.ID = "alsa:" + sanitizeID(cardID) + ":" + cable
				fp.Group = cardID
				fp.Name = portName(cardID, s.Name, ordinal[cardID], len(subs[i]) > 1)
				ordinal[cardID]++
				fp.Detail = fp.Hw
			}
			if seen[fp.ID] { // should not happen; never hand the router duplicates
				fp.ID += "~" + strconv.Itoa(d.card)
			}
			seen[fp.ID] = true
			out = append(out, fp)
		}
	}
	return out, nil
}

// cableID is the last field of a USB port ID: the subdevice number for the
// first raw MIDI device of a card, "<dev>-<sub>" for further ones.
func cableID(dev, sub int) string {
	if dev == 0 {
		return strconv.Itoa(sub)
	}
	return fmt.Sprintf("%d-%d", dev, sub)
}

func parseMidiName(name string) (card, dev int, ok bool) {
	rest, found := strings.CutPrefix(name, "midiC")
	if !found {
		return 0, 0, false
	}
	c, d, found := strings.Cut(rest, "D")
	if !found {
		return 0, 0, false
	}
	var err error
	if card, err = strconv.Atoi(c); err != nil || card < 0 {
		return 0, 0, false
	}
	if dev, err = strconv.Atoi(d); err != nil || dev < 0 {
		return 0, 0, false
	}
	return card, dev, true
}

func readAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// findUSBAncestor resolves the sysfs class entry and walks up its physical
// parents to the USB device (the directory that has idVendor and idProduct).
func findUSBAncestor(classEntry string) *usbInfo {
	// The entry itself is a symlink into /sys/devices/...; its "device" link
	// points at the card. Either leads to the USB interface and its parent.
	for _, start := range []string{classEntry, filepath.Join(classEntry, "device")} {
		real, err := filepath.EvalSymlinks(start)
		if err != nil {
			continue
		}
		p := real
		for i := 0; i < 16; i++ {
			parent := filepath.Dir(p)
			if parent == p || parent == "/" || parent == "." {
				break
			}
			p = parent
			if _, err := os.Stat(filepath.Join(p, "idVendor")); err == nil {
				return &usbInfo{
					dir:          p,
					path:         filepath.Base(p),
					vid:          strings.ToLower(readAttr(p, "idVendor")),
					pid:          strings.ToLower(readAttr(p, "idProduct")),
					manufacturer: readAttr(p, "manufacturer"),
					product:      readAttr(p, "product"),
					serial:       readAttr(p, "serial"),
					busnum:       readAttr(p, "busnum"),
					devnum:       readAttr(p, "devnum"),
				}
			}
		}
	}
	return nil
}

// usableSerial reports whether a USB serial number looks like a real, unique
// per-unit identifier rather than empty or a well-known placeholder.
func usableSerial(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return false
	}
	l := strings.ToLower(s)
	// all one repeated character: 0000, FFFF, ....
	same := true
	for i := 1; i < len(l); i++ {
		if l[i] != l[0] {
			same = false
			break
		}
	}
	if same {
		return false
	}
	switch strings.Trim(l, " -_.") {
	case "", "serial", "serialnumber", "none", "null", "n/a", "na", "unknown", "default",
		"default string", "not specified", "notspecified", "to be filled by o.e.m.",
		"123456", "1234567", "12345678", "123456789", "1234567890", "0123456789",
		"123456789a", "123456789ab", "123456789abc", "123456789abcd", "123456789abcdef",
		"0123456789ab", "0123456789abcdef", "abcdef", "0123456789abcdef0123456789abcdef":
		return false
	}
	// A pure ascending run of hex digits starting at 0 or 1 is a placeholder too.
	if isAscendingHex(l) {
		return false
	}
	return true
}

func isAscendingHex(s string) bool {
	const digits = "0123456789abcdef"
	if len(s) < 6 {
		return false
	}
	start := strings.IndexByte(digits, s[0])
	if start < 0 || start > 1 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if start+i >= len(digits) || s[i] != digits[start+i] {
			return false
		}
	}
	return true
}

// sanitizeID keeps only [A-Za-z0-9:._-]; anything else becomes '_'.
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ':':
			b.WriteRune('_') // ':' separates the ID fields
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// groupName joins manufacturer and product without repeating the manufacturer.
func groupName(manufacturer, product string) string {
	m, p := strings.TrimSpace(manufacturer), strings.TrimSpace(product)
	switch {
	case m == "":
		return p
	case p == "":
		return m
	case strings.HasPrefix(strings.ToLower(p), strings.ToLower(m)):
		return p
	}
	return m + " " + p
}

// portName names one port. With a single port it is just the product; with
// several cables the kernel's subdevice name is used (usually "<product> MIDI
// n"), falling back to "<product> <n>" where n counts the ports of the device.
func portName(product, subName string, ordinal int, several bool) string {
	if !several {
		return product
	}
	subName = strings.TrimSpace(subName)
	switch {
	case subName == "":
		return fmt.Sprintf("%s %d", product, ordinal+1)
	case strings.Contains(strings.ToLower(subName), strings.ToLower(product)):
		return subName
	}
	return product + " " + subName
}
