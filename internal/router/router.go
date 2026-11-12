package router

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/midi"
)

// Options configures a Router. The zero value is usable.
type Options struct {
	// SysExChunk is the largest SysEx piece that is moved as one unit
	// (default 256 bytes). Smaller chunks mean tighter interleaving of
	// realtime messages during a dump; larger ones mean fewer writes.
	SysExChunk int
	// QueueBytes bounds the backlog per destination (default 4 MiB).
	QueueBytes int
	// OnConnectionsChanged is called after the set of connections or any
	// filter changes, so it can be persisted. Calls never overlap and are
	// made in order: each one gets the connections as they are at that
	// moment, so the last call always carries the latest state. It runs
	// outside the router's own locks but must not call back into the
	// router's Connect, Disconnect or SetFilter.
	OnConnectionsChanged func([]ConnSpec)
	Logger               *slog.Logger
}

// Router moves MIDI between ports according to its connections.
type Router struct {
	opts Options
	log  *slog.Logger

	mu    sync.RWMutex
	ports map[string]*portState
	conns map[string]*conn // key: from + "\x00" + to

	changeMu sync.Mutex
	changeCh chan struct{}

	// persistMu serialises OnConnectionsChanged (see afterConnChange).
	persistMu sync.Mutex

	monitorOn   atomic.Int32
	monitorMu   sync.Mutex
	monitorSubs map[int]chan MonitorEntry
	monitorNext int
}

type portState struct {
	info   PortInfo
	port   Port
	parser *midi.Parser
	out    *outQueue // nil when the port cannot receive MIDI

	routes atomic.Pointer[[]*conn] // connections whose source is this port
	tap    tapSet                  // listeners for the SysEx this port receives (console.go)

	rxMsgs, rxBytes, txMsgs, txBytes, dropped, errors atomic.Uint64
	lastRx, lastTx                                    atomic.Int64 // unix nanos
	removed                                           atomic.Bool
}

type conn struct {
	spec ConnSpec // guarded by Router.mu
	cf   atomic.Pointer[compiledFilter]
	dst  atomic.Pointer[portState]

	// mu makes "filter, enqueue, track the note" one step for every event that
	// is not SysEx (their enqueue never blocks), and the release of the held
	// notes (Disconnect, RemovePort, a filter change) one step the other way,
	// so a note-on can never reach the destination without the tracker knowing
	// about it. SysEx chunks only take it around the decisions: their enqueue
	// may wait for queue space and must not hold it. Lock order: Router.mu,
	// then conn.mu, then the queue's mutex.
	mu     sync.Mutex
	notes  noteTracker
	sxDrop bool
	dead   bool // removed by Disconnect: nothing is forwarded any more
}

func connKey(from, to string) string { return from + "\x00" + to }

