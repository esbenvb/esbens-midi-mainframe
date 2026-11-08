package device

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"emm/internal/router"
)

func rawDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- fake sysfs ------------------------------------------------------------

type rawFakeSys struct {
	t    *testing.T
	root string
}

func newRawFakeSys(t *testing.T) *rawFakeSys {
	t.Helper()
	s := &rawFakeSys{t: t, root: t.TempDir()}
	rawMust(t, os.MkdirAll(filepath.Join(s.root, "class", "sound"), 0o755))
	return s
}

func rawMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type rawUSBAttrs struct {
	vid, pid, manufacturer, product, serial string
	busnum, devnum                          string
}

// usb creates /devices/.../<busPath>/ with the sysfs attribute files of a USB
// device and returns its directory.
func (s *rawFakeSys) usb(busPath string, a rawUSBAttrs) string {
	s.t.Helper()
	dir := filepath.Join(s.root, "devices", "pci0000:00", "usb1", busPath)
	rawMust(s.t, os.MkdirAll(dir, 0o755))
	if a.busnum == "" {
		a.busnum = "1"
	}
	if a.devnum == "" {
		a.devnum = "2"
	}
	for name, val := range map[string]string{
		"idVendor": a.vid, "idProduct": a.pid, "manufacturer": a.manufacturer,
		"product": a.product, "serial": a.serial, "busnum": a.busnum, "devnum": a.devnum,
	} {
		if val != "" {
			rawMust(s.t, os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644))
		}
	}
	return dir
}

// midi adds /sys/class/sound/midiC<card>D<dev> pointing below the USB device,
// the way the kernel lays it out.
func (s *rawFakeSys) midi(usbDir string, card, dev int) {
	s.t.Helper()
	real := filepath.Join(usbDir, filepath.Base(usbDir)+":1.1", "sound",
		fmt.Sprintf("card%d", card), fmt.Sprintf("midiC%dD%d", card, dev))
	rawMust(s.t, os.MkdirAll(real, 0o755))
	rawMust(s.t, os.Symlink(real, filepath.Join(s.root, "class", "sound", fmt.Sprintf("midiC%dD%d", card, dev))))
}

// nonUSB adds a raw MIDI device that has no USB ancestor (e.g. snd-virmidi).
func (s *rawFakeSys) nonUSB(card, dev int, cardID string) {
	s.t.Helper()
	real := filepath.Join(s.root, "devices", "platform", "snd_virmidi", "sound",
		fmt.Sprintf("card%d", card), fmt.Sprintf("midiC%dD%d", card, dev))
	rawMust(s.t, os.MkdirAll(real, 0o755))
	rawMust(s.t, os.Symlink(real, filepath.Join(s.root, "class", "sound", fmt.Sprintf("midiC%dD%d", card, dev))))
	cardDir := filepath.Join(s.root, "class", "sound", fmt.Sprintf("card%d", card))
	rawMust(s.t, os.MkdirAll(cardDir, 0o755))
	rawMust(s.t, os.WriteFile(filepath.Join(cardDir, "id"), []byte(cardID+"\n"), 0o644))
}

// unplug removes the class entry and the whole USB device directory.
func (s *rawFakeSys) unplug(usbDir string, card, dev int) {
	s.t.Helper()
	_ = os.Remove(filepath.Join(s.root, "class", "sound", fmt.Sprintf("midiC%dD%d", card, dev)))
	rawMust(s.t, os.RemoveAll(usbDir))
}

func (s *rawFakeSys) opts() RawMIDIOptions {
	return RawMIDIOptions{SysRoot: s.root, DevRoot: "/dev/snd", Poll: 10 * time.Millisecond, Logger: rawDiscardLogger()}.withDefaults()
}

// ---- fake kernel access ----------------------------------------------------

// rawFakeDevice is the "device side" of a fake node: the test writes what the
// device sends into toHost, and reads what the host wrote from fromHost.
type rawFakeDevice struct {
	toHost   *os.File
	fromHost *os.File
	node     *rawFakeNode
}

