// Raw MIDI backend: USB MIDI devices through ALSA raw MIDI device nodes.
//
// UNVERIFIED ON HARDWARE. Everything here is unit-tested against fake sysfs
// trees and fake device nodes (pipes) on the development machine, and the
// Linux-only ioctl code is cross-compiled and vetted, but it has not been run
// against a real kernel with real USB devices. The parts that depend on kernel
// behaviour are isolated in rawmidi_linux.go; see the notes there.
//
// How it works:
//   - Discovery reads /sys/class/sound/midiC<card>D<dev>, walks up to the USB
//     device for identity (vendor/product/serial/port path), and asks the
//     control node which subdevices (cables) and directions exist.
//   - Each subdevice becomes one router port, opened as /dev/snd/midiC<c>D<d>
//     with the subdevice chosen by the PREFER_SUBDEVICE ioctl.
//   - The ALSA sequencer is deliberately not used: it cannot throttle senders
//     and splits SysEx into 256-byte events. Raw MIDI has real back-pressure.
//   - Hot-plug is a 1 s poll of sysfs (no udev/netlink needed, which keeps it
//     working in a minimal initramfs system).
//
// I/O semantics relied on (Linux): an *os.File opened on a pollable character
// device is registered with the Go runtime poller and put in non-blocking
// mode. Read and Write then park only the calling goroutine; Write loops until
// every byte has been accepted by the kernel buffer (so it blocks while the
// device is slower than the source, which is the back-pressure the router
// wants); Close wakes parked Read/Write calls with an error. The tests prove
// these semantics on pipes, which use the same poller.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/router"
)

// RawMIDIOptions configures the raw MIDI backend. The zero value is usable.
type RawMIDIOptions struct {
	// SysRoot is where sysfs is mounted (default "/sys").
	SysRoot string
	// DevRoot is the directory with the ALSA device nodes (default "/dev/snd").
	DevRoot string
	// Poll is the hot-plug polling interval (default 1s).
	Poll time.Duration
	// IncludeNonUSB also exposes raw MIDI devices that are not USB (virmidi,
	// on-board or PCI interfaces) as virtual ports with `alsa:` IDs.
	IncludeNonUSB bool
	Logger        *slog.Logger
}

