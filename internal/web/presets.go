package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"emm/internal/config"
	"emm/internal/router"
)

// Presets are saved copies of the connection matrix (with every filter). The
// device's other settings (names, hidden flags, network exposure) are not part of
// a preset: they describe the devices, not the routing.

const (
	maxPresets    = 50
	maxPresetName = 40
)

type presetJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Saved       int64  `json:"saved"`
	Connections int    `json:"connections"`
}

func presetList(st *config.State) []presetJSON {
	out := make([]presetJSON, 0, len(st.Presets))
	for _, p := range st.Presets {
		out = append(out, presetJSON{ID: p.ID, Name: p.Name, Saved: p.Saved, Connections: len(p.Connections)})
	}
	return out
}

func (s *Server) presets() []presetJSON {
	var out []presetJSON
	s.d.Store.View(func(st *config.State) { out = presetList(st) })
	return out
}

// NotifyPresets pushes the preset list to every client.
func (s *Server) NotifyPresets() { s.hub.publish("presets", s.presets()) }

func (s *Server) handlePresetsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.presets())
}

// cleanPresetName trims a name and checks its length.
func cleanPresetName(n string) (string, bool) {
	n = strings.Join(strings.Fields(n), " ")
	return n, n != "" && utf8.RuneCountInString(n) <= maxPresetName
}

func sameName(a, b string) bool { return strings.EqualFold(a, b) }

// POST /api/presets {"name": "...", "replace": false}: saves the connections as
// they are now. A name that is taken is refused unless replace is true.
func (s *Server) handlePresetSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Replace bool   `json:"replace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name, ok := cleanPresetName(body.Name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "give the preset a name (max "+strconv.Itoa(maxPresetName)+" characters)")
		return
	}
	conns := s.d.Router.Connections()
	var (
		result   presetJSON
		conflict bool
		full     bool
	)
	s.d.Store.Update(func(st *config.State) {
		now := time.Now().Unix()
		for i := range st.Presets {
			if sameName(st.Presets[i].Name, name) {
				if !body.Replace {
					conflict = true
					return
				}
				st.Presets[i].Name, st.Presets[i].Saved, st.Presets[i].Connections = name, now, conns
				result = presetJSON{ID: st.Presets[i].ID, Name: name, Saved: now, Connections: len(conns)}
				return
			}
		}
		if len(st.Presets) >= maxPresets {
			full = true
			return
		}
		id := newPresetID(st, name)
		st.Presets = append(st.Presets, config.Preset{ID: id, Name: name, Saved: now, Connections: conns})
		result = presetJSON{ID: id, Name: name, Saved: now, Connections: len(conns)}
	})
	switch {
	case conflict:
		writeErr(w, http.StatusConflict, "a preset called “"+name+"” already exists")
		return
	case full:
		writeErr(w, http.StatusConflict, "too many presets (max "+strconv.Itoa(maxPresets)+"): delete one first")
		return
	}
	s.NotifyPresets()
	writeJSON(w, http.StatusOK, result)
}

func newPresetID(st *config.State, name string) string {
	base := "p-" + slug(name)
	if base == "p-" {
		base = "p-preset"
	}
	taken := map[string]bool{}
	for _, p := range st.Presets {
		taken[p.ID] = true
	}
	id := base
	for i := 2; taken[id]; i++ {
		id = base + "-" + strconv.Itoa(i)
	}
	return id
}

func (s *Server) findPreset(id string) (config.Preset, bool) {
	var out config.Preset
	found := false
	s.d.Store.View(func(st *config.State) {
		for _, p := range st.Presets {
			if p.ID == id {
				out, found = p, true
				out.Connections = append([]router.ConnSpec(nil), p.Connections...)
			}
		}
	})
	return out, found
}

type skippedConn struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// POST /api/presets/{id}/recall: replaces the current connections with the
// preset's. Connections to a device the unit has never seen are skipped (and
// reported); a connection to a known but unplugged device goes dormant, as usual.
func (s *Server) handlePresetRecall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pr, ok := s.findPreset(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such preset")
		return
	}
	want := map[string]bool{}
	var apply []router.ConnSpec
	skipped := []skippedConn{}
	for _, c := range pr.Connections {
		if !validID(c.From) || !validID(c.To) {
			continue
		}
		if !s.portKnown(c.From) || !s.portKnown(c.To) {
			skipped = append(skipped, skippedConn{c.From, c.To, "device not known to this unit any more"})
			continue
		}
		want[c.From+"\x00"+c.To] = true
		apply = append(apply, c)
	}
	// Remove what the preset does not have first, so a destination never gets
	// both the old and the new routing at once. Disconnect releases held notes.
	for _, c := range s.d.Router.Connections() {
		if !want[c.From+"\x00"+c.To] {
			_ = s.d.Router.Disconnect(c.From, c.To)
		}
	}
	applied := 0
	for _, c := range apply {
		if err := s.d.Router.Connect(c.From, c.To, c.Filter); err != nil {
			reason := err.Error()
			if errors.Is(err, router.ErrNotInput) || errors.Is(err, router.ErrNotOutput) {
				reason = "the device no longer has that direction"
			}
			skipped = append(skipped, skippedConn{c.From, c.To, reason})
			continue
		}
		applied++
	}
	if s.d.Reconcile != nil {
		s.d.Reconcile()
	}
	s.NotifyTopology()
	writeJSON(w, http.StatusOK, map[string]any{"name": pr.Name, "applied": applied, "skipped": skipped})
}

// PATCH /api/presets/{id} {"name": "..."}: renames.
func (s *Server) handlePresetRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name, ok := cleanPresetName(body.Name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "give the preset a name (max "+strconv.Itoa(maxPresetName)+" characters)")
		return
	}
	found, conflict := false, false
	s.d.Store.Update(func(st *config.State) {
		for _, p := range st.Presets {
			if p.ID != id && sameName(p.Name, name) {
				conflict = true
				return
			}
		}
		for i := range st.Presets {
			if st.Presets[i].ID == id {
				st.Presets[i].Name = name
				found = true
			}
		}
	})
	switch {
	case conflict:
		writeErr(w, http.StatusConflict, "a preset called “"+name+"” already exists")
		return
	case !found:
		writeErr(w, http.StatusNotFound, "no such preset")
		return
	}
	s.NotifyPresets()
	writeJSON(w, http.StatusOK, s.presets())
}

func (s *Server) handlePresetDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found := false
	s.d.Store.Update(func(st *config.State) {
		keep := st.Presets[:0]
		for _, p := range st.Presets {
			if p.ID == id {
				found = true
				continue
			}
			keep = append(keep, p)
		}
		st.Presets = keep
	})
	if !found {
		writeErr(w, http.StatusNotFound, "no such preset")
		return
	}
	s.NotifyPresets()
	writeJSON(w, http.StatusOK, s.presets())
}
