package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"emm/internal/config"
	"emm/internal/router"
)

// Backup and restore of the application state: the routing, presets, device
// names and the other settings that live in state.json. The device's own
// network, web password and SSH settings (system.json) are never part of a
// backup and a restore cannot touch them: they are set up on the device, or copied
// to the card when it is flashed.

const (
	backupFormat   = "emm-backup"
	backupVersion  = 1
	maxBackupBytes = 8 << 20

	// Limits that keep a damaged or hostile file from filling the card or the
	// router. They are far above what a real unit has (a handful of devices and a
	// few hundred connections), and the connection budget covers the current
	// connections and every preset together.
	maxBackupConns    = 1000 // per list
	maxBackupBudget   = 4000 // connections in all lists together
	maxBackupPorts    = 500
	maxBackupVirtual  = 50
	maxBackupWarnings = 20
)

type backupFile struct {
	Format  string          `json:"format"`
	Version int             `json:"version"`
	Created string          `json:"created,omitempty"`
	Emmd    string          `json:"emmd,omitempty"`
	Host    string          `json:"host,omitempty"`
	State   json.RawMessage `json:"state"`
}

// GET /api/backup: the application state as a file to save.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	st := s.d.Store.Snapshot()
	st.Connections = s.d.Router.Connections() // the router is the truth; the stored copy follows it
	state, err := json.Marshal(&st)           // application state only: Web, SSH and Network are json:"-"
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not encode the backup")
		return
	}
	host := ""
	if s.d.Hostname != nil {
		host = s.d.Hostname()
	}
	now := time.Now()
	bf := backupFile{Format: backupFormat, Version: backupVersion, Created: now.UTC().Format(time.RFC3339), Emmd: s.d.Version, Host: host, State: state}
	out, err := json.MarshalIndent(&bf, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not encode the backup")
		return
	}
	name := "emm-backup-"
	if h := slug(host); h != "" {
		name += h + "-"
	}
	name += now.Format("20060102-1504") + ".json"
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(append(out, '\n'))
}

// sanitiseBackup removes what must not reach the router or the card from a
// parsed backup, and says what it removed. Connections are returned separately
// (they are applied through the router).
var (
	virtualRTPID = regexp.MustCompile(`^rtp:net-[a-z0-9-]{1,100}$`)
	portKinds    = map[router.PortKind]bool{router.KindUSB: true, router.KindRTP: true, router.KindBLE: true, router.KindVirtual: true, router.KindSim: true}
)