func (o RawMIDIOptions) withDefaults() RawMIDIOptions {
	if o.SysRoot == "" {
		o.SysRoot = "/sys"
	}
	if o.DevRoot == "" {
		o.DevRoot = "/dev/snd"
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.Logger = o.Logger.With("component", "rawmidi")
	return o
}

var (
	errRawPortClosed  = errors.New("rawmidi: port closed")
	errRawPortFailed  = errors.New("rawmidi: port failed")
	errRawInputOnly   = errors.New("rawmidi: port cannot send MIDI")
	errRawUnsupported = errors.New("rawmidi: raw MIDI access is only available on Linux")
)

// rawBackend implements router.Backend.
type rawBackend struct {
	opts RawMIDIOptions
	acc  rawAccess
	log  *slog.Logger

	// tunables (tests shorten them)
	retryBase      time.Duration // first open retry delay; doubles each attempt
	maxAttempts    int           // open attempts before giving up on a plugged-in device
	readerStopWait time.Duration // how long shutdown waits for reader goroutines
	closeWait      time.Duration // how long Port.Close waits for the kernel close
}

func newRawBackend(o RawMIDIOptions, acc rawAccess) *rawBackend {
	o = o.withDefaults()
	return &rawBackend{
		opts: o, acc: acc, log: o.Logger,
		retryBase:      250 * time.Millisecond,
		maxAttempts:    6,
		readerStopWait: 3 * time.Second,
		closeWait:      3 * time.Second,
	}
}

func (b *rawBackend) Name() string { return "rawmidi" }

type entryState int

const (
	statePending entryState = iota // present in sysfs, not open yet (or retrying)
	stateActive                    // open and registered with the router
	stateDead                      // was open, then failed; stays down until re-plugged
	stateGaveUp                    // could not be opened; stays down until re-plugged
)

type rawEntry struct {
	fp       FoundPort
	key      string
	state    entryState
	attempts int
	nextTry  time.Time
	port     *rawPort
}

type rawSession struct {
	b     *rawBackend
	host  router.Host
	items map[string]*rawEntry
	order []string // keys in discovery order, for deterministic iteration
	dead  chan *rawEntry
	stop  chan struct{}
	wg    sync.WaitGroup

	taint       map[string]bool // see rawDiscover
	lastScanErr time.Time
}

// rawKey identifies one plugged-in port: a re-plug changes Gen, a different
// device on the same card number changes ID.
func rawKey(fp FoundPort) string { return fp.ID + "|" + fp.Hw + "|" + fp.Gen }

// Run implements router.Backend: it polls for ports until ctx is cancelled.
func (b *rawBackend) Run(ctx context.Context, h router.Host) error {
	s := &rawSession{
		b: b, host: h,
		items: map[string]*rawEntry{},
		dead:  make(chan *rawEntry, 64),
		stop:  make(chan struct{}),
		taint: map[string]bool{},
	}
	defer s.shutdown()

	var nextPoll time.Time // zero: poll immediately
	for {
		now := time.Now()
		if !now.Before(nextPoll) {
			s.scan(now)
			nextPoll = now.Add(b.opts.Poll)
		}
		s.openDue(now)

		wake := nextPoll
		if t, ok := s.earliestRetry(); ok && t.Before(wake) {
			wake = t
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case e := <-s.dead:
			timer.Stop()
			s.onDead(e)
		case <-timer.C:
		}
	}
}

// scan diffs sysfs against what is tracked.
func (s *rawSession) scan(now time.Time) {
	found, err := rawDiscover(s.b.opts, s.b.acc, s.b.log, s.taint)
	if err != nil {
		// A failed listing says nothing about the devices: keep what we have.
		if time.Since(s.lastScanErr) > 30*time.Second {
			s.lastScanErr = time.Now()
			s.b.log.Warn("raw MIDI discovery failed", "err", err)
		}
		return
	}
	cur := make(map[string]FoundPort, len(found))
	keys := make([]string, 0, len(found))
	for _, fp := range found {
		k := rawKey(fp)
		cur[k] = fp
		keys = append(keys, k)
	}

	// Gone (or replaced by a re-plug): remove first, so a replacement with the
	// same port ID is added to a clean slate.
	for _, k := range s.order {
		if _, ok := cur[k]; !ok {
			s.drop(s.items[k])
			delete(s.items, k)
		}
	}
	for _, k := range keys {
		if _, ok := s.items[k]; !ok {
			s.items[k] = &rawEntry{fp: cur[k], key: k, state: statePending, nextTry: now}
		}
	}
	s.order = keys
}

// drop unregisters an entry that vanished from sysfs.
func (s *rawSession) drop(e *rawEntry) {
	if e == nil {
		return
	}
	if e.state == stateActive {
		s.b.log.Info("device removed", "id", e.fp.ID, "hw", e.fp.Hw)
		s.release(e)
	}
	e.state = stateGaveUp // never touched again; stale readers check the map
}

// release closes the device node and unregisters the port. The node is closed
// first on purpose: if the router's writer is parked inside Write (a device
// that stopped draining), closing wakes it, so RemovePort does not have to
// wait out its grace period.
func (s *rawSession) release(e *rawEntry) {
	if e.port != nil {
		_ = e.port.Close()
	}
	s.host.RemovePort(e.fp.ID)
}

func (s *rawSession) earliestRetry() (time.Time, bool) {
	var t time.Time
	ok := false
	for _, k := range s.order {
		e := s.items[k]
		if e != nil && e.state == statePending && (!ok || e.nextTry.Before(t)) {
			t, ok = e.nextTry, true
		}
	}
	return t, ok
}

func (s *rawSession) openDue(now time.Time) {
	for _, k := range s.order {
		e := s.items[k]
		if e != nil && e.state == statePending && !now.Before(e.nextTry) {
			s.tryOpen(e, now)
		}
	}
}

func (s *rawSession) tryOpen(e *rawEntry, now time.Time) {
	fp := e.fp
	if !fp.Input && !fp.Output {
		e.state = stateGaveUp
		return
	}
	node, err := s.b.acc.Open(fp.Card, fp.Device, fp.Sub, fp.Input, fp.Output)
	if err != nil {
		e.attempts++
		if e.attempts >= s.b.maxAttempts {
			e.state = stateGaveUp
			s.b.log.Warn("giving up opening MIDI port until it is re-plugged",
				"id", fp.ID, "hw", fp.Hw, "attempts", e.attempts, "err", err)
			return
		}
		delay := s.b.retryBase << (e.attempts - 1)
		e.nextTry = now.Add(delay)
		s.b.log.Debug("open failed, will retry", "id", fp.ID, "hw", fp.Hw, "attempt", e.attempts, "in", delay, "err", err)
		return
	}

	p := &rawPort{
		info: router.PortInfo{
			ID: fp.ID, Name: fp.Name, Kind: fp.Kind, Group: fp.Group,
			Input: fp.Input, Output: fp.Output, Detail: fp.Detail,
		},
		node:      node,
		closeWait: s.b.closeWait,
		onFail: func() {
			select {
			case s.dead <- e:
			case <-s.stop:
			}
		},
	}
	e.port = p
	e.state = stateActive
	h := s.host.AddPort(p)
	s.b.log.Info("device added", "id", fp.ID, "name", fp.Name, "hw", fp.Hw, "in", fp.Input, "out", fp.Output)

	if fp.Input {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			p.readLoop(h)
		}()
	}
}

