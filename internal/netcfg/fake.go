package netcfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeNet is a network in the simulated air.
type fakeNet struct {
	SSID     string
	Signal   int
	Security string // "open", "WPA2", …
}

// fakeProfile is a connection profile somebody else made (Raspberry Pi Imager,
// nmtui): NetworkManager knows it, the Manager must leave it alone.
type fakeProfile struct {
	ID          string
	SSID        string
	Autoconnect bool
	// Type is what `connection show` lists: "" is a WiFi profile, "ethernet" one
	// for the cable (SSID is unused then). With no emm-eth of EMM's, an
	// autoconnect one serves the cable, as NetworkManager would.
	Type string
}

// fakeWorld is a small simulation of NetworkManager + a WiFi environment. It
// implements Runner and understands exactly the nmcli/iw/hostname invocations
// Manager makes, reading the keyfiles Manager writes. It backs NewFake (for
// developing the UI on a laptop) and the unit tests.
type fakeWorld struct {
	mu    sync.Mutex
	nmDir string

	// Behaviour knobs.
	delay     func(ctx context.Context, d time.Duration) error // nil: no delay
	joinDelay time.Duration
	nmDown    bool // nmcli fails with "NetworkManager is not running"

	hostname string
	wifiDev  string // "" = no WiFi adapter (yet: see addWifiDevice)
	ethDev   string // "" = no Ethernet adapter
	wifiMAC  string
	ethCable bool
	ethIP    string
	networks []fakeNet

	// WiFi behaviour.
	wifiNeverConnects bool            // `connection up` succeeds but the link never comes up
	failSSIDs         map[string]bool // joining these always fails
	// hidden lists networks that do not broadcast their name: absent from scan
	// results, joinable by an explicit `connection up`, but (as with
	// NetworkManager) not by autoconnect unless the profile says hidden=true.
	hidden map[string]bool
	// stations are the MAC addresses of clients associated with the AP, as
	// `iw dev X station dump` reports them while the AP is up.
	stations []string
	// apUpFails makes the next N `connection up emm-setup-ap` fail.
	apUpFails int
	// foreign are profiles that were there before EMM. They are listed by
	// `connection show` and can be brought up by name, and nothing else: whether
	// one is active is up to the test (activateForeign), as NetworkManager's
	// autoconnect timing is.
	foreign []fakeProfile

	// Runtime state.
	apUp       bool
	wifiConn   string // active client profile id
	wifiIP     string
	connecting bool // wifi stuck connecting (wifiNeverConnects)
	ethOff     bool // device disconnected by `nmcli device disconnect`
	nextHost   int

	// Observations for assertions.
	cmds          []string
	scansDuringAP int
	apUps         int
}

func newFakeWorld(nmDir string) *fakeWorld {
	return &fakeWorld{
		nmDir:    nmDir,
		hostname: "raspberrypi",
		wifiDev:  "wlan0", ethDev: "eth0",
		wifiMAC: "B8:27:EB:3F:A1:C2",
		ethIP:   "192.168.1.50",
		networks: []fakeNet{
			{"HomeNet-5G", 82, "WPA2"},
			{"Skatteguiden Guest", 74, "WPA2"},
			{"Fiber-3F21", 61, "WPA3"},
			{"linksys", 44, "WPA2"},
			{"Studio:5G", 41, "WPA2"},
			{"CoffeeShop Open", 38, "open"},
			{"TP-LINK_A1B2", 30, "WPA1 WPA2"},
			{"Telia-9842", 22, "WPA2"},
		},
		failSSIDs: map[string]bool{},
		hidden:    map[string]bool{},
		nextHost:  100,
	}
}

func (w *fakeWorld) sleep(ctx context.Context, d time.Duration) error {
	if w.delay == nil || d <= 0 {
		return ctx.Err()
	}
	return w.delay(ctx, d)
}

