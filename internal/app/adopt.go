package app

import (
	"regexp"
	"sort"
	"strings"

	"emm/internal/config"
	"emm/internal/router"
)

// Moving a USB device to another hub port.
//
// A USB port ID is usb:<vid>:<pid>:<serial-or-usb-path>:<cable> (see the device
// package). A device that reports no usable serial number is identified by the
// hub port it sits in, so plugging it into another port makes a new device: its
// routing is gone, and the old entry stays behind as an offline duplicate.
//
// adoptMoved undoes that where it is unambiguous: if exactly one device of a
// model is attached and exactly one path-identified device of the same model
// (same vendor and product, same cables) is remembered but absent, they are the
// same device, so the old entry's connections, name and network port move to the
// new IDs. With two units of a model attached, or several absent candidates,
// nothing is guessed: the offline entries stay until the user forgets them.

// usbID is a parsed USB port ID.
type usbID struct{ vid, pid, inst, cable string }

// unit is the part of the ID that names one physical device (everything but the cable).
func (u usbID) unit() string { return "usb:" + u.vid + ":" + u.pid + ":" + u.inst }

// model is what identical devices share.
func (u usbID) model() string { return u.vid + ":" + u.pid }

func (u usbID) id() string { return u.unit() + ":" + u.cable }

// kernelUSBPath matches a USB device's place in the bus tree as the kernel
// names it: "<bus>-<port>[.<port>...]", e.g. "1-1.2.3". A serial number does not look like that.
var kernelUSBPath = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

// parsePathUSBID parses id when it is a USB port ID built from a USB path.
func parsePathUSBID(id string) (usbID, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 5 || parts[0] != "usb" || !kernelUSBPath.MatchString(parts[3]) {
		return usbID{}, false
	}
	return usbID{vid: parts[1], pid: parts[2], inst: parts[3], cable: parts[4]}, true
}

// usbUnit is one physical device and the cables seen on it.
type usbUnit struct {
	key    string
	model  string
	inst   string
	cables map[string]bool
	ids    []string
}

func (u *usbUnit) add(p usbID) {
	if u.cables == nil {
		u.cables = map[string]bool{}
		u.key, u.model, u.inst = p.unit(), p.model(), p.inst
	}
	u.cables[p.cable] = true
	u.ids = append(u.ids, p.id())
}

func sameCables(a, b *usbUnit) bool {
	if len(a.cables) != len(b.cables) {
		return false
	}
	for c := range a.cables {
		if !b.cables[c] {
			return false
		}
	}
	return true
}

// twinPorts returns the live ports that belong to a path-identified device
// while another such device of the same model is attached too.
func twinPorts(snap router.Snapshot) map[string]bool {
	units := map[string]map[string]bool{} // model -> unit keys
	for _, p := range snap.Ports {
		if id, ok := parsePathUSBID(p.ID); ok {
			if units[id.model()] == nil {
				units[id.model()] = map[string]bool{}
			}
			units[id.model()][id.unit()] = true
		}
	}
	out := map[string]bool{}
	for _, p := range snap.Ports {
		if id, ok := parsePathUSBID(p.ID); ok && len(units[id.model()]) > 1 {
			out[p.ID] = true
		}
	}
	return out
}

