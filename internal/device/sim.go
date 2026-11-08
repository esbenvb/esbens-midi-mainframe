package device

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/router"
)

// SimOptions configures the simulated devices.
type SimOptions struct {
	// Hotplug makes the simulated synth unplug and re-plug itself every ~20 s,
	// to exercise dormant connections and the UI.
	Hotplug bool
	Logger  *slog.Logger
}

// NewSim returns a backend that provides a handful of fake MIDI devices, so the
// whole daemon (router, web UI, RTP-MIDI) can be developed without hardware:
//
//   - "Sim Keyboard": input only, plays a slow arpeggio and sweeps the mod wheel.
//   - "Sim Synth": in/out, answers every note-on it receives with a CC 74 sweep.
//   - "Sim Drum Machine": in/out, plays a four-on-the-floor pattern on channel 10.
//   - "Sim Sampler": in/out, swallows SysEx at ~150 kB/s like a slowish device,
//     which makes back-pressure through the router visible.
func NewSim(o SimOptions) router.Backend {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &sim{opts: o, log: log.With("component", "sim")}
}

type sim struct {
	opts SimOptions
	log  *slog.Logger
}

func (s *sim) Name() string { return "sim" }

type simDevice struct {
	id, name string
	in, out  bool
	rate     int // bytes/s the device accepts; 0 = unlimited
	run      func(ctx context.Context, d *simRunning)
	onWrite  func(d *simRunning, b []byte)
	hotplug  bool
}

// simRunning is one plugged-in instance of a simDevice.
type simRunning struct {
	dev    simDevice
	handle atomic.Pointer[router.Handle] // set once AddPort returns; Write can race ahead of that
	rxMu   sync.Mutex                    // Handle.Receive is single-goroutine: serialise generator + echo
	wrote  atomic.Uint64
	log    *slog.Logger
}

func (d *simRunning) send(b []byte) {
	h := d.handle.Load()
	if h == nil {
		return
	}
	d.rxMu.Lock()
	h.Receive(b)
	d.rxMu.Unlock()
}

type simPort struct {
	info router.PortInfo
	d    *simRunning
	rate int
	done chan struct{}
	once sync.Once
}

func (p *simPort) Info() router.PortInfo { return p.info }
func (p *simPort) Write(b []byte) error {
	if p.rate > 0 {
		t := time.NewTimer(time.Duration(len(b)) * time.Second / time.Duration(p.rate))
		select {
		case <-t.C:
		case <-p.done:
			t.Stop()
			return nil
		}
	}
	p.d.wrote.Add(uint64(len(b)))
	if p.d.dev.onWrite != nil {
		p.d.dev.onWrite(p.d, b)
	}
	return nil
}
func (p *simPort) Close() error { p.once.Do(func() { close(p.done) }); return nil }

func (s *sim) devices() []simDevice {
	return []simDevice{
		{id: "sim:keyboard", name: "Sim Keyboard", in: true, run: runKeyboard},
		{id: "sim:synth", name: "Sim Synth", in: true, out: true, onWrite: synthEcho, hotplug: true},
		{id: "sim:drums", name: "Sim Drum Machine", in: true, out: true, run: runDrums},
		{id: "sim:sampler", name: "Sim Sampler", in: true, out: true, rate: 150_000},
	}
}

func (s *sim) Run(ctx context.Context, h router.Host) error {
	var wg sync.WaitGroup
	for _, dev := range s.devices() {
		wg.Add(1)
		go func(dev simDevice) {
			defer wg.Done()
			s.runDevice(ctx, h, dev)
		}(dev)
	}
	wg.Wait()
	return nil
}

func (s *sim) runDevice(ctx context.Context, h router.Host, dev simDevice) {
	for {
		pctx, unplug := context.WithCancel(ctx)
		s.plug(pctx, h, dev)
		if s.opts.Hotplug && dev.hotplug {
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Second):
			}
			unplug()
			h.RemovePort(dev.id)
			select {
			case <-ctx.Done():
				return
			case <-time.After(6 * time.Second): // unplugged for a while
			}
			continue
		}
		<-ctx.Done()
		unplug()
		h.RemovePort(dev.id)
		return
	}
}

func (s *sim) plug(ctx context.Context, h router.Host, dev simDevice) {
	d := &simRunning{dev: dev, log: s.log}
	port := &simPort{
		d: d, rate: dev.rate, done: make(chan struct{}),
		info: router.PortInfo{
			ID: dev.id, Name: dev.name, Kind: router.KindSim, Group: dev.name,
			Input: dev.in, Output: dev.out, Detail: "simulated device",
		},
	}
	d.handle.Store(h.AddPort(port))
	if dev.run != nil {
		go dev.run(ctx, d)
	}
}

// ---- device behaviours -------------------------------------------------------

func runKeyboard(ctx context.Context, d *simRunning) {
	notes := []byte{60, 63, 67, 70, 72, 70, 67, 63} // C minor 7 arpeggio
	i := 0
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n := notes[i%len(notes)]
		d.send([]byte{0x90, n, 90})
		select {
		case <-ctx.Done():
			return
		case <-time.After(180 * time.Millisecond):
		}
		d.send([]byte{0x80, n, 0})
		if i%8 == 0 {
			d.send([]byte{0xB0, 1, byte((i * 9) % 128)})
		}
		i++
	}
}

func runDrums(ctx context.Context, d *simRunning) {
	pattern := []struct {
		note byte
		vel  byte
	}{{36, 110}, {42, 60}, {38, 100}, {42, 60}}
	i := 0
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		p := pattern[i%len(pattern)]
		d.send([]byte{0x99, p.note, p.vel})
		d.send([]byte{0x89, p.note, 0})
		i++
	}
}

// synthEcho answers note-ons with a CC 74 value derived from the note, after a
// short delay, so the return path through the router lights up.
func synthEcho(d *simRunning, b []byte) {
	for i := 0; i+2 < len(b); i++ {
		if b[i]&0xF0 == 0x90 && b[i+2] != 0 {
			note := b[i+1]
			time.AfterFunc(40*time.Millisecond, func() {
				d.send([]byte{0xB0, 74, note & 0x7F})
			})
			return
		}
	}
}
