package web

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"emm/internal/config"
)

// ---- RTP-MIDI network ports --------------------------------------------------

func (s *Server) rtpList() []RTPEndpoint {
	if s.d.RTP == nil {
		return []RTPEndpoint{}
	}
	out := s.d.RTP()
	if out == nil {
		out = []RTPEndpoint{}
	}
	return out
}

func (s *Server) handleRTPList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.rtpList())
}

func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) handleRTPAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		writeErr(w, http.StatusBadRequest, "name is required (max 40 characters)")
		return
	}
	base := "rtp:net-" + slug(name)
	if base == "rtp:net-" {
		base = "rtp:net-port"
	}
	var id string
	s.d.Store.Update(func(st *config.State) {
		taken := map[string]bool{}
		for _, v := range st.VirtualRTP {
			taken[v.ID] = true
		}
		id = base
		for i := 2; taken[id]; i++ {
			id = base + "-" + strconv.Itoa(i)
		}
		st.VirtualRTP = append(st.VirtualRTP, config.VirtualRTP{ID: id, Name: name})
	})
	if s.d.Reconcile != nil {
		s.d.Reconcile()
	}
	s.NotifyTopology()
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": name})
}

func (s *Server) handleRTPDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found := false
	s.d.Store.Update(func(st *config.State) {
		keep := st.VirtualRTP[:0]
		for _, v := range st.VirtualRTP {
			if v.ID == id {
				found = true
				continue
			}
			keep = append(keep, v)
		}
		st.VirtualRTP = keep
	})
	if !found {
		writeErr(w, http.StatusNotFound, "no such network port (device ports are removed by turning off network exposure on the device)")
		return
	}
	for _, c := range s.d.Router.Connections() {
		if c.From == id || c.To == id {
			_ = s.d.Router.Disconnect(c.From, c.To)
		}
	}
	if s.d.Reconcile != nil {
		s.d.Reconcile()
	}
	s.NotifyTopology()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- settings ------------------------------------------------------------------

type settingsBody struct {
	RTP struct {
		Enabled bool `json:"enabled"`
	} `json:"rtp"`
	BLE struct {
		Enabled     bool   `json:"enabled"`
		Name        string `json:"name"`
		Available   bool   `json:"available"`
		Advertising bool   `json:"advertising"`
		Connected   int    `json:"connected"`
		Error       string `json:"error"`
	} `json:"ble"`
	Web struct {
		PasswordSet bool `json:"password_set"`
	} `json:"web"`
	// Managed is true for a setting that emm.txt fixes: the UI shows it
	// read-only and a PUT for it is refused with 409.
	Managed struct {
		RTP         bool `json:"rtp"`
		BLE         bool `json:"ble"`
		BLEName     bool `json:"ble_name"`
		WebPassword bool `json:"web_password"`
	} `json:"managed"`
}