func (w *fakeWorld) profile(id string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(w.nmDir, id+".nmconnection"))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// kfGet returns the first value of key in section of a keyfile, unescaped.
func kfGet(content, section, key string) (string, bool) {
	cur := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = line[1 : len(line)-1]
			continue
		}
		if cur != section {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 && line[:i] == key {
			return line[i+1:], true
		}
	}
	return "", false
}

// kfUnescape reverses kfEscape (GLib's key file string rules). With isSSID it
// then reads the value the way NetworkManager reads `ssid=` (nm-keyfile.c,
// get_bytes with unescape_semicolon): a value of only digits, ';' and white
// space that parses as "n;n;n;" (each 0-255, each followed by ';') is the
// obsolete byte list; any other value is a string in which `\;` means ';'.
func kfUnescape(v string, isSSID bool) string {
	s := kfUnescapeString(v)
	if !isSSID {
		return s
	}
	if s != "" && strings.Trim(s, "0123456789; \t\n\v\f\r") == "" {
		if out, ok := parseByteList(s); ok {
			return string(out)
		}
	}
	return strings.ReplaceAll(s, `\;`, ";")
}

// parseByteList parses "49;50;" (white space between tokens tolerated).
func parseByteList(s string) ([]byte, bool) {
	var out []byte
	rest := strings.TrimSpace(s)
	for rest != "" {
		i := strings.IndexByte(rest, ';')
		if i < 0 {
			return nil, false // every number must be followed by ';'
		}
		tok := strings.TrimSpace(rest[:i])
		n, err := strconv.Atoi(tok)
		if err != nil || tok == "" || len(tok) > 3 || n > 255 {
			return nil, false
		}
		out = append(out, byte(n))
		rest = strings.TrimSpace(rest[i+1:])
	}
	return out, len(out) > 0
}

func kfUnescapeString(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
			switch v[i] {
			case 's':
				b.WriteByte(' ')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(v[i])
			}
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func nmErr(msg string) error {
	return &RunError{Cmd: "nmcli", Stderr: "Error: " + msg + "\n", Err: errors.New("exit status 4")}
}

func terseEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, ":", `\:`)
}

// ethState reports the Ethernet device state. Caller holds w.mu.
func (w *fakeWorld) ethStateLocked() (state, conn string) {
	if w.ethDev == "" {
		return "", ""
	}
	if !w.ethCable {
		return "unavailable", ""
	}
	if w.ethOff {
		return "disconnected", ""
	}
	prof, ok := w.profile(ethConnID)
	if !ok {
		for _, p := range w.foreign {
			if p.Type == "ethernet" && p.Autoconnect {
				return "connected", p.ID
			}
		}
		return "disconnected", ""
	}
	if strings.Contains(prof, "autoconnect=false") {
		return "disconnected", ""
	}
	return "connected", ethConnID
}

func (w *fakeWorld) wifiStateLocked() (state, conn string) {
	switch {
	case w.wifiDev == "":
		return "", ""
	case w.apUp:
		return "connected", apConnID
	case w.wifiConn != "":
		return "connected", w.wifiConn
	case w.connecting:
		return "connecting (configuring)", wifiConnID
	}
	return "disconnected", ""
}