type rawFakeNode struct {
	r, w   *os.File // host side of the two pipes
	mu     sync.Mutex
	closed bool
}

func (n *rawFakeNode) Read(p []byte) (int, error)  { return n.r.Read(p) }
func (n *rawFakeNode) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n *rawFakeNode) Close() error {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.r.Close()
	n.w.Close()
	return nil
}
func (n *rawFakeNode) isClosed() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.closed }

func newRawPipeNode(t *testing.T) (*rawFakeNode, *rawFakeDevice) {
	t.Helper()
	inR, inW, err := os.Pipe()
	rawMust(t, err)
	outR, outW, err := os.Pipe()
	rawMust(t, err)
	n := &rawFakeNode{r: inR, w: outW}
	d := &rawFakeDevice{toHost: inW, fromHost: outR, node: n}
	t.Cleanup(func() { inW.Close(); outR.Close(); n.Close() })
	return n, d
}

type rawFakeAccess struct {
	t *testing.T

	mu       sync.Mutex
	subs     map[[2]int][]rawSub
	probeErr map[[2]int]error
	openFail map[string]int // by "hw:c,d,s": failures left (-1 = always)
	opens    map[string]int
	devs     map[string]*rawFakeDevice
}

func newRawFakeAccess(t *testing.T) *rawFakeAccess {
	return &rawFakeAccess{
		t: t, subs: map[[2]int][]rawSub{}, probeErr: map[[2]int]error{},
		openFail: map[string]int{}, opens: map[string]int{}, devs: map[string]*rawFakeDevice{},
	}
}

func rawHwName(card, dev, sub int) string { return fmt.Sprintf("hw:%d,%d,%d", card, dev, sub) }

func (a *rawFakeAccess) setSubs(card, dev int, subs ...rawSub) {
	a.mu.Lock()
	a.subs[[2]int{card, dev}] = subs
	a.mu.Unlock()
}

func (a *rawFakeAccess) Probe(card, dev int) ([]rawSub, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.probeErr[[2]int{card, dev}]; err != nil {
		return nil, err
	}
	if s, ok := a.subs[[2]int{card, dev}]; ok {
		return s, nil
	}
	return []rawSub{{Sub: 0, Input: true, Output: true}}, nil
}

func (a *rawFakeAccess) Open(card, dev, sub int, in, out bool) (rawNode, error) {
	hw := rawHwName(card, dev, sub)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens[hw]++
	if rem := a.openFail[hw]; rem != 0 {
		if rem > 0 {
			a.openFail[hw]--
		}
		return nil, errors.New("fake: device busy")
	}
	n, d := newRawPipeNode(a.t)
	a.devs[hw] = d
	return n, nil
}

func (a *rawFakeAccess) openCount(hw string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opens[hw]
}

func (a *rawFakeAccess) setOpenFail(hw string, n int) {
	a.mu.Lock()
	a.openFail[hw] = n
	a.mu.Unlock()
}

// dev waits for the first open of hw and returns the device side.
func (a *rawFakeAccess) dev(hw string) *rawFakeDevice {
	a.t.Helper()
	var d *rawFakeDevice
	rawWaitFor(a.t, "device "+hw+" opened", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		d = a.devs[hw]
		return d != nil
	})
	return d
}

// ---- router-side fakes ------------------------------------------------------

type rawRecPort struct {
	info router.PortInfo
	mu   sync.Mutex
	got  []byte
}

func (p *rawRecPort) Info() router.PortInfo { return p.info }
func (p *rawRecPort) Write(b []byte) error {
	p.mu.Lock()
	p.got = append(p.got, b...)
	p.mu.Unlock()
	return nil
}
func (p *rawRecPort) Close() error { return nil }
func (p *rawRecPort) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.got...)
}

func rawWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func rawMkdirAll(parts ...string) error { return os.MkdirAll(filepath.Join(parts...), 0o755) }