// New creates a Router.
func New(opts Options) *Router {
	if opts.SysExChunk <= 0 {
		opts.SysExChunk = midi.DefaultSysExChunk
	}
	if opts.QueueBytes <= 0 {
		opts.QueueBytes = 4 << 20
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Router{
		opts: opts, log: log.With("component", "router"),
		ports:       map[string]*portState{},
		conns:       map[string]*conn{},
		changeCh:    make(chan struct{}),
		monitorSubs: map[int]chan MonitorEntry{},
	}
}

// Handle is how a port feeds received bytes into the router.
type Handle struct {
	r  *Router
	ps *portState
}

// ID returns the port ID this handle belongs to.
func (h *Handle) ID() string { return h.ps.info.ID }

// Receive parses raw MIDI bytes received on the port and routes them.
// It is not safe for concurrent use; serialise calls per port. It may block
// (back-pressure) while a SysEx is being forwarded to a slow destination.
func (h *Handle) Receive(data []byte) {
	if h.ps.removed.Load() || !h.ps.info.Input {
		return
	}
	h.ps.parser.Feed(data, func(ev midi.Event) { h.r.dispatch(h.ps, ev) })
}

// AddPort implements Host.
func (r *Router) AddPort(p Port) *Handle {
	info := p.Info()
	ps := &portState{info: info, port: p, parser: midi.NewParser()}
	ps.parser.MaxChunk = r.opts.SysExChunk
	if info.Output {
		ps.out = newOutQueue(r, ps, r.opts.QueueBytes)
	}

	r.mu.Lock()
	old := r.ports[info.ID]
	r.ports[info.ID] = ps
	r.rebuildLocked()
	r.mu.Unlock()

	if old != nil {
		r.retire(old)
	}
	r.log.Info("port added", "id", info.ID, "name", info.Name, "kind", info.Kind,
		"in", info.Input, "out", info.Output)
	r.notifyChange()
	return &Handle{r: r, ps: ps}
}

// RemovePort implements Host.
func (r *Router) RemovePort(id string) {
	r.mu.Lock()
	ps := r.ports[id]
	if ps == nil {
		r.mu.Unlock()
		return
	}
	delete(r.ports, id)
	// From here on dispatch routes nothing more from this port (it checks under
	// each connection's mutex), so the release below is final even if a Receive
	// is still working through a buffer.
	ps.removed.Store(true)
	// Release anything this port was holding on its destinations.
	var release []*conn
	for _, c := range r.conns {
		if c.spec.From == id {
			release = append(release, c)
		}
	}
	r.rebuildLocked()
	r.mu.Unlock()

	for _, c := range release {
		if dst := c.dst.Load(); dst != nil && dst.out != nil {
			c.release(dst, id, false)
		}
	}
	r.retire(ps)
	r.log.Info("port removed", "id", id)
	r.notifyChange()
}

// retire stops a port's queue and closes the port.
func (r *Router) retire(ps *portState) {
	ps.removed.Store(true)
	ps.tap.closeAll()
	if ps.out != nil {
		ps.out.close()
		select {
		case <-ps.out.done:
		case <-time.After(2 * time.Second):
			r.log.Warn("writer slow to stop; closing port anyway", "id", ps.info.ID)
		}
	}
	if err := ps.port.Close(); err != nil {
		r.log.Debug("port close", "id", ps.info.ID, "err", err)
	}
}

// rebuildLocked re-resolves every connection's endpoints. r.mu must be held for writing.
func (r *Router) rebuildLocked() {
	bySrc := map[string][]*conn{}
	for _, c := range r.conns {
		c.dst.Store(r.ports[c.spec.To])
		bySrc[c.spec.From] = append(bySrc[c.spec.From], c)
	}
	for id, ps := range r.ports {
		list := bySrc[id]
		sort.Slice(list, func(i, j int) bool { return list[i].spec.To < list[j].spec.To })
		ps.routes.Store(&list)
	}
}

// release gives back what the connection holds on dst: the source's unfinished
// SysEx (the destination is told the source is gone) and every note still
// sounding. With kill set the connection is also marked dead, so a dispatch
// that still holds it from before it was removed forwards nothing more.
func (c *conn) release(dst *portState, from string, kill bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kill {
		c.dead = true
	}
	// Close the SysEx first: note-offs from an owner would otherwise be
	// written into the middle of its own open message.
	dst.out.sourceGone(from)
	for _, d := range c.notes.release() {
		// The tracker has forgotten the note by now, so this note-off must not
		// be dropped for lack of queue space.
		dst.out.enqueueRelease(from, midi.Event{Data: d})
	}
}

// swapFilter installs a new filter on a live connection. If the change could
// strand a note the connection holds on dst (the matching note-off would come
// out different, or not at all), the held notes are released in the same
// critical section as the swap: no event can be filtered by the new filter
// before the destination has been told to let go of what the old one let
// through. A change that cannot affect a note-off (blocking clock, say) leaves
// held notes alone. Only notes are tracked: a sustain pedal that is down stays
// down on the destination through any filter change (or Disconnect), and
// Panic is what lets go of it.
//
// The source is reported gone to the destination (which closes its open SysEx
// message with an F7 at once) whenever the swap releases notes, because the
// note-offs would otherwise be written into the middle of the source's own
// message, and when the new filter starts blocking SysEx, because the rest of
// the message would never arrive and the destination would stay owned by it.
func (c *conn) swapFilter(nf *compiledFilter, from string) {
	dst := c.dst.Load()
	if dst != nil && dst.out == nil {
		dst = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.cf.Swap(nf)
	if old == nil {
		return
	}
	var offs [][]byte
	if old.notesDiffer(nf) {
		offs = c.notes.release() // also forgets notes of a destination that is not there
	}
	if dst == nil {
		return
	}
	if len(offs) > 0 || (nf.blocksSysEx() && !old.blocksSysEx()) {
		dst.out.sourceGone(from)
	}
	for _, d := range offs {
		dst.out.enqueueRelease(from, midi.Event{Data: d})
	}
}

func (r *Router) dispatch(src *portState, ev midi.Event) {
	n := uint64(len(ev.Data))
	src.rxBytes.Add(n)
	if !ev.SysEx || ev.First {
		src.rxMsgs.Add(1)
	}
	src.lastRx.Store(time.Now().UnixNano())
	if r.monitorOn.Load() > 0 {
		r.emitMonitor(src.info.ID, "rx", "", ev)
	}
	if ev.SysEx && src.tap.n.Load() > 0 {
		src.tap.event(ev)
	}
	rs := src.routes.Load()
	if rs == nil {
		return
	}
	for _, c := range *rs {
		dst := c.dst.Load()
		if dst == nil || dst.out == nil {
			continue
		}
		if ev.SysEx {
			c.forwardSysEx(src, dst, ev)
		} else {
			c.forward(src, dst, ev)
		}
	}
}

// forward passes on an event that is not SysEx. Filtering, enqueueing and
// tracking the note are one critical section (the enqueue never blocks: a full
// queue drops the event), so a release can't run in between and miss it, and
// nothing is forwarded once the connection or the source is gone.
func (c *conn) forward(src, dst *portState, ev midi.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead || src.removed.Load() {
		return
	}
	out, ok := c.cf.Load().apply(ev)
	if !ok {
		return
	}
	if dst.out.enqueue(src.info.ID, out) {
		c.notes.observe(out)
	}
}

// forwardSysEx passes on one SysEx chunk. The enqueue may wait up to
// sysexEnqueueWait for room, so c.mu is not held across it; if the chunk is
// refused, the rest of the message is dropped with it (sxDrop).
func (c *conn) forwardSysEx(src, dst *portState, ev midi.Event) {
	c.mu.Lock()
	if c.dead || src.removed.Load() {
		c.mu.Unlock()
		return
	}
	out, ok := c.cf.Load().apply(ev)
	if !ok {
		c.mu.Unlock()
		return
	}
	if ev.First {
		c.sxDrop = false // a new message: nothing is left of an earlier one
	}
	skip := c.sxDrop
	if skip && ev.Last {
		c.sxDrop = false
	}
	c.mu.Unlock()
	if skip {
		return
	}

	accepted := dst.out.enqueue(src.info.ID, out)

	c.mu.Lock()
	if !accepted {
		c.sxDrop = !ev.Last
	} else if ev.Last {
		c.sxDrop = false
	}
	c.mu.Unlock()
}

// ---- connections -----------------------------------------------------------

var (
	ErrUnknownPort = errors.New("unknown port")
	ErrNotInput    = errors.New("source port cannot send MIDI")
	ErrNotOutput   = errors.New("destination port cannot receive MIDI")
	ErrSamePort    = errors.New("source and destination are the same port")
	ErrNoSuchConn  = errors.New("no such connection")
)

// Connect creates the connection from → to, or replaces its filter if it exists.
// A port that is not currently present is allowed (the connection is dormant).
func (r *Router) Connect(from, to string, f Filter) error {
	if from == to {
		return ErrSamePort
	}
	if err := f.Validate(); err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	f = f.Normalize()
	cf, _ := f.compile()
	r.mu.Lock()
	if p := r.ports[from]; p != nil && !p.info.Input {
		r.mu.Unlock()
		return ErrNotInput
	}
	if p := r.ports[to]; p != nil && !p.info.Output {
		r.mu.Unlock()
		return ErrNotOutput
	}
	key := connKey(from, to)
	c := r.conns[key]
	if c == nil {
		c = &conn{spec: ConnSpec{From: from, To: to}}
		c.cf.Store(&cf)
		r.conns[key] = c
	} else {
		c.swapFilter(&cf, from)
	}
	c.spec.Filter = f
	r.rebuildLocked()
	r.mu.Unlock()

	r.afterConnChange()
	return nil
}

// Disconnect removes the connection from → to.
func (r *Router) Disconnect(from, to string) error {
	r.mu.Lock()
	key := connKey(from, to)
	c := r.conns[key]
	if c == nil {
		r.mu.Unlock()
		return ErrNoSuchConn
	}
	dst := c.dst.Load()
	delete(r.conns, key)
	r.rebuildLocked()
	r.mu.Unlock()

	if dst != nil && dst.out != nil {
		c.release(dst, from, true)
	} else {
		c.mu.Lock()
		c.dead = true
		c.mu.Unlock()
	}
	r.afterConnChange()
	return nil
}

// Load installs persisted connections (replacing all current ones) without
// requiring their ports to exist yet. Invalid entries are skipped.
func (r *Router) Load(specs []ConnSpec) {
	r.mu.Lock()
	r.conns = map[string]*conn{}
	for _, s := range specs {
		if s.From == "" || s.To == "" || s.From == s.To {
			continue
		}
		if err := s.Filter.Validate(); err != nil {
			r.log.Warn("skipping connection with bad filter", "from", s.From, "to", s.To, "err", err)
			continue
		}
		s.Filter = s.Filter.Normalize()
		cf, _ := s.Filter.compile()
		c := &conn{spec: s}
		c.cf.Store(&cf)
		r.conns[connKey(s.From, s.To)] = c
	}
	r.rebuildLocked()
	r.mu.Unlock()
	r.notifyChange()
}

// Connections returns the current connection specs, sorted.
func (r *Router) Connections() []ConnSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.specsLocked()
}

func (r *Router) specsLocked() []ConnSpec {
	out := make([]ConnSpec, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c.spec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// afterConnChange hands the current connections to OnConnectionsChanged. The
// snapshot is taken inside persistMu, not before it: whoever delivers last has
// looked at the router last, so a slow earlier call can never overwrite newer
// state with an older list.
func (r *Router) afterConnChange() {
	if cb := r.opts.OnConnectionsChanged; cb != nil {
		r.persistMu.Lock()
		cb(r.Connections())
		r.persistMu.Unlock()
	}
	r.notifyChange()
}

// ---- change notification ---------------------------------------------------

// Changes returns a channel that is closed the next time the topology
// (ports or connections) changes. Fetch a fresh one after each wake-up.
func (r *Router) Changes() <-chan struct{} {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	return r.changeCh
}

func (r *Router) notifyChange() {
	r.changeMu.Lock()
	close(r.changeCh)
	r.changeCh = make(chan struct{})
	r.changeMu.Unlock()
}

// ---- snapshot --------------------------------------------------------------

// PortStats are the live counters of a port.
type PortStats struct {
	RxMsgs      uint64 `json:"rx_msgs"`
	RxBytes     uint64 `json:"rx_bytes"`
	TxMsgs      uint64 `json:"tx_msgs"`
	TxBytes     uint64 `json:"tx_bytes"`
	Dropped     uint64 `json:"dropped"`
	Errors      uint64 `json:"errors"`
	QueuedBytes int    `json:"queued_bytes"`
	LastRx      int64  `json:"last_rx"` // unix millis, 0 = never
	LastTx      int64  `json:"last_tx"`
}

// PortState is a live port plus its counters.
type PortState struct {
	PortInfo
	PortStats
}

// ConnState is a connection plus whether both ends are currently present.
type ConnState struct {
	ConnSpec
	Online bool `json:"online"`
}

// Snapshot is a consistent view of the router.
type Snapshot struct {
	Ports       []PortState `json:"ports"`
	Connections []ConnState `json:"connections"`
}

func (ps *portState) stats() PortStats {
	s := PortStats{
		RxMsgs: ps.rxMsgs.Load(), RxBytes: ps.rxBytes.Load(),
		TxMsgs: ps.txMsgs.Load(), TxBytes: ps.txBytes.Load(),
		Dropped: ps.dropped.Load(), Errors: ps.errors.Load(),
		LastRx: ps.lastRx.Load() / 1e6, LastTx: ps.lastTx.Load() / 1e6,
	}
	if ps.out != nil {
		s.QueuedBytes = ps.out.queuedBytes()
	}
	return s
}

// Snapshot returns the live ports (sorted) and all connections.
func (r *Router) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := Snapshot{Ports: make([]PortState, 0, len(r.ports)), Connections: make([]ConnState, 0, len(r.conns))}
	for _, ps := range r.ports {
		snap.Ports = append(snap.Ports, PortState{PortInfo: ps.info, PortStats: ps.stats()})
	}
	sort.Slice(snap.Ports, func(i, j int) bool {
		a, b := snap.Ports[i], snap.Ports[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	for _, s := range r.specsLocked() {
		snap.Connections = append(snap.Connections, ConnState{
			ConnSpec: s,
			Online:   r.ports[s.From] != nil && r.ports[s.To] != nil,
		})
	}
	return snap
}

// Stats returns only the counters of every live port, keyed by ID.
// It is cheaper than Snapshot and meant for frequent polling.
func (r *Router) Stats() map[string]PortStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]PortStats, len(r.ports))
	for id, ps := range r.ports {
		out[id] = ps.stats()
	}
	return out
}

// HasPort reports whether a port is currently present.
func (r *Router) HasPort(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ports[id]
	return ok
}

// ---- injection -------------------------------------------------------------

// Inject sends raw MIDI to a destination port as if from a source called
// "ui" (used for the panic button and test notes).
func (r *Router) Inject(to string, data []byte) error {
	r.mu.RLock()
	ps := r.ports[to]
	r.mu.RUnlock()
	if ps == nil {
		return ErrUnknownPort
	}
	if ps.out == nil {
		return ErrNotOutput
	}
	p := midi.NewParser()
	p.MaxChunk = r.opts.SysExChunk
	refused := false
	p.Feed(data, func(ev midi.Event) {
		if !ps.out.enqueue("ui", ev) {
			refused = true
		}
	})
	if refused {
		return ErrQueueFull
	}
	return nil
}

// Panic sends All Sound Off, All Notes Off and note-offs-by-CC on every
// channel to the destination.
func (r *Router) Panic(to string) error {
	var buf []byte
	for ch := byte(0); ch < 16; ch++ {
		buf = append(buf, 0xB0|ch, 120, 0, 0xB0|ch, 123, 0, 0xB0|ch, 64, 0)
	}
	return r.Inject(to, buf)
}

// ---- monitor ---------------------------------------------------------------

// MonitorEntry is one line of the live MIDI monitor.
type MonitorEntry struct {
	T    time.Time `json:"t"`
	Port string    `json:"port"`
	Dir  string    `json:"dir"`            // "rx" (received) or "tx" (sent)
	From string    `json:"from,omitempty"` // for tx: the source it came from
	Desc string    `json:"desc"`
	Hex  string    `json:"hex"`
}

// SubscribeMonitor starts delivering monitor entries. Entries are produced
// only while at least one subscriber exists. The channel drops entries when
// the consumer is slow. Call cancel when done.
func (r *Router) SubscribeMonitor() (<-chan MonitorEntry, func()) {
	ch := make(chan MonitorEntry, 512)
	r.monitorMu.Lock()
	id := r.monitorNext
	r.monitorNext++
	r.monitorSubs[id] = ch
	r.monitorOn.Add(1)
	r.monitorMu.Unlock()
	return ch, func() {
		r.monitorMu.Lock()
		if _, ok := r.monitorSubs[id]; ok {
			delete(r.monitorSubs, id)
			r.monitorOn.Add(-1)
		}
		r.monitorMu.Unlock()
	}
}

func (r *Router) emitMonitor(port, dir, from string, ev midi.Event) {
	hex := ev.Data
	if len(hex) > 8 {
		hex = hex[:8]
	}
	e := MonitorEntry{T: time.Now(), Port: port, Dir: dir, From: from, Desc: midi.Describe(ev),
		Hex: fmt.Sprintf("% X", hex)}
	if len(ev.Data) > 8 {
		e.Hex += " …"
	}
	r.monitorMu.Lock()
	for _, ch := range r.monitorSubs {
		select {
		case ch <- e:
		default:
		}
	}
	r.monitorMu.Unlock()
}

// SetFilter replaces the filter of an existing connection.
func (r *Router) SetFilter(from, to string, f Filter) error {
	r.mu.RLock()
	_, ok := r.conns[connKey(from, to)]
	r.mu.RUnlock()
	if !ok {
		return ErrNoSuchConn
	}
	return r.Connect(from, to, f)
}