// adoptMoved carries a moved device's settings over to its new IDs. It reports
// whether it changed anything.
func (r *Reconciler) adoptMoved() bool {
	snap := r.Router.Snapshot()
	liveUnits := map[string]*usbUnit{}
	liveIDs := map[string]bool{}
	for _, p := range snap.Ports {
		liveIDs[p.ID] = true
		if id, ok := parsePathUSBID(p.ID); ok {
			u := liveUnits[id.unit()]
			if u == nil {
				u = &usbUnit{}
				liveUnits[id.unit()] = u
			}
			u.add(id)
		}
	}
	if len(liveUnits) == 0 {
		return false
	}

	// Absent devices that are remembered: by Known, by saved settings and by
	// connections that still name them.
	absent := map[string]*usbUnit{}
	note := func(id string) {
		id = strings.TrimPrefix(id, "rtp:")
		if liveIDs[id] {
			return
		}
		p, ok := parsePathUSBID(id)
		if !ok || liveUnits[p.unit()] != nil {
			return // not path-identified, or the device is attached (under other cables)
		}
		u := absent[p.unit()]
		if u == nil {
			u = &usbUnit{}
			absent[p.unit()] = u
		}
		if !u.cables[p.cable] {
			u.add(p)
		}
	}
	twin := map[string]bool{} // absent units that were once attached together with an identical one
	r.Store.View(func(st *config.State) {
		for id, k := range st.Known {
			if strings.HasPrefix(id, "rtp:") {
				continue // the network endpoint of a port: the port's own entry says it all
			}
			note(id)
			if p, ok := parsePathUSBID(id); ok && k.Twin {
				twin[p.unit()] = true
			}
		}
		for id := range st.Ports {
			note(id)
		}
	})
	for _, c := range snap.Connections {
		note(c.From)
		note(c.To)
	}
	if len(absent) == 0 {
		return false
	}

	byModel := map[string][]*usbUnit{}
	for _, u := range liveUnits {
		byModel[u.model] = append(byModel[u.model], u)
	}
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	changed := false
	for _, m := range models {
		if len(byModel[m]) != 1 {
			continue // two identical devices are attached: cannot tell whose settings are whose
		}
		live := byModel[m][0]
		var cands []*usbUnit
		for _, u := range absent {
			if u.model == m && !twin[u.key] && sameCables(u, live) {
				cands = append(cands, u)
			}
		}
		if len(cands) != 1 {
			continue // none, or several: leave them for the user to forget
		}
		r.moveUnit(cands[0], live)
		changed = true
	}
	return changed
}

// moveUnit renames every ID of the absent unit old to the matching ID of the
// attached unit cur: connections (also those of the device's network endpoint),
// per-port settings and the remembered entry.
func (r *Reconciler) moveUnit(old, cur *usbUnit) {
	rename := map[string]string{}
	for c := range old.cables {
		rename[old.key+":"+c] = cur.key + ":" + c
	}
	remap := func(id string) string {
		if n, ok := rename[id]; ok {
			return n
		}
		if dev, ok := strings.CutPrefix(id, "rtp:"); ok {
			if n, ok := rename[dev]; ok {
				return "rtp:" + n
			}
		}
		return id
	}

	r.logger().Info("a USB device without a serial number moved to another port: keeping its connections and settings",
		"from", old.inst, "to", cur.inst, "ports", len(old.cables))

	// Connections first, so the old network endpoint is no longer referenced when
	// the next reconcile pass removes it.
	for _, c := range r.Router.Connections() {
		from, to := remap(c.From), remap(c.To)
		if from == c.From && to == c.To {
			continue
		}
		_ = r.Router.Disconnect(c.From, c.To)
		exists := false
		for _, e := range r.Router.Connections() {
			if e.From == from && e.To == to {
				exists = true
				break
			}
		}
		if exists {
			continue // the new ID already has this connection; its filter wins
		}
		if err := r.Router.Connect(from, to, c.Filter); err != nil {
			r.logger().Warn("could not carry a connection over to the device's new port", "from", from, "to", to, "err", err)
		}
	}

	r.Store.Update(func(st *config.State) {
		// Saved presets name the device too: recalling one must not bring the old,
		// offline ID back.
		for i := range st.Presets {
			conns := st.Presets[i].Connections
			seen := map[string]bool{}
			out := conns[:0:0]
			for _, c := range conns {
				c.From, c.To = remap(c.From), remap(c.To)
				k := c.From + "\x00" + c.To
				if c.From == c.To || seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, c)
			}
			st.Presets[i].Connections = out
		}
		for oldID, newID := range rename {
			ps, had := st.Ports[oldID]
			if had {
				dst, ok := st.Ports[newID]
				switch {
				case !ok || dst == (config.PortSettings{}):
					dst = ps
				default:
					if dst.Alias == "" {
						dst.Alias = ps.Alias
					}
					if dst.RTP == nil {
						dst.RTP = ps.RTP
					}
					// The network endpoint keeps its UDP port, so a DAW's saved session
					// still finds it. (If the old endpoint has not released the port yet, the
					// reconciler's bind-failure retry picks one.)
					if dst.RTPPort == 0 {
						dst.RTPPort = ps.RTPPort
					}
					dst.RTPDefaultsDone = dst.RTPDefaultsDone || ps.RTPDefaultsDone
				}
				st.Ports[newID] = dst
				delete(st.Ports, oldID)
			}
			delete(st.Known, oldID)
			delete(st.Known, "rtp:"+oldID)
		}
	})
}