// tryWifi attempts to bring the client profile up. Caller holds w.mu; the lock
// is released while sleeping to emulate the join taking time.
//
// auto is true when NetworkManager itself is activating a freshly loaded
// profile (autoconnect) rather than answering `nmcli connection up`.
func (w *fakeWorld) tryWifi(ctx context.Context, auto bool) error {
	prof, ok := w.profile(wifiConnID)
	if !ok {
		return nmErr("unknown connection '" + wifiConnID + "'.")
	}
	rawSSID, _ := kfGet(prof, "wifi", "ssid")
	ssid := kfUnescape(rawSSID, true)
	profHidden, _ := kfGet(prof, "wifi", "hidden")
	rawPSK, hasPSK := kfGet(prof, "wifi-security", "psk")
	psk := kfUnescape(rawPSK, false)

	var net *fakeNet
	for i := range w.networks {
		if w.networks[i].SSID == ssid {
			net = &w.networks[i]
		}
	}
	w.apUp = false
	w.wifiConn = ""
	if w.wifiNeverConnects {
		w.connecting = true
		return nil
	}
	jd := w.joinDelay
	w.connecting = true
	w.mu.Unlock()
	err := w.sleep(ctx, jd)
	w.mu.Lock()
	w.connecting = false
	if err != nil {
		return err
	}
	switch {
	case net == nil:
		return nmErr(fmt.Sprintf("Connection activation failed: No network with SSID '%s' found.", ssid))
	case w.hidden[ssid] && auto && profHidden != "true":
		return nmErr("Connection activation failed: (53) The Wi-Fi network could not be found")
	case w.failSSIDs[ssid]:
		return nmErr("Connection activation failed: (53) The Wi-Fi network could not be found")
	case net.Security != "open" && (!hasPSK || strings.HasPrefix(psk, "wrong")):
		return nmErr("Connection activation failed: (7) Secrets were required, but not provided.")
	}
	w.wifiConn = wifiConnID
	w.nextHost++
	w.wifiIP = fmt.Sprintf("192.168.1.%d", w.nextHost)
	return nil
}

// Run implements Runner.
func (w *fakeWorld) Run(ctx context.Context, name string, args ...string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cmds = append(w.cmds, name+" "+strings.Join(args, " "))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch name {
	case "iw":
		if len(args) == 4 && args[0] == "dev" && args[2] == "station" && args[3] == "dump" {
			return w.stationDumpLocked(args[1]), nil
		}
		return "", nil
	case "rfkill", "systemctl":
		return "", nil
	case "hostname":
		if len(args) == 0 {
			return w.hostname + "\n", nil
		}
		w.hostname = args[0]
		return "", nil
	case "hostnamectl":
		if len(args) == 2 && args[0] == "set-hostname" {
			w.hostname = args[1]
			return "", nil
		}
		return "", nmErr("bad hostnamectl call")
	case "nmcli":
		return w.nmcliLocked(ctx, args)
	}
	return "", &RunError{Cmd: name, Err: errors.New("executable file not found")}
}

// stationDumpLocked mimics `iw dev X station dump`: one block per associated
// client while the AP is up, nothing otherwise.
func (w *fakeWorld) stationDumpLocked(dev string) string {
	if !w.apUp || dev != w.wifiDev {
		return ""
	}
	var b strings.Builder
	for _, mac := range w.stations {
		fmt.Fprintf(&b, "Station %s (on %s)\n\tinactive time:\t120 ms\n\trx bytes:\t5113\n\tsignal:  \t-48 dBm\n", mac, dev)
	}
	return b.String()
}

func (w *fakeWorld) nmcliLocked(ctx context.Context, args []string) (string, error) {
	if w.nmDown {
		return "", &RunError{Cmd: "nmcli", Stderr: "Error: NetworkManager is not running.\n", Err: errors.New("exit status 8")}
	}
	var get string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
		case "-f", "-g":
			if i+1 < len(args) {
				get = args[i+1]
				i++
			}
		case "--wait", "-w":
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		return "", nmErr("missing command")
	}
	switch rest[0] {
	case "device":
		return w.deviceLocked(ctx, rest[1:], get)
	case "connection":
		return w.connectionLocked(ctx, rest[1:], get)
	}
	return "", nmErr("unknown command " + rest[0])
}

