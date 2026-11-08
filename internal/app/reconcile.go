// Package app holds the daemon's glue logic that is independent of any
// particular backend: keeping the persisted view of ports up to date, and
// deriving the set of RTP-MIDI network endpoints (and their default
// connections) from the ports that exist.
package app

import (
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

// RTPSpec is one network MIDI endpoint that should exist.
type RTPSpec struct {
	ID          string // router port ID, "rtp:" + device port ID, or "rtp:net-…"
	Name        string
	ControlPort int
}

// RTPSyncer creates, renames and removes network endpoints so they match specs.
// It must be idempotent: syncing an unchanged list must not touch the router.
type RTPSyncer interface {
	Sync(specs []RTPSpec) error
}

// BasePort is the first UDP control port handed to a network endpoint
// (5004 is the conventional RTP-MIDI port). Endpoints use an even/odd pair.
const BasePort = 5004

// maxAllocPort bounds the search for a free port pair: a pair is only handed
// out if both of its ports are below this.
const maxAllocPort = 65000

// Reconciler keeps persisted state and network endpoints consistent with the
// router's live ports.
type Reconciler struct {
	Router *router.Router
	Store  *config.Store
	RTP    RTPSyncer // nil: no network endpoints
	// RTPEnabled reports whether network MIDI is on (settings + emm.txt).
	RTPEnabled func() bool
	Log        *slog.Logger
	// OnChange is called after a reconcile that changed anything the UI shows.
	OnChange func()
	// PortFree reports whether a UDP control/data port pair can be bound.
	// Defaults to probing the real network stack.
	PortFree func(controlPort int) bool

	mu      sync.Mutex
	trigger chan struct{}
	retries atomic.Int32
	// allocWarned is set while "no free UDP port pair" has been logged and no
	// allocation has succeeded since, so a persistent shortage logs once.
	allocWarned atomic.Bool
}

// udpPairFree reports whether UDP ports p and p+1 can be bound right now.
func udpPairFree(p int) bool {
	for _, port := range []int{p, p + 1} {
		c, err := net.ListenPacket("udp", ":"+strconv.Itoa(port))
		if err != nil {
			return false
		}
		c.Close()
	}
	return true
}

// Trigger asks for a reconcile soon (coalesced).
func (r *Reconciler) Trigger() {
	r.mu.Lock()
	if r.trigger == nil {
		r.trigger = make(chan struct{}, 1)
	}
	ch := r.trigger
	r.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run reconciles whenever the router topology changes or Trigger is called,
// until done is closed.
func (r *Reconciler) Run(done <-chan struct{}) {
	r.mu.Lock()
	if r.trigger == nil {
		r.trigger = make(chan struct{}, 1)
	}
	trig := r.trigger
	r.mu.Unlock()
	// Re-arm the change channel *before* reconciling, so a topology change that
	// happens while a pass is running wakes the next iteration instead of
	// being lost (the pass itself adds network ports, which triggers one more,
	// idempotent, pass).
	changes := r.Router.Changes()
	r.Reconcile()
	for {
		select {
		case <-done:
			return
		case <-changes:
		case <-trig:
		}
		changes = r.Router.Changes()
		select { // debounce bursts (hub with many devices)
		case <-done:
			return
		case <-time.After(150 * time.Millisecond):
		}
		r.Reconcile()
	}
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func displayName(p router.PortInfo, st *config.State) string {
	if a := strings.TrimSpace(st.Ports[p.ID].Alias); a != "" {
		return a
	}
	return p.Name
}

// Reconcile runs one pass.
func (r *Reconciler) Reconcile() {
	changed := r.updateKnown()
	if r.reconcileRTP() {
		changed = true
	}
	if changed && r.OnChange != nil {
		r.OnChange()
	}
}

// updateKnown records what every live port looks like, so connections to it can
// still be shown by name while it is unplugged. It reports whether anything changed.
func (r *Reconciler) updateKnown() bool {
	snap := r.Router.Snapshot()
	now := time.Now().Unix()
	changed := false
	r.Store.Update(func(st *config.State) {
		for _, p := range snap.Ports {
			k := config.KnownPort{Name: p.Name, Kind: p.Kind, Group: p.Group, Input: p.Input, Output: p.Output,
				Detail: p.Detail, LastSeen: now}
			old, ok := st.Known[p.ID]
			if ok {
				same := old
				same.LastSeen = k.LastSeen
				if same == k && now-old.LastSeen < 3600 {
					continue // nothing to record; avoid rewriting the file for LastSeen alone
				}
			}
			st.Known[p.ID] = k
			if !ok || old.Name != k.Name || old.Kind != k.Kind || old.Input != k.Input || old.Output != k.Output {
				changed = true
			}
		}
		// Forget ports unseen for 90 days that nothing refers to. A route that
		// names a device's network endpoint ("rtp:<id>") refers to the device too:
		// the endpoint is built from the device's Known entry (see reconcileRTP).
		referenced := map[string]bool{}
		for _, c := range st.Connections {
			for _, e := range []string{c.From, c.To} {
				referenced[e] = true
				referenced[strings.TrimPrefix(e, "rtp:")] = true
			}
		}
		for id, k := range st.Known {
			if !referenced[id] && now-k.LastSeen > 90*24*3600 {
				delete(st.Known, id)
			}
		}
	})
	return changed
}

// noPortPair logs, once until an allocation succeeds again, that a network
// endpoint was skipped for lack of a free UDP port pair.
func (r *Reconciler) noPortPair(id string) {
	if r.allocWarned.CompareAndSwap(false, true) {
		r.logger().Warn("no free UDP port pair for a network MIDI endpoint: skipping it until one is free", "id", id)
	}
}

// reconcileRTP makes the network endpoints match the ports and settings, and
// creates the default connections for new ones. It reports whether it changed
// anything visible.
func (r *Reconciler) reconcileRTP() bool {
	if r.RTP == nil {
		return false
	}
	enabled := r.RTPEnabled == nil || r.RTPEnabled()
	snap := r.Router.Snapshot()

	live := map[string]router.PortInfo{}
	for _, p := range snap.Ports {
		live[p.ID] = p.PortInfo
	}
	referenced := map[string]bool{} // endpoints that some connection mentions
	for _, c := range snap.Connections {
		referenced[c.From], referenced[c.To] = true, true
	}

	var specs []RTPSpec
	changed := false
	if enabled {
		r.Store.Update(func(st *config.State) {
			used := map[int]bool{}
			for _, ps := range st.Ports {
				if ps.RTPPort != 0 {
					used[ps.RTPPort], used[ps.RTPPort+1] = true, true
				}
			}
			for _, v := range st.VirtualRTP {
				if v.Port != 0 {
					used[v.Port], used[v.Port+1] = true, true
				}
			}
			free := r.PortFree
			if free == nil {
				free = udpPairFree
			}
			// alloc returns the first free even/odd port pair, or 0 if there is none
			// below maxAllocPort. It runs under the store mutex, so it must end: with
			// every probe failing (out of file descriptors, UDP denied by a sandbox)
			// an unbounded search would hang the whole daemon. After the first
			// exhaustion the rest of the pass does not search again.
			exhausted := false
			alloc := func() int {
				for p := BasePort; !exhausted && p+1 < maxAllocPort; p += 2 {
					if !used[p] && !used[p+1] && free(p) {
						used[p], used[p+1] = true, true
						r.allocWarned.Store(false)
						return p
					}
				}
				exhausted = true
				return 0
			}

			// Device-bound endpoints: live USB ports, plus absent ones whose
			// endpoint is still referenced by a connection (so remote sessions
			// survive a hub glitch and dormant routes keep their endpoint).
			ids := make([]string, 0, len(st.Known))
			for id, k := range st.Known {
				if k.Kind.IsDevice() {
					ids = append(ids, id)
				}
			}
			for id, p := range live {
				if p.Kind.IsDevice() {
					if _, ok := st.Known[id]; !ok {
						ids = append(ids, id)
					}
				}
			}
			sort.Strings(ids)
			for _, id := range ids {
				ps := st.Ports[id]
				if !ps.RTPEnabled() {
					continue
				}
				_, isLive := live[id]
				if !isLive && !referenced["rtp:"+id] {
					continue
				}
				if ps.RTPPort == 0 {
					p := alloc()
					if p == 0 {
						r.noPortPair(id) // skip this endpoint; the next pass tries again
						continue
					}
					ps.RTPPort = p
					st.Ports[id] = ps
					changed = true
				}
				name := st.Known[id].Name
				if p, ok := live[id]; ok {
					name = displayName(p, st)
				} else if a := strings.TrimSpace(ps.Alias); a != "" {
					name = a
				}
				if name == "" {
					name = id
				}
				specs = append(specs, RTPSpec{ID: "rtp:" + id, Name: name, ControlPort: ps.RTPPort})
			}
			for i := range st.VirtualRTP {
				v := &st.VirtualRTP[i]
				if v.Port == 0 {
					p := alloc()
					if p == 0 {
						r.noPortPair(v.ID)
						continue
					}
					v.Port = p
					changed = true
				}
				specs = append(specs, RTPSpec{ID: v.ID, Name: v.Name, ControlPort: v.Port})
			}
		})
	}

	if err := r.RTP.Sync(specs); err != nil {
		r.logger().Warn("some network MIDI endpoints could not be started", "err", err)
	}
	// An endpoint that did not come up usually lost its UDP port to another
	// program. Forget its stored port so the next pass picks a free pair, and
	// try again shortly (a few times only).
	var failed []string
	for _, sp := range specs {
		if !r.Router.HasPort(sp.ID) {
			failed = append(failed, sp.ID)
		}
	}
	if len(failed) == 0 {
		r.retries.Store(0)
	} else if r.retries.Add(1) <= 5 {
		r.Store.Update(func(st *config.State) {
			for _, id := range failed {
				if dev := strings.TrimPrefix(id, "rtp:"); !strings.HasPrefix(dev, "net-") {
					ps := st.Ports[dev]
					ps.RTPPort = 0
					st.Ports[dev] = ps
					continue
				}
				for i := range st.VirtualRTP {
					if st.VirtualRTP[i].ID == id {
						st.VirtualRTP[i].Port = 0
					}
				}
			}
		})
		time.AfterFunc(2*time.Second, r.Trigger)
		changed = true
	}

	if !enabled {
		return changed
	}
	// Default connections: each device ↔ its own network endpoint, once.
	snap = r.Router.Snapshot()
	live = map[string]router.PortInfo{}
	for _, p := range snap.Ports {
		live[p.ID] = p.PortInfo
	}
	var todo []router.PortInfo
	r.Store.View(func(st *config.State) {
		for id, p := range live {
			if !p.Kind.IsDevice() {
				continue
			}
			ps := st.Ports[id]
			if ps.RTPEnabled() && !ps.RTPDefaultsDone {
				if _, ok := live["rtp:"+id]; ok {
					todo = append(todo, p)
				}
			}
		}
	})
	for _, p := range todo {
		pair := "rtp:" + p.ID
		ok := true
		if p.Input {
			ok = r.Router.Connect(p.ID, pair, router.DefaultFilter()) == nil && ok
		}
		if p.Output {
			ok = r.Router.Connect(pair, p.ID, router.DefaultFilter()) == nil && ok
		}
		if ok {
			r.Store.Update(func(st *config.State) {
				ps := st.Ports[p.ID]
				ps.RTPDefaultsDone = true
				st.Ports[p.ID] = ps
			})
			changed = true
		}
	}
	return changed
}
