package rtpmidi

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"emm/internal/router"
)

// Spec describes one network endpoint the Manager should run.
type Spec struct {
	// ID is the router port ID, e.g. "rtp:usb:1234:abcd:1.2:0".
	ID   string
	Name string
	// ControlPort is the UDP control port (data = +1). 0 means "don't care":
	// the first time an endpoint is created a free pair is chosen, and an
	// existing endpoint keeps whatever it has.
	ControlPort int
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	Host router.Host
	// Announcer publishes endpoints via mDNS; nil means NoopAnnouncer. The
	// Manager withdraws what it announced but does not Close the Announcer.
	Announcer Announcer
	BindAddr  string
	Journal   bool
	Logger    *slog.Logger
	// OnChange is called (without locks held) when the set of endpoints or any
	// endpoint's peers change.
	OnChange func()
}

// Status reports one running endpoint.
type Status struct {
	ID          string
	Name        string
	ControlPort int
	Peers       []PeerInfo
}

// Manager runs a Server per Spec and exposes each as a router port.
type Manager struct {
	o   ManagerOptions
	log *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
}

type entry struct {
	spec Spec // Name as applied; ControlPort is the port actually bound
	srv  *Server
	port atomic.Pointer[netPort]
}

// netPort is the router.Port in front of a Server.
type netPort struct {
	info   router.PortInfo
	srv    *Server
	closed atomic.Bool

	rxMu   sync.Mutex // several peers send concurrently; Handle.Receive is not reentrant
	handle *router.Handle
}

func (p *netPort) Info() router.PortInfo { return p.info }

func (p *netPort) Write(b []byte) error {
	if !p.closed.Load() {
		p.srv.Send(b)
	}
	return nil
}

// Close only retires this router port; the Manager owns the Server's lifetime
// (the router also closes a port that is merely being replaced).
func (p *netPort) Close() error { p.closed.Store(true); return nil }

func (p *netPort) receive(data []byte) {
	p.rxMu.Lock()
	if h := p.handle; h != nil && !p.closed.Load() {
		h.Receive(data)
	}
	p.rxMu.Unlock()
}

// NewManager creates a Manager. Nothing runs until Sync.
func NewManager(o ManagerOptions) *Manager {
	if o.Announcer == nil {
		o.Announcer = NoopAnnouncer{}
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Manager{o: o, log: log.With("component", "rtpmidi-manager"), entries: map[string]*entry{}}
}

func (m *Manager) changed() {
	if m.o.OnChange != nil {
		m.o.OnChange()
	}
}

func portInfo(s Spec, control int) router.PortInfo {
	return router.PortInfo{
		ID: s.ID, Name: s.Name, Kind: router.KindRTP, Input: true, Output: true,
		Detail: fmt.Sprintf("network · UDP %d", control),
	}
}

// Sync makes the running endpoints match specs. It is idempotent. A failure
// for one spec does not stop the others; the errors are joined.
func (m *Manager) Sync(specs []Spec) error {
	want := make(map[string]Spec, len(specs))
	for _, s := range specs {
		if s.ID != "" {
			want[s.ID] = s
		}
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var errs []error
	var stale []*Server
	changed := false

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("rtpmidi: manager closed")
	}
	for id, e := range m.entries {
		if _, ok := want[id]; !ok {
			stale = append(stale, m.removeLocked(id, e))
			changed = true
		}
	}
	for _, id := range ids {
		s := want[id]
		e := m.entries[id]
		switch {
		case e == nil:
			if err := m.addLocked(s); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
			}
			changed = true

		case s.ControlPort != 0 && s.ControlPort != e.spec.ControlPort:
			stale = append(stale, m.removeLocked(id, e))
			if err := m.addLocked(s); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
			}
			changed = true

		case s.Name != e.spec.Name:
			e.spec.Name = s.Name
			e.srv.SetName(s.Name)
			// Replacing the router port refreshes the name the UI shows.
			np := &netPort{info: portInfo(e.spec, e.spec.ControlPort), srv: e.srv}
			np.handle = m.o.Host.AddPort(np)
			e.port.Store(np)
			if err := m.o.Announcer.Announce(id, s.Name, e.spec.ControlPort); err != nil {
				errs = append(errs, fmt.Errorf("%s: announce: %w", id, err))
			}
			changed = true
		}
	}
	m.mu.Unlock()

	for _, srv := range stale {
		srv.Close()
	}
	if changed {
		m.changed()
	}
	return errors.Join(errs...)
}

// addLocked starts the endpoint for s. m.mu must be held.
func (m *Manager) addLocked(s Spec) error {
	e := &entry{spec: s}
	srv := NewServer(Config{
		Name: s.Name, ControlPort: s.ControlPort, BindAddr: m.o.BindAddr,
		Journal: m.o.Journal, ReleaseNotesOnLoss: true, Logger: m.log,
	}, Handler{
		OnMIDI: func(_ PeerInfo, data []byte) {
			if p := e.port.Load(); p != nil {
				p.receive(data)
			}
		},
		OnPeers: m.changed,
	})
	if err := srv.Start(); err != nil {
		return err
	}
	cp, _ := srv.Ports()
	e.srv = srv
	e.spec.ControlPort = cp
	np := &netPort{info: portInfo(e.spec, cp), srv: srv}
	np.handle = m.o.Host.AddPort(np)
	e.port.Store(np)
	m.entries[s.ID] = e

	var err error
	if aerr := m.o.Announcer.Announce(s.ID, s.Name, cp); aerr != nil {
		m.log.Warn("announce failed (endpoint still reachable by address)", "id", s.ID, "err", aerr)
		err = fmt.Errorf("announce: %w", aerr)
	}
	return err
}

// removeLocked unregisters the endpoint and returns its Server, to be closed
// after m.mu is released. m.mu must be held.
func (m *Manager) removeLocked(id string, e *entry) *Server {
	delete(m.entries, id)
	m.o.Host.RemovePort(id)
	m.o.Announcer.Withdraw(id)
	if p := e.port.Load(); p != nil {
		p.closed.Store(true)
	}
	return e.srv
}

// Status lists the running endpoints, sorted by ID.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	out := make([]Status, 0, len(m.entries))
	srvs := make([]*Server, 0, len(m.entries))
	for id, e := range m.entries {
		out = append(out, Status{ID: id, Name: e.spec.Name, ControlPort: e.spec.ControlPort})
		srvs = append(srvs, e.srv)
	}
	m.mu.Unlock()
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return out[idx[a]].ID < out[idx[b]].ID })
	sorted := make([]Status, len(out))
	for k, i := range idx {
		sorted[k] = out[i]
		sorted[k].Peers = srvs[i].Peers()
	}
	return sorted
}

// Close stops every endpoint and removes its router port and announcement.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var stale []*Server
	for id, e := range m.entries {
		stale = append(stale, m.removeLocked(id, e))
	}
	m.mu.Unlock()
	for _, srv := range stale {
		srv.Close()
	}
}