func (s *Server) buildSettings() settingsBody {
	var b settingsBody
	b.Managed.RTP = s.d.BootRTP != nil
	b.Managed.BLE = s.d.BootBLE != nil
	b.Managed.BLEName = s.d.BootBLEName != ""
	b.Managed.WebPassword = s.d.BootWebPassword != ""
	st := s.d.Store.Snapshot()
	b.RTP.Enabled = st.RTPOn()
	if s.d.BootRTP != nil {
		b.RTP.Enabled = *s.d.BootRTP
	}
	b.BLE.Enabled = st.BLEOn()
	if s.d.BootBLE != nil {
		b.BLE.Enabled = *s.d.BootBLE
	}
	b.BLE.Name = st.BLE.Name
	if s.d.BootBLEName != "" {
		b.BLE.Name = s.d.BootBLEName
	}
	if b.BLE.Name == "" {
		b.BLE.Name = s.d.BLEDefault
		if b.BLE.Name == "" {
			b.BLE.Name = "EMM"
		}
	}
	if s.d.BLE != nil {
		bs := s.d.BLE()
		b.BLE.Available, b.BLE.Advertising, b.BLE.Connected, b.BLE.Error = bs.Available, bs.Advertising, bs.Connected, bs.Error
	}
	b.Web.PasswordSet = s.d.BootWebPassword != "" || st.WebPasswordSet()
	return b
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildSettings())
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RTP *struct {
			Enabled *bool `json:"enabled"`
		} `json:"rtp"`
		BLE *struct {
			Enabled *bool   `json:"enabled"`
			Name    *string `json:"name"`
		} `json:"ble"`
		Web *struct {
			Password *string `json:"password"`
		} `json:"web"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.BLE != nil && body.BLE.Name != nil {
		n := strings.TrimSpace(*body.BLE.Name)
		if n == "" || utf8.RuneCountInString(n) > 20 {
			writeErr(w, http.StatusBadRequest, "Bluetooth name must be 1–20 characters")
			return
		}
		body.BLE.Name = &n
	}
	if body.Web != nil && body.Web.Password != nil && len(*body.Web.Password) > 200 {
		writeErr(w, http.StatusBadRequest, "password is too long")
		return
	}
	// Settings fixed by emm.txt cannot be changed here: saying "ok" while the
	// file keeps winning would be a lie.
	var fixed []string
	if body.RTP != nil && body.RTP.Enabled != nil && s.d.BootRTP != nil {
		fixed = append(fixed, "rtp")
	}
	if body.BLE != nil && body.BLE.Enabled != nil && s.d.BootBLE != nil {
		fixed = append(fixed, "ble")
	}
	if body.BLE != nil && body.BLE.Name != nil && s.d.BootBLEName != "" {
		fixed = append(fixed, "ble_name")
	}
	if body.Web != nil && body.Web.Password != nil && s.d.BootWebPassword != "" {
		fixed = append(fixed, "web_password")
	}
	if len(fixed) > 0 {
		writeErr(w, http.StatusConflict, "controlled by emm.txt on the SD card ("+strings.Join(fixed, ", ")+"); edit or remove it there")
		return
	}
	s.d.Store.Update(func(st *config.State) {
		if body.RTP != nil && body.RTP.Enabled != nil {
			v := *body.RTP.Enabled
			st.RTP.Enabled = &v
		}
		if body.BLE != nil {
			if body.BLE.Enabled != nil {
				v := *body.BLE.Enabled
				st.BLE.Enabled = &v
			}
			if body.BLE.Name != nil {
				st.BLE.Name = *body.BLE.Name
			}
		}
		if body.Web != nil && body.Web.Password != nil {
			st.SetWebPassword(*body.Web.Password)
		}
	})
	if s.d.ApplySettings != nil {
		s.d.ApplySettings()
	}
	s.NotifySettings()
	writeJSON(w, http.StatusOK, s.buildSettings())
}

// ---- network -----------------------------------------------------------------------

func (s *Server) networkBody() map[string]any {
	if s.d.Net == nil {
		return map[string]any{
			"status": map[string]any{"mode": "unmanaged", "setup_mode": false, "hostname": s.d.Hostname(),
				"ethernet": map[string]any{"link": false, "ip": "", "mode": ""}, "scan_age": 0},
			"networks": []any{}, "saved_ssid": "",
			"ethernet":     map[string]any{"mode": "auto", "address": "", "gateway": "", "dns": []string{}},
			"boot_managed": []string{},
		}
	}
	managed := s.d.Net.BootManaged()
	if managed == nil {
		managed = []string{}
	}
	return map[string]any{
		"status":       s.d.Net.Status(),
		"networks":     s.d.Net.Networks(),
		"saved_ssid":   s.d.Net.SavedSSID(),
		"ethernet":     s.d.Net.Ethernet(),
		"boot_managed": managed,
	}
}

func (s *Server) needNet(w http.ResponseWriter) bool {
	if s.d.Net == nil {
		writeErr(w, http.StatusNotImplemented, "network management is not available on this system")
		return false
	}
	return true
}

func (s *Server) handleNetworkGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.networkBody())
}

func (s *Server) handleNetworkScan(w http.ResponseWriter, r *http.Request) {
	if !s.needNet(w) {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	// A forced scan in setup mode takes the access point down, which drops the
	// very phone that asked: the request context is cancelled mid-scan. Detach
	// from it so the scan finishes and the fresh list is there when the phone
	// reconnects (the UI polls for it).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	nets, stale, err := s.d.Net.Scan(ctx, force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": nets, "stale": stale})
}

func (s *Server) handleNetworkWifi(w http.ResponseWriter, r *http.Request) {
	if !s.needNet(w) {
		return
	}
	var body struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.SSID == "" || len(body.SSID) > 32 {
		writeErr(w, http.StatusBadRequest, "ssid is required (max 32 bytes)")
		return
	}
	if !validWifiPassword(body.Password) {
		writeErr(w, http.StatusBadRequest, "WPA password must be 8–63 characters or 64 hex digits (leave empty for an open network)")
		return
	}
	if err := s.d.Net.Connect(body.SSID, body.Password); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	s.NotifyNetwork()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": s.d.Net.Status()})
}

// validWifiPassword accepts what netcfg accepts: empty (open network), a
// passphrase of 8-63 bytes, or a raw 64-digit hex PSK.
func validWifiPassword(p string) bool {
	if p == "" || len(p) >= 8 && len(p) <= 63 {
		return true
	}
	return len(p) == 64 && isHex(p)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (s *Server) handleNetworkForget(w http.ResponseWriter, r *http.Request) {
	if !s.needNet(w) {
		return
	}
	if err := s.d.Net.Forget(); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	s.NotifyNetwork()
	writeJSON(w, http.StatusOK, map[string]any{"status": s.d.Net.Status()})
}

func (s *Server) handleNetworkEthernet(w http.ResponseWriter, r *http.Request) {
	if !s.needNet(w) {
		return
	}
	var body struct {
		Mode    string   `json:"mode"`
		Address string   `json:"address"`
		Gateway string   `json:"gateway"`
		DNS     []string `json:"dns"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	switch body.Mode {
	case "auto", "dhcp", "static", "off":
	default:
		writeErr(w, http.StatusBadRequest, "mode must be auto, dhcp, static or off")
		return
	}
	if err := s.d.Net.SetEthernet(body.Mode, body.Address, body.Gateway, body.DNS); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	s.NotifyNetwork()
	writeJSON(w, http.StatusOK, s.networkBody())
}