// backupID is validID, and the first character is a letter or digit: "." and ".."
// would be taken as path elements by the URL router and could never be addressed.
func backupID(id string) bool {
	if !validID(id) {
		return false
	}
	c := id[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// backupPort keeps a UDP port only if the allocator could have handed it out; any
// other value is dropped, so the endpoint gets a fresh one.
func backupPort(p int) int {
	if p < 1024 || p >= 65000 {
		return 0
	}
	return p
}

func sanitiseBackup(b *config.State) (conns []router.ConnSpec, warns []string) {
	dropped := map[string]int{}
	drop := func(what string) { dropped[what]++ }
	budget := maxBackupBudget

	cleanConns := func(in []router.ConnSpec, what string) []router.ConnSpec {
		var out []router.ConnSpec
		seen := map[string]bool{}
		for _, c := range in {
			k := c.From + "\x00" + c.To
			switch {
			case !backupID(c.From) || !backupID(c.To) || c.From == c.To:
				drop(what + " with an invalid port")
			case c.Filter.Validate() != nil:
				drop(what + " with an invalid filter")
			case seen[k]:
				drop("duplicate " + what)
			case len(out) >= maxBackupConns || budget <= 0:
				drop(what + " over the limit")
			default:
				seen[k] = true
				budget--
				c.Filter = c.Filter.Normalize()
				out = append(out, c)
			}
		}
		return out
	}
	conns = cleanConns(b.Connections, "connection")

	ports := map[string]config.PortSettings{}
	for id, ps := range b.Ports {
		if !backupID(id) || len(ports) >= maxBackupPorts {
			drop("port setting")
			continue
		}
		ps.Alias = truncate(strings.TrimSpace(ps.Alias), 60)
		ps.RTPPort = backupPort(ps.RTPPort)
		ports[id] = ps
	}
	b.Ports = ports

	known := map[string]config.KnownPort{}
	for id, k := range b.Known {
		if !backupID(id) || len(known) >= maxBackupPorts || !portKinds[k.Kind] {
			drop("remembered device")
			continue
		}
		k.Name = truncate(strings.TrimSpace(k.Name), 200)
		k.Group = truncate(k.Group, 200)
		k.Detail = truncate(k.Detail, 300)
		known[id] = k
	}
	b.Known = known

	var virt []config.VirtualRTP
	seenV := map[string]bool{}
	for _, v := range b.VirtualRTP {
		name := truncate(strings.TrimSpace(v.Name), 40)
		_, clash := b.Known[v.ID]
		_, clashP := b.Ports[v.ID]
		if !virtualRTPID.MatchString(v.ID) || name == "" || seenV[v.ID] || clash || clashP || len(virt) >= maxBackupVirtual {
			drop("network MIDI port")
			continue
		}
		seenV[v.ID] = true
		virt = append(virt, config.VirtualRTP{ID: v.ID, Name: name, Port: backupPort(v.Port)})
	}
	b.VirtualRTP = virt

	var presets []config.Preset
	names, ids := map[string]bool{}, map[string]bool{}
	for _, p := range b.Presets {
		name, ok := cleanPresetName(p.Name)
		lower := strings.ToLower(name)
		switch {
		case !ok || names[lower]:
			drop("preset with a missing or repeated name")
			continue
		case len(presets) >= maxPresets:
			drop("preset over the limit")
			continue
		}
		id := p.ID
		if !backupID(id) || ids[id] {
			id = "p-" + slug(name)
			if id == "p-" {
				id = "p-preset"
			}
			base := id
			for i := 2; ids[id]; i++ {
				id = fmt.Sprintf("%s-%d", base, i)
			}
		}
		names[lower], ids[id] = true, true
		presets = append(presets, config.Preset{ID: id, Name: name, Saved: p.Saved, Connections: cleanConns(p.Connections, "preset connection")})
	}
	b.Presets = presets

	b.BLE.Name = strings.TrimSpace(b.BLE.Name)
	if utf8.RuneCountInString(b.BLE.Name) > 20 {
		b.BLE.Name = truncate(b.BLE.Name, 20)
		drop("Bluetooth name that was too long")
	}

	for what, n := range dropped {
		if len(warns) < maxBackupWarnings {
			warns = append(warns, fmt.Sprintf("left out %d %s", n, what))
		}
	}
	return conns, warns
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// POST /api/restore: the body is a file from /api/backup. It replaces the
// application state with the backup's. The network, web password and SSH
// settings are left as they are, whatever the file says.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupBytes)
	var bf backupFile
	if err := json.NewDecoder(r.Body).Decode(&bf); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeErr(w, http.StatusRequestEntityTooLarge, "that file is too large to be an EMM backup")
		default:
			writeErr(w, http.StatusBadRequest, "that is not an EMM backup file (not valid JSON)")
		}
		return
	}
	if bf.Format != backupFormat {
		writeErr(w, http.StatusBadRequest, "that is not an EMM backup file")
		return
	}
	if bf.Version != backupVersion {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("this backup has format %d; this version of EMM reads format %d", bf.Version, backupVersion))
		return
	}
	b, err := config.ParseApplication(bf.State)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the backup cannot be read: "+err.Error())
		return
	}
	conns, warns := sanitiseBackup(&b)

	// From here on the device changes. Everything that could refuse has been checked.
	// One restore or preset recall at a time: two interleaved would leave connections
	// of both in the router.
	s.applyMu.Lock()
	s.d.Store.Update(func(st *config.State) { st.ApplyBackup(&b) })
	applied, skipped := s.applyConnections(conns)
	if s.d.ApplySettings != nil {
		s.d.ApplySettings()
	}
	if s.d.Reconcile != nil {
		s.d.Reconcile()
	}
	s.applyMu.Unlock()
	s.NotifyTopology()
	s.NotifyPresets()
	s.NotifySettings() // the Bluetooth name and the RTP and BLE switches may have changed

	// Do not report success for something that exists only in memory: write it now,
	// and say so if the card refuses.
	saveErr := ""
	if err := s.d.Store.Flush(); err != nil {
		saveErr = err.Error()
	}
	s.log.Info("application state restored from a backup", "connections", applied, "skipped", len(skipped), "presets", len(b.Presets), "backup_from", bf.Host, "created", bf.Created)
	writeJSON(w, http.StatusOK, map[string]any{
		"connections": applied,
		"skipped":     skipped,
		"presets":     len(b.Presets),
		"warnings":    append([]string{}, warns...),
		"saved":       saveErr == "",
		"save_error":  saveErr,
		"created":     bf.Created,
		"host":        bf.Host,
	})
}