func (w *fakeWorld) deviceLocked(ctx context.Context, a []string, fields string) (string, error) {
	if len(a) == 0 { // device list
		var b strings.Builder
		if st, conn := w.ethStateLocked(); st != "" {
			fmt.Fprintf(&b, "%s:ethernet:%s:%s\n", w.ethDev, st, conn)
		}
		if st, conn := w.wifiStateLocked(); st != "" {
			fmt.Fprintf(&b, "%s:wifi:%s:%s\n", w.wifiDev, st, conn)
			fmt.Fprintf(&b, "p2p-dev-%s:wifi-p2p:disconnected:\n", w.wifiDev)
		}
		b.WriteString("lo:loopback:unmanaged:\n")
		return b.String(), nil
	}
	switch a[0] {
	case "show":
		if len(a) < 2 {
			return "", nmErr("device show needs a device")
		}
		dev := a[1]
		switch fields {
		case "GENERAL.HWADDR":
			if dev == w.wifiDev {
				return terseEscape(w.wifiMAC) + "\n", nil
			}
			return "", nil
		case "IP4.ADDRESS":
			switch dev {
			case w.ethDev:
				if st, _ := w.ethStateLocked(); st == "connected" {
					return w.ethIP + "/24\n", nil
				}
			case w.wifiDev:
				if w.apUp {
					return apCIDR + "\n", nil
				}
				if w.wifiConn != "" {
					return w.wifiIP + "/24\n", nil
				}
			}
			return "", nil
		}
	case "wifi":
		if len(a) > 1 && a[1] == "list" {
			if w.apUp {
				w.scansDuringAP++
				return "", nmErr("Failed to request scan: Scanning not allowed while the access point is active")
			}
			var b strings.Builder
			for _, n := range w.networks {
				if w.hidden[n.SSID] {
					continue
				}
				inUse := " "
				if w.wifiConn != "" {
					if prof, ok := w.profile(wifiConnID); ok {
						raw, _ := kfGet(prof, "wifi", "ssid")
						if kfUnescape(raw, true) == n.SSID {
							inUse = "*"
						}
					}
				}
				sec := n.Security
				if sec == "open" {
					sec = "--"
				}
				fmt.Fprintf(&b, "%s:%s:%d:%s\n", inUse, terseEscape(n.SSID), n.Signal, sec)
				// A second access point of the same network, a bit weaker.
				if n.Signal > 60 {
					fmt.Fprintf(&b, " :%s:%d:%s\n", terseEscape(n.SSID), n.Signal-25, sec)
				}
			}
			b.WriteString(" ::50:WPA2\n") // a hidden network
			return b.String(), nil
		}
	case "disconnect":
		if len(a) > 1 && a[1] == w.ethDev {
			w.ethOff = true
		}
		return "", nil
	}
	return "", nmErr("unsupported device command")
}

// autoconnectLocked is NetworkManager auto-connecting EMM's freshly loaded WiFi
// profile (or one that was loaded while there was no device to use it on).
func (w *fakeWorld) autoconnectLocked(ctx context.Context) {
	if prof, ok := w.profile(wifiConnID); ok && w.wifiDev != "" && !w.apUp && w.wifiConn == "" &&
		!strings.Contains(prof, "autoconnect=false") {
		_ = w.tryWifi(ctx, true) // failure just leaves the device disconnected
	}
}

// addWifiDevice makes the WiFi adapter appear (its driver finished loading).
// NetworkManager then auto-connects EMM's WiFi profile if there is one.
func (w *fakeWorld) addWifiDevice(ctx context.Context, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wifiDev = name
	w.autoconnectLocked(ctx)
}

// activateForeign is NetworkManager bringing up one of the foreign profiles.
func (w *fakeWorld) activateForeign(ctx context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tryForeignLocked(ctx, id)
}

func (w *fakeWorld) foreignLocked(id string) (fakeProfile, bool) {
	for _, p := range w.foreign {
		if p.ID == id {
			return p, true
		}
	}
	return fakeProfile{}, false
}

// tryForeignLocked brings a foreign profile up on the WiFi device: like any
// activation it first takes the device away from whatever it was doing.
func (w *fakeWorld) tryForeignLocked(ctx context.Context, id string) error {
	p, ok := w.foreignLocked(id)
	if !ok {
		return nmErr("unknown connection '" + id + "'.")
	}
	if w.wifiDev == "" {
		return nmErr("Connection activation failed: No suitable device found for this connection.")
	}
	w.apUp, w.wifiConn, w.connecting = false, "", false
	var net *fakeNet
	for i := range w.networks {
		if w.networks[i].SSID == p.SSID {
			net = &w.networks[i]
		}
	}
	if net == nil || w.failSSIDs[p.SSID] {
		return nmErr("Connection activation failed: No network with SSID '" + p.SSID + "' found.")
	}
	w.wifiConn = p.ID
	w.nextHost++
	w.wifiIP = fmt.Sprintf("192.168.1.%d", w.nextHost)
	return nil
}