func (s *Server) handleNetworkHostname(w http.ResponseWriter, r *http.Request) {
	if !s.needNet(w) {
		return
	}
	var body struct {
		Hostname string `json:"hostname"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.d.Net.SetHostname(body.Hostname); err != nil {
		writeErr(w, errStatus(err), err.Error())
		return
	}
	s.NotifyNetwork()
	s.NotifyTopology()
	writeJSON(w, http.StatusOK, s.networkBody())
}

// ---- system ---------------------------------------------------------------------------

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

func systemUptime(started time.Time) int64 {
	if f := strings.Fields(readTrim("/proc/uptime")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			return int64(v)
		}
	}
	return int64(time.Since(started).Seconds())
}

// deviceKey names the physical device behind a port, so that the ports of one
// device (one per cable) count once and two identical devices count twice. USB
// port IDs are usb:<vid>:<pid>:<serial-or-usb-path>:<cable>; dropping the cable
// field leaves the device. Other IDs (simulated ports, CoreMIDI's usb:cm-<uid>)
// have no such shape and stand for themselves. The display Group does not do:
// it is built from the product name only.
func deviceKey(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) >= 5 && parts[0] == "usb" {
		return strings.Join(parts[:len(parts)-1], ":")
	}
	return id
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	model := readTrim("/proc/device-tree/model")
	if model == "" {
		model = runtime.GOOS + "/" + runtime.GOARCH
	}
	var temp any
	if t := readTrim("/sys/class/thermal/thermal_zone0/temp"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			temp = float64(v) / 1000
		}
	}
	devices := map[string]bool{}
	for _, p := range s.d.Router.Snapshot().Ports {
		if p.Kind.IsDevice() {
			devices[deviceKey(p.ID)] = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.d.Version,
		"uptime_s":       systemUptime(s.started),
		"model":          model,
		"kernel":         readTrim("/proc/sys/kernel/osrelease"),
		"cpu_temp_c":     temp,
		"usb_devices":    len(devices),
		"allow_power":    s.d.AllowPower,
		"backend":        s.d.Backend,
		"state_writable": s.d.Store.Writable(),
	})
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request, action string) {
	if !s.d.AllowPower {
		writeErr(w, http.StatusForbidden, "power control is disabled (start emmd with --allow-power)")
		return
	}
	_ = s.d.Store.Flush()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": action + " scheduled"})
	go func() {
		time.Sleep(700 * time.Millisecond) // let the response reach the browser
		if err := s.d.Power(action); err != nil {
			s.log.Error("power action failed", "action", action, "err", err)
		}
	}()
}

func defaultPower(action string) error {
	verb := map[string]string{"reboot": "reboot", "shutdown": "poweroff"}[action]
	if verb == "" {
		return os.ErrInvalid
	}
	if err := exec.Command("systemctl", verb).Run(); err == nil {
		return nil
	}
	return exec.Command(verb).Run() // busybox fallback
}
