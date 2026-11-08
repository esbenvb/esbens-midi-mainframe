package netcfg

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

// Network is one WiFi network from a scan.
type Network struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"`   // 0-100
	Security string `json:"security"` // "open", "WPA2", "WPA3", "WPA1 WPA2", …
	InUse    bool   `json:"in_use"`
}

// Open reports whether the network needs no password.
func (n Network) Open() bool { return n.Security == "open" }

// splitTerse splits one line of `nmcli -t` output. Fields are separated by ':'
// and a literal ':' or '\' inside a field is escaped with a backslash.
func splitTerse(line string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(fields, cur.String())
}

// parseWifiList parses the output of
//
//	nmcli -t -f IN-USE,SSID,SIGNAL,SECURITY device wifi list
//
// Hidden networks (empty SSID) are dropped. Duplicate SSIDs (several access
// points of one network) are merged, keeping the strongest signal. The result
// is sorted by signal, strongest first.
func parseWifiList(out string) []Network {
	best := map[string]*Network{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 4 {
			continue
		}
		ssid := f[1]
		if ssid == "" {
			continue
		}
		sig, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		if sig < 0 {
			sig = 0
		}
		if sig > 100 {
			sig = 100
		}
		sec := strings.TrimSpace(f[3])
		if sec == "" || sec == "--" {
			sec = "open"
		}
		inUse := strings.TrimSpace(f[0]) == "*"
		if n, ok := best[ssid]; ok {
			if sig > n.Signal {
				n.Signal, n.Security = sig, sec
			}
			n.InUse = n.InUse || inUse
			continue
		}
		best[ssid] = &Network{SSID: ssid, Signal: sig, Security: sec, InUse: inUse}
		order = append(order, ssid)
	}
	nets := make([]Network, 0, len(order))
	for _, s := range order {
		nets = append(nets, *best[s])
	}
	sort.SliceStable(nets, func(i, j int) bool { return nets[i].Signal > nets[j].Signal })
	return nets
}

// nmDevice is one row of `nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device`.
type nmDevice struct {
	Name  string
	Type  string
	State string // first word: connected, connecting, disconnected, unavailable, unmanaged, …
	Conn  string
}

func parseDevices(out string) []nmDevice {
	var devs []nmDevice
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 3 {
			continue
		}
		d := nmDevice{Name: f[0], Type: f[1], State: strings.ToLower(firstWord(f[2]))}
		if len(f) > 3 {
			d.Conn = f[3]
		}
		devs = append(devs, d)
	}
	return devs
}

// nmProfile is one row of `nmcli -t -f NAME,TYPE,AUTOCONNECT connection show`.
type nmProfile struct {
	Name        string
	Type        string // "wifi" (older nmcli: "802-11-wireless"), "ethernet", …
	Autoconnect bool
}

func parseProfiles(out string) []nmProfile {
	var ps []nmProfile
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 3 || f[0] == "" {
			continue
		}
		ps = append(ps, nmProfile{Name: f[0], Type: strings.ToLower(strings.TrimSpace(f[1])),
			Autoconnect: strings.EqualFold(strings.TrimSpace(f[2]), "yes")})
	}
	return ps
}

// isEMMConn reports whether a connection profile is one of EMM's own (emm-wifi,
// emm-eth, emm-setup-ap). Everything else was made by somebody else (Raspberry
// Pi Imager, nmtui, raspi-config, a desktop) and EMM leaves it alone.
func isEMMConn(name string) bool { return strings.HasPrefix(name, "emm-") }

// foreignWifiProfiles returns the names of the WiFi profiles EMM did not make
// that NetworkManager may bring up by itself (autoconnect on).
func foreignWifiProfiles(ps []nmProfile) []string {
	var names []string
	for _, p := range ps {
		if (p.Type == "wifi" || p.Type == "802-11-wireless") && p.Autoconnect && !isEMMConn(p.Name) {
			names = append(names, p.Name)
		}
	}
	return names
}

// foreignEthernetProfiles returns the names of the Ethernet profiles EMM did not
// make, whether or not they autoconnect: any of them is somebody's own setup of
// the cable (a static address from nmtui, NetworkManager's "Wired connection 1").
func foreignEthernetProfiles(ps []nmProfile) []string {
	var names []string
	for _, p := range ps {
		if (p.Type == "ethernet" || p.Type == "802-3-ethernet") && !isEMMConn(p.Name) {
			names = append(names, p.Name)
		}
	}
	return names
}

// unescapeTerse undoes nmcli's terse-mode escaping (`\:` and `\\`) of a single
// value that was not split into fields.
func unescapeTerse(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t("); i >= 0 {
		return s[:i]
	}
	return s
}

// pickDevices chooses the WiFi and Ethernet devices to manage. Overrides win;
// otherwise the first WiFi device and the first non-unmanaged Ethernet device.
func pickDevices(devs []nmDevice, wifiOverride, ethOverride string) (wifi, eth string) {
	wifi, eth = wifiOverride, ethOverride
	for _, d := range devs {
		if wifi == "" && d.Type == "wifi" {
			wifi = d.Name
		}
		if eth == "" && d.Type == "ethernet" && d.State != "unmanaged" {
			eth = d.Name
		}
	}
	return wifi, eth
}

func findDevice(devs []nmDevice, name string) (nmDevice, bool) {
	for _, d := range devs {
		if d.Name == name {
			return d, true
		}
	}
	return nmDevice{}, false
}

// parseIPv4 extracts the first IPv4 address from `nmcli -g IP4.ADDRESS device
// show X` output (one "a.b.c.d/nn" per line, possibly "|"-separated, possibly
// prefixed with the field name). It returns "" if there is none.
func parseIPv4(out string) string {
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '|' }) {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "IP4.ADDRESS"); i >= 0 {
			if j := strings.IndexByte(line[i:], ':'); j >= 0 {
				line = strings.TrimSpace(line[i+j+1:])
			}
		}
		if i := strings.IndexByte(line, '/'); i >= 0 {
			line = line[:i]
		}
		if isIPv4(line) {
			return line
		}
	}
	return ""
}

// parseMAC extracts the MAC address from `nmcli -g GENERAL.HWADDR device show X`.
func parseMAC(out string) string {
	s := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	s = strings.ReplaceAll(s, `\`, "")
	if hw, err := net.ParseMAC(s); err == nil {
		return hw.String()
	}
	return ""
}

// macSuffix returns the last two bytes of a MAC as upper-case hex ("B8:27:EB:12:34:56" → "3456").
func macSuffix(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) < 2 {
		return ""
	}
	n := len(hw)
	return strings.ToUpper(hexByte(hw[n-2]) + hexByte(hw[n-1]))
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}
