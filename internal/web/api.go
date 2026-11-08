package web

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

type portJSON struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Alias  string           `json:"alias"`
	Kind   router.PortKind  `json:"kind"`
	Group  string           `json:"group,omitempty"`
	Input  bool             `json:"input"`
	Output bool             `json:"output"`
	Detail string           `json:"detail,omitempty"`
	Online bool             `json:"online"`
	RTP    bool             `json:"rtp"`
	Hidden bool             `json:"hidden"`
	Stats  router.PortStats `json:"stats"`
}

type connJSON struct {
	From   string        `json:"from"`
	To     string        `json:"to"`
	Online bool          `json:"online"`
	Filter router.Filter `json:"filter"`
}

type stateBody struct {
	Version     string     `json:"version"`
	Hostname    string     `json:"hostname"`
	Now         int64      `json:"now"`
	Ports       []portJSON `json:"ports"`
	Connections []connJSON `json:"connections"`
}

func guessKind(id string) router.PortKind {
	switch {
	case strings.HasPrefix(id, "usb:"):
		return router.KindUSB
	case strings.HasPrefix(id, "rtp:"):
		return router.KindRTP
	case strings.HasPrefix(id, "ble:"):
		return router.KindBLE
	case strings.HasPrefix(id, "sim:"):
		return router.KindSim
	}
	return router.KindVirtual
}

// buildState merges the router's live view with persisted settings, and adds
// "remembered" ports for connections whose device is currently absent.
func (s *Server) buildState() stateBody {
	snap := s.d.Router.Snapshot()
	if s.testHookSnapshotTaken != nil {
		s.testHookSnapshotTaken()
	}
	st := s.d.Store.Snapshot()

	body := stateBody{
		Version: s.d.Version, Hostname: s.d.Hostname(), Now: time.Now().UnixMilli(),
		Ports:       make([]portJSON, 0, len(snap.Ports)),
		Connections: make([]connJSON, 0, len(snap.Connections)),
	}
	live := map[string]bool{}
	for _, p := range snap.Ports {
		live[p.ID] = true
		ps := st.Ports[p.ID]
		body.Ports = append(body.Ports, portJSON{
			ID: p.ID, Name: p.Name, Alias: ps.Alias, Kind: p.Kind, Group: p.Group,
			Input: p.Input, Output: p.Output, Detail: p.Detail, Online: true,
			RTP: p.Kind.IsDevice() && ps.RTPEnabled(), Hidden: ps.Hidden, Stats: p.PortStats,
		})
	}

	var ghosts []portJSON
	seen := map[string]bool{}
	addGhost := func(id string, wantIn, wantOut bool) {
		if live[id] || seen[id] {
			return
		}
		seen[id] = true
		g := portJSON{ID: id, Name: id, Kind: guessKind(id), Input: wantIn, Output: wantOut, Online: false}
		if k, ok := st.Known[id]; ok {
			g.Name, g.Kind, g.Group, g.Input, g.Output, g.Detail = k.Name, k.Kind, k.Group, k.Input, k.Output, k.Detail
		}
		ps := st.Ports[id]
		g.Alias, g.Hidden = ps.Alias, ps.Hidden
		g.RTP = g.Kind.IsDevice() && ps.RTPEnabled()
		ghosts = append(ghosts, g)
	}
	for _, c := range snap.Connections {
		addGhost(c.From, true, false)
		addGhost(c.To, false, true)
		body.Connections = append(body.Connections, connJSON{From: c.From, To: c.To, Online: c.Online, Filter: c.Filter})
	}
	sort.Slice(ghosts, func(i, j int) bool {
		if ghosts[i].Kind != ghosts[j].Kind {
			return ghosts[i].Kind < ghosts[j].Kind
		}
		return ghosts[i].Name < ghosts[j].Name
	})
	body.Ports = append(body.Ports, ghosts...)
	return body
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildState())
}

// ---- connections -----------------------------------------------------------