// connectionShowLocked answers `nmcli -t -f NAME,TYPE,AUTOCONNECT connection
// show` and `nmcli -g 802-11-wireless.ssid connection show id NAME`.
func (w *fakeWorld) connectionShowLocked(a []string, fields string) (string, error) {
	if len(a) == 0 {
		var b strings.Builder
		for _, p := range w.foreign {
			ac, typ := "no", "wifi"
			if p.Autoconnect {
				ac = "yes"
			}
			if p.Type != "" {
				typ = p.Type
			}
			fmt.Fprintf(&b, "%s:%s:%s\n", terseEscape(p.ID), typ, ac)
		}
		for _, id := range []string{wifiConnID, apConnID, ethConnID} {
			if prof, ok := w.profile(id); ok {
				typ, ac := "wifi", "yes"
				if id == ethConnID {
					typ = "ethernet"
				}
				if strings.Contains(prof, "autoconnect=false") {
					ac = "no"
				}
				fmt.Fprintf(&b, "%s:%s:%s\n", id, typ, ac)
			}
		}
		return b.String(), nil
	}
	if len(a) != 2 || a[0] != "id" || fields != "802-11-wireless.ssid" {
		return "", nmErr("unsupported connection show")
	}
	if p, ok := w.foreignLocked(a[1]); ok {
		return terseEscape(p.SSID) + "\n", nil
	}
	if prof, ok := w.profile(a[1]); ok {
		raw, _ := kfGet(prof, "wifi", "ssid")
		return terseEscape(kfUnescape(raw, true)) + "\n", nil
	}
	return "", nmErr("unknown connection '" + a[1] + "'.")
}

func (w *fakeWorld) connectionLocked(ctx context.Context, a []string, fields string) (string, error) {
	if len(a) == 0 {
		return "", nmErr("connection needs a verb")
	}
	switch a[0] {
	case "reload":
		w.autoconnectLocked(ctx)
		return "", nil
	case "show":
		return w.connectionShowLocked(a[1:], fields)
	case "up":
		if len(a) < 2 {
			return "", nmErr("connection up needs an id")
		}
		if a[1] == "id" && len(a) > 2 {
			a = a[1:]
		}
		switch a[1] {
		case apConnID:
			if _, ok := w.profile(apConnID); !ok {
				return "", nmErr("unknown connection '" + apConnID + "'.")
			}
			if w.apUpFails > 0 {
				w.apUpFails--
				return "", nmErr("Connection activation failed: Could not start the access point")
			}
			w.wifiConn, w.connecting, w.apUp = "", false, true
			w.apUps++
			return "", nil
		case wifiConnID:
			return "", w.tryWifi(ctx, false)
		case ethConnID:
			if _, ok := w.profile(ethConnID); !ok {
				return "", nmErr("unknown connection '" + ethConnID + "'.")
			}
			w.ethOff = false
			if !w.ethCable {
				return "", nmErr("Connection activation failed: Device is not available because the device has no carrier")
			}
			return "", nil
		}
		if _, ok := w.foreignLocked(a[1]); ok {
			return "", w.tryForeignLocked(ctx, a[1])
		}
		return "", nmErr("unknown connection '" + a[1] + "'.")
	case "down":
		if len(a) > 1 {
			switch a[1] {
			case apConnID:
				w.apUp = false
			case wifiConnID:
				w.wifiConn, w.connecting = "", false
			default:
				if w.wifiConn == a[1] {
					w.wifiConn = ""
				}
			}
		}
		return "", nil
	case "delete":
		if len(a) > 1 {
			switch a[1] {
			case apConnID:
				w.apUp = false
			case wifiConnID:
				w.wifiConn, w.connecting = "", false
			default:
				if _, ok := w.foreignLocked(a[1]); ok {
					if w.wifiConn == a[1] {
						w.wifiConn = ""
					}
					var keep []fakeProfile
					for _, p := range w.foreign {
						if p.ID != a[1] {
							keep = append(keep, p)
						}
					}
					w.foreign = keep
					return "", nil
				}
			}
			if _, ok := w.profile(a[1]); !ok && a[1] != wifiConnID && a[1] != apConnID {
				return "", nmErr("unknown connection '" + a[1] + "'.")
			}
			_ = os.Remove(filepath.Join(w.nmDir, a[1]+".nmconnection"))
		}
		return "", nil
	}
	return "", nmErr("unsupported connection command")
}