// onDead handles a port whose I/O failed (typically an unplug noticed before
// the next poll). It stays down until it vanishes from sysfs and returns.
func (s *rawSession) onDead(e *rawEntry) {
	if s.items[e.key] != e || e.state != stateActive {
		return
	}
	s.b.log.Warn("MIDI port failed; removing it until the device is re-plugged", "id", e.fp.ID, "hw", e.fp.Hw)
	e.state = stateDead
	s.release(e)
}

func (s *rawSession) shutdown() {
	close(s.stop)
	for _, k := range s.order {
		if e := s.items[k]; e != nil && e.state == stateActive {
			e.state = stateGaveUp
			s.release(e)
		}
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.b.readerStopWait):
		s.b.log.Warn("reader goroutines did not stop in time")
	}
}

// rawPort is one open subdevice. It implements router.Port.
type rawPort struct {
	info      router.PortInfo
	node      rawNode
	closeWait time.Duration
	onFail    func()

	closed atomic.Bool
	failed atomic.Bool
	once   sync.Once
}

func (p *rawPort) Info() router.PortInfo { return p.info }

// Write sends bytes to the device. It blocks until every byte has been
// accepted. After the first I/O error the port is failed: later writes are
// refused immediately, so nothing can follow a possibly truncated message.
func (p *rawPort) Write(b []byte) error {
	if !p.info.Output {
		return errRawInputOnly
	}
	if p.closed.Load() {
		return errRawPortClosed
	}
	if p.failed.Load() {
		return errRawPortFailed
	}
	if len(b) == 0 {
		return nil
	}
	n, err := p.node.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if p.closed.Load() {
			return errRawPortClosed
		}
		p.fail()
		return fmt.Errorf("rawmidi write: %w", err)
	}
	return nil
}

func (p *rawPort) fail() {
	if p.failed.CompareAndSwap(false, true) && p.onFail != nil {
		p.onFail()
	}
}

// Close closes the device node (waking any blocked Write or Read). The kernel
// may delay close(2) while it drains pending output, so the wait is bounded.
func (p *rawPort) Close() error {
	var err error
	p.once.Do(func() {
		p.closed.Store(true)
		done := make(chan error, 1)
		go func() { done <- p.node.Close() }()
		select {
		case err = <-done:
		case <-time.After(p.closeWait):
			err = errors.New("rawmidi: close timed out")
		}
	})
	return err
}

// readLoop feeds received bytes to the router until the node fails or closes.
func (p *rawPort) readLoop(h *router.Handle) {
	buf := make([]byte, 4096)
	for {
		n, err := p.node.Read(buf)
		if n > 0 {
			h.Receive(buf[:n])
		}
		if err != nil {
			if !p.closed.Load() {
				p.fail()
			}
			return
		}
	}
}