func (s *Server) handlePutConnection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From   string         `json:"from"`
		To     string         `json:"to"`
		Filter *router.Filter `json:"filter"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !validID(body.From) || !validID(body.To) {
		writeErr(w, http.StatusBadRequest, "from and to must be valid port IDs")
		return
	}
	// A connection may point at a port that is unplugged right now (it goes
	// dormant), but only at one the device has seen; otherwise a typo would be
	// persisted as a junk connection that never comes online.
	for _, id := range []string{body.From, body.To} {
		if !s.portKnown(id) {
			writeErr(w, http.StatusNotFound, "unknown port: "+id)
			return
		}
	}
	f := router.DefaultFilter()
	for _, c := range s.d.Router.Connections() {
		if c.From == body.From && c.To == body.To {
			f = c.Filter
		}
	}
	if body.Filter != nil {
		f = *body.Filter
	}
	if err := s.d.Router.Connect(body.From, body.To, f); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, router.ErrUnknownPort) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	for _, c := range s.d.Router.Snapshot().Connections {
		if c.From == body.From && c.To == body.To {
			writeJSON(w, http.StatusOK, connJSON{From: c.From, To: c.To, Online: c.Online, Filter: c.Filter})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if !validID(from) || !validID(to) {
		writeErr(w, http.StatusBadRequest, "from and to query parameters are required")
		return
	}
	if err := s.d.Router.Disconnect(from, to); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, router.ErrNoSuchConn) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- ports -----------------------------------------------------------------

// portKnown reports whether id is a port the device has a claim on: live in
// the router, remembered from an earlier run (Known, Ports), or a network port
// the user created (VirtualRTP). The last one matters right after
// POST /api/rtp/ports: the id has been handed out, but the endpoint only goes
// live when the reconciler has run, and until then the id is in no other list.
func (s *Server) portKnown(id string) bool {
	if s.d.Router.HasPort(id) {
		return true
	}
	known := false
	s.d.Store.View(func(st *config.State) {
		_, known = st.Known[id]
		if _, ok := st.Ports[id]; ok {
			known = true
		}
		for _, v := range st.VirtualRTP {
			if v.ID == id {
				known = true
				break
			}
		}
	})
	return known
}

func (s *Server) handlePatchPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) || !s.portKnown(id) {
		writeErr(w, http.StatusNotFound, "unknown port")
		return
	}
	var body struct {
		Alias  *string `json:"alias"`
		RTP    *bool   `json:"rtp"`
		Hidden *bool   `json:"hidden"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Alias != nil {
		a := strings.TrimSpace(*body.Alias)
		if len([]rune(a)) > 60 {
			writeErr(w, http.StatusBadRequest, "alias is too long (max 60 characters)")
			return
		}
		body.Alias = &a
	}
	if body.RTP != nil && !guessKind(id).IsDevice() {
		writeErr(w, http.StatusBadRequest, "only device ports can be exposed over the network")
		return
	}

	rtpOff := false
	s.d.Store.Update(func(st *config.State) {
		ps := st.Ports[id]
		if body.Alias != nil {
			ps.Alias = *body.Alias
		}
		if body.Hidden != nil {
			ps.Hidden = *body.Hidden
		}
		if body.RTP != nil {
			v := *body.RTP
			if !v && ps.RTPEnabled() {
				rtpOff = true
				ps.RTPDefaultsDone = false // re-enabling brings the defaults back
			}
			ps.RTP = &v
		}
		st.Ports[id] = ps
	})
	if rtpOff {
		// Drop the paired connections; the endpoint itself disappears on reconcile.
		pair := "rtp:" + id
		for _, c := range s.d.Router.Connections() {
			if c.From == pair || c.To == pair {
				_ = s.d.Router.Disconnect(c.From, c.To)
			}
		}
	}
	if s.d.Reconcile != nil {
		s.d.Reconcile()
	}
	s.NotifyTopology()
	for _, p := range s.buildState().Ports {
		if p.ID == id {
			writeJSON(w, http.StatusOK, p)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePanic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.d.Router.Panic(id); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, router.ErrUnknownPort) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleTestNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.d.Router.Inject(id, []byte{0x90, 60, 100}); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, router.ErrUnknownPort) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = s.d.Router.Inject(id, []byte{0x80, 60, 0}) })
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