// NewFake returns a Manager wired to a simulated NetworkManager, so the web UI
// and daemon can be developed on a machine without one (e.g. a Mac).
//
// The simulated Pi has eight WiFi networks around it and no Ethernet cable. If
// Saved has no WiFi it starts in setup mode (AP up). Connecting to any network
// succeeds after roughly two seconds, unless the password starts with "wrong"
// (or the network is secured and the password is empty), in which case it fails
// and setup mode returns. WPA length rules are relaxed so "wrong" can be typed. Temp directories stand in for /etc/NetworkManager.
//
// Options left empty are filled in: NMDir/DNSMasqDir (temp dirs), Timing
// (short waits), and in-memory Load/Save.
func NewFake(o Options) *Manager {
	if o.NMDir == "" {
		o.NMDir, _ = os.MkdirTemp("", "emm-fake-nm-")
	}
	if o.DNSMasqDir == "" {
		o.DNSMasqDir, _ = os.MkdirTemp("", "emm-fake-dnsmasq-")
	}
	if o.HostsFile == "" {
		// A renamed fake must not touch the laptop's own /etc/hosts. If no temp
		// directory can be made, point at a file that is not there: it is then
		// simply left alone.
		dir, err := os.MkdirTemp("", "emm-fake-hosts-")
		if err != nil {
			dir = filepath.Join(os.TempDir(), "emm-fake-hosts-unavailable")
		}
		o.HostsFile = filepath.Join(dir, "hosts")
		seed := "127.0.0.1\tlocalhost\n::1\t\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + DefaultHostname + "\n"
		_ = os.WriteFile(o.HostsFile, []byte(seed), 0o644)
	}
	w := newFakeWorld(o.NMDir)
	w.hostname = DefaultHostname // the appliance image's name: the UI shows what it would on the real thing
	w.delay = realSleep
	w.joinDelay = time.Second
	o.Runner = w
	o.RelaxedPSK = true
	if o.Timing == (Timing{}) {
		o.Timing = Timing{
			NMReady: 2 * time.Second, EthernetWait: 3 * time.Second, EthernetNoCarrier: 400 * time.Millisecond,
			WifiWait: 15 * time.Second, WifiDeviceWait: 2 * time.Second, Poll: 100 * time.Millisecond, Monitor: 2 * time.Second,
			SetupEthCheck: 5 * time.Second, ConnectDelay: time.Second,
		}
	}
	if o.Load == nil || o.Save == nil {
		var mu sync.Mutex
		var held Saved
		if o.Load != nil {
			held = o.Load()
		}
		o.Load = func() Saved { mu.Lock(); defer mu.Unlock(); return held }
		o.Save = func(s Saved) error { mu.Lock(); defer mu.Unlock(); held = s; return nil }
	}
	if s := o.Load(); s.WifiSSID != "" {
		known := false
		for _, n := range w.networks {
			known = known || n.SSID == s.WifiSSID
		}
		if !known {
			w.networks = append(w.networks, fakeNet{s.WifiSSID, 70, "WPA2"})
		}
	}
	return NewManager(o)
}
