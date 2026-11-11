package netcfg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EthernetSettings is the wired network configuration.
type EthernetSettings struct {
	// Mode: "auto" (DHCP when a cable is plugged in), "dhcp" (DHCP required,
	// IPv6 off), "static", or "off".
	Mode    string   `json:"mode"`
	Address string   `json:"address,omitempty"` // static: 192.168.1.50/24
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// Saved is what the web UI persists (in the daemon's system.json). Settings in
// emm.txt override the matching fields on every boot.
type Saved struct {
	Hostname     string           `json:"hostname,omitempty"`
	WifiSSID     string           `json:"wifi_ssid,omitempty"`
	WifiPassword string           `json:"wifi_password,omitempty"`
	WifiHidden   bool             `json:"wifi_hidden,omitempty"` // not in the scan when chosen: its profile probes for it (set by Connect)
	WifiCountry  string           `json:"wifi_country,omitempty"`
	Ethernet     EthernetSettings `json:"ethernet"`
}

// Timing holds every wait, so tests and the fake can shorten them.
// Zero fields take the defaults noted.
type Timing struct {
	NMReady           time.Duration // wait for NetworkManager to appear (30 s)
	EthernetWait      time.Duration // wait for a cable's DHCP when no WiFi is configured (15 s)
	EthernetNoCarrier time.Duration // give up early when no cable is plugged in (5 s)
	WifiWait          time.Duration // wait for a configured WiFi network (45 s)
	WifiDeviceWait    time.Duration // wait for the WiFi adapter itself to appear, its driver may still be loading (30 s)
	Poll              time.Duration // poll interval while waiting (1 s)
	Monitor           time.Duration // background status refresh (5 s)
	SetupEthCheck     time.Duration // how often setup mode looks for Ethernet (30 s)
	SetupRetry        time.Duration // how often setup mode retries the configured WiFi while nobody is connected to the AP (3 min)
	ConnectDelay      time.Duration // pause between Connect() returning and the AP going down (2 s)
}

func (t Timing) withDefaults() Timing {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.NMReady, 30*time.Second)
	def(&t.EthernetWait, 15*time.Second)
	def(&t.EthernetNoCarrier, 5*time.Second)
	def(&t.WifiWait, 45*time.Second)
	def(&t.WifiDeviceWait, 30*time.Second)
	def(&t.Poll, time.Second)
	def(&t.Monitor, 5*time.Second)
	def(&t.SetupEthCheck, 30*time.Second)
	def(&t.SetupRetry, 3*time.Minute)
	def(&t.ConnectDelay, 2*time.Second)
	return t
}

// Options configures a Manager.
type Options struct {
	// BootDir holds emm.txt and reset-network (the FAT32 partition). Empty
	// means there is no boot file.
	BootDir string
	// NMDir is where NetworkManager keyfiles are written
	// (default /etc/NetworkManager/system-connections).
	NMDir string
	// DNSMasqDir is where the captive-DNS snippet goes
	// (default /etc/NetworkManager/dnsmasq-shared.d).
	DNSMasqDir string
	// HostsFile is the hosts file whose 127.0.1.1 line follows a rename, so that
	// sudo can still resolve the machine's own name (default /etc/hosts). Tests
	// point it at a temp file.
	HostsFile string
	// WifiIface / EthIface override device detection.
	WifiIface, EthIface string

	Runner Runner
	// Load returns the saved UI settings; Save persists them. Either may be nil.
	Load func() Saved
	Save func(Saved) error
	// OnStatusChange is called (outside locks) when the Status changes.
	OnStatusChange func()
	Logger         *slog.Logger

	// RelaxedPSK skips the WPA passphrase length rules (8-63 characters).
	// NewFake sets it so that a UI developer can type "wrong" to see a failed
	// join; real deployments must leave it off.
	RelaxedPSK bool

	Timing Timing
	// Now and Sleep exist for tests (fake clock). Sleep must return early with
	// ctx.Err() when ctx ends.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Errors returned by Manager methods.
var (
	// ErrBootManaged: the setting is fixed by emm.txt; edit that file instead.
	ErrBootManaged = errors.New("this setting is controlled by emm.txt on the SD card; edit or remove it there")
	// ErrNoWifi: no WiFi adapter was found.
	ErrNoWifi = errors.New("no WiFi adapter found")
	// ErrBusy: a WiFi join is already in progress (Connect was called again
	// before the previous one finished). A client should wait and retry; a web
	// layer should answer 409.
	ErrBusy = errors.New("a WiFi join is already in progress; wait for it to finish and try again")
)

// EthernetStatus is the wired part of Status.
type EthernetStatus struct {
	Link bool   `json:"link"` // a cable is plugged in (device is not "unavailable")
	IP   string `json:"ip,omitempty"`
	Mode string `json:"mode"`
}

// Status is a snapshot for the UI.
type Status struct {
	// Mode: "client" (on WiFi), "ethernet", "ap" (setup network up),
	// "connecting", "offline", or "unmanaged" (no NetworkManager).
	Mode      string `json:"mode"`
	SetupMode bool   `json:"setup_mode"`
	Joining   bool   `json:"joining"` // a Connect() is in progress
	SSID      string `json:"ssid,omitempty"`
	// Adopted: the WiFi connection in use was made outside EMM (Raspberry Pi
	// Imager, nmtui, raspi-config) and is left exactly as it is; SSID and IP are
	// NetworkManager's. Joining another network from the UI creates EMM's own
	// profile and ends the adoption.
	Adopted   bool           `json:"adopted,omitempty"`
	IP        string         `json:"ip,omitempty"`
	Hostname  string         `json:"hostname"` // emm.txt's or the UI's, else the machine's own; never empty
	APSSID    string         `json:"ap_ssid,omitempty"`
	LastError string         `json:"last_error,omitempty"`
	Ethernet  EthernetStatus `json:"ethernet"`
	ScanAge   int            `json:"scan_age"` // seconds since the cached scan, -1 = never
	// BootManaged lists which settings emm.txt fixes ("hostname", "wifi",
	// "ethernet", "wifi_country"); the UI should show them as read-only.
	BootManaged []string `json:"boot_managed,omitempty"`
	// Warnings are problems found in emm.txt.
	Warnings []string `json:"warnings,omitempty"`
}

// effective is the merge of emm.txt and saved settings.
type effective struct {
	Hostname, Country string // Hostname "" = leave the system's name alone
	SSID, PSK         string
	Hidden            bool // from Saved.WifiHidden; emm.txt networks are judged by the scan cache
	Eth               EthernetSettings
	// EthExplicit: somebody configured the wired network, either emm.txt (any
	// `ethernet` value, "auto" included) or the saved UI settings with a mode
	// other than the plain default. Only then is emm-eth written whatever else
	// NetworkManager has for the cable (see renderEthernetProfile).
	EthExplicit   bool
	APSSID, APPSK string
}

// mergeConfig combines emm.txt and the saved settings. Hostname stays empty
// when neither sets one: the machine keeps the name it has.
func mergeConfig(b BootConfig, s Saved) effective {
	var e effective
	if s.Hostname != "" {
		e.Hostname = s.Hostname
	}
	if b.Hostname != "" {
		e.Hostname = b.Hostname
	}
	e.Country = s.WifiCountry
	if b.WifiCountry != "" {
		e.Country = b.WifiCountry
	}
	if b.WifiSSID != "" {
		e.SSID, e.PSK = b.WifiSSID, b.WifiPassword
	} else {
		e.SSID, e.PSK, e.Hidden = s.WifiSSID, s.WifiPassword, s.WifiHidden
	}
	if b.Ethernet != "" {
		e.Eth = EthernetSettings{Mode: b.Ethernet, Address: b.EthAddress, Gateway: b.EthGateway, DNS: b.EthDNS}
		e.EthExplicit = true
	} else {
		e.Eth = s.Ethernet
		// The UI's plain "auto" is what an untouched box has: not a choice.
		mode := strings.ToLower(strings.TrimSpace(s.Ethernet.Mode))
		e.EthExplicit = mode != "" && mode != "auto"
	}
	if e.Eth.Mode == "" {
		e.Eth.Mode = "auto"
	}
	e.APSSID, e.APPSK = b.APSSID, b.APPassword
	return e
}

// Manager owns the machine's network configuration.
//
// UNVERIFIED on hardware: everything that talks to NetworkManager (nmcli
// output formats, keyfile acceptance, AP + dnsmasq behaviour) is tested only
// against a fake. See the package documentation.
type Manager struct {
	o   Options
	log *slog.Logger
	t   Timing

	opMu sync.Mutex // serialises state transitions (startup, Connect, scans, …)

	mu       sync.Mutex // guards everything below
	boot     BootConfig
	saved    Saved
	eff      effective
	mode     string
	setup    bool
	joining  bool
	ssid     string
	ip       string
	apSSID   string
	lastErr  string
	eth      EthernetStatus
	scan     []Network
	scanAt   time.Time
	wifiDev  string
	ethDev   string
	runCtx   context.Context
	lastKey  string
	setupChk time.Time
	cfgWarn  []string // problems with the effective config (not cleared when we get online)
	sysHost  string   // the machine's own host name (lower-case first label): what Status reports when emm.txt and the UI set none

	// Connections somebody else made (EMM only ever creates and deletes emm-*).
	foreignAuto  []string // WiFi profiles NetworkManager may bring up by itself, seen at boot
	foreignWifi  string   // the foreign WiFi profile last seen in use: where a failed join goes back to
	adopted      bool     // the WiFi connection in use is a foreign one
	radioApplied bool     // rfkill and the regulatory domain were applied to the current WiFi device
	wifiAppeared bool     // a WiFi device showed up after boot; monitorTick acts on it

	// Setup mode bookkeeping.
	setupRetryAt time.Time // when the configured WiFi was last tried (or last skipped), see maybeRetryWifi
	forcedSetup  bool      // setup mode was asked for with reset-network: never retry the old WiFi
	apFails      int       // consecutive failed attempts to restore the AP (monitor loop)
	apRetryAt    time.Time // earliest next attempt
	failMsg      string    // the last message given to fail()

	bg sync.WaitGroup // background joins; tests wait on it
}

// NewManager creates a Manager. Call Run to start it.
func NewManager(o Options) *Manager {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.NMDir == "" {
		o.NMDir = "/etc/NetworkManager/system-connections"
	}
	if o.DNSMasqDir == "" {
		o.DNSMasqDir = "/etc/NetworkManager/dnsmasq-shared.d"
	}
	if o.HostsFile == "" {
		o.HostsFile = "/etc/hosts"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = realSleep
	}
	m := &Manager{o: o, log: o.Logger.With("component", "netcfg"), t: o.Timing.withDefaults(), mode: "connecting"}
	if o.Load != nil {
		m.saved = o.Load()
	}
	m.eff = mergeConfig(BootConfig{}, m.saved)
	m.eth.Mode = m.eff.Eth.Mode
	// Until Run has read it again: the web layer asks for the name from the start.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.sysHost = m.systemHostname(ctx)
	return m
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (m *Manager) now() time.Time { return m.o.Now() }
func (m *Manager) sleep(ctx context.Context, d time.Duration) error {
	return m.o.Sleep(ctx, d)
}

// ---- small helpers ---------------------------------------------------------

func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	m.log.Debug("exec", "cmd", name, "args", strings.Join(args, " "))
	return m.o.Runner.Run(ctx, name, args...)
}

func (m *Manager) nmcli(ctx context.Context, args ...string) (string, error) {
	return m.run(ctx, "nmcli", args...)
}

func (m *Manager) baseCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runCtx != nil {
		return m.runCtx
	}
	return context.Background()
}

// set mutates state under the lock and fires OnStatusChange if it changed.
func (m *Manager) set(fn func()) {
	m.mu.Lock()
	fn()
	key := m.keyLocked()
	changed := key != m.lastKey
	m.lastKey = key
	m.mu.Unlock()
	if changed && m.o.OnStatusChange != nil {
		m.o.OnStatusChange()
	}
}

func (m *Manager) keyLocked() string {
	return strings.Join([]string{m.mode, strconv.FormatBool(m.setup), strconv.FormatBool(m.joining),
		m.ssid, m.ip, m.apSSID, m.lastErr, strconv.FormatBool(m.eth.Link), m.eth.IP, m.eth.Mode, m.hostnameLocked(),
		strconv.FormatBool(m.adopted)}, "|")
}

func (m *Manager) persist(s Saved) error {
	if m.o.Save != nil {
		return m.o.Save(s)
	}
	return nil
}

// checkPSK validates a passphrase under the manager's rules.
func (m *Manager) checkPSK(p string) error {
	if m.o.RelaxedPSK {
		return nil
	}
	return validatePSK(p)
}

func (m *Manager) renderWifi(eff effective) (string, error) {
	return renderWifiProfile(eff.SSID, eff.PSK, m.o.RelaxedPSK, m.wifiHidden(eff))
}

// wifiHidden decides whether the client profile for eff gets hidden=true. A
// network chosen through the UI carries the decision Connect made (it was not
// in the scan). One fixed by emm.txt has no such record, so it is judged by the
// cached scan, if there is one: absent from the scan means hidden. Before any
// scan there is nothing to judge by and the profile is written as visible.
func (m *Manager) wifiHidden(eff effective) bool {
	if eff.Hidden {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.boot.WifiSSID == "" || len(m.scan) == 0 {
		return false
	}
	return !scanHas(m.scan, eff.SSID)
}

func scanHas(nets []Network, ssid string) bool {
	for _, n := range nets {
		if n.SSID == ssid {
			return true
		}
	}
	return false
}

// updateSaved stores s as the saved settings and recomputes the effective ones.
func (m *Manager) updateSaved(s Saved) {
	m.set(func() {
		m.saved = s
		m.eff = mergeConfig(m.boot, s)
		m.eth.Mode = m.eff.Eth.Mode
	})
}

func (m *Manager) writeProfile(id, content string) error {
	if err := os.MkdirAll(m.o.NMDir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(m.o.NMDir, id+".nmconnection"), []byte(content), 0o600)
}

func (m *Manager) removeProfile(id string) {
	_ = os.Remove(filepath.Join(m.o.NMDir, id+".nmconnection"))
}

// ensureProfile writes one of EMM's profiles unless it is there as it should be,
// and reports whether it wrote anything (then NetworkManager needs a reload).
func (m *Manager) ensureProfile(id, content string) (bool, error) {
	if cur, err := os.ReadFile(filepath.Join(m.o.NMDir, id+".nmconnection")); err == nil && string(cur) == content {
		return false, nil
	}
	return true, m.writeProfile(id, content)
}

func (m *Manager) writeDNSSnippet() error {
	if err := os.MkdirAll(m.o.DNSMasqDir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(m.o.DNSMasqDir, dnsmasqSnippet), []byte(dnsmasqWildcard()), 0o644)
}

func (m *Manager) removeDNSSnippet() {
	_ = os.Remove(filepath.Join(m.o.DNSMasqDir, dnsmasqSnippet))
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) reload(ctx context.Context) {
	if _, err := m.nmcli(ctx, "connection", "reload"); err != nil {
		m.log.Warn("nmcli connection reload failed", "err", err)
	}
}

// ---- probing ---------------------------------------------------------------

type snapshot struct {
	wifi, eth       nmDevice
	hasWifi, hasEth bool
	wifiIP, ethIP   string
	wifiSSID        string // the network of a foreign WiFi connection (the profile's name need not match it)
	anyConnecting   bool
}

func (m *Manager) probe(ctx context.Context) (snapshot, error) {
	var s snapshot
	out, err := m.nmcli(ctx, "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device")
	if err != nil {
		return s, err
	}
	devs := parseDevices(out)
	wd, ed := pickDevices(devs, m.o.WifiIface, m.o.EthIface)
	s.wifi, s.hasWifi = findDevice(devs, wd)
	s.eth, s.hasEth = findDevice(devs, ed)
	if !s.hasWifi {
		wd = "" // a device NetworkManager does not list (yet) is no device
	}
	m.mu.Lock()
	if wd == "" {
		m.radioApplied = false
	} else if m.wifiDev == "" {
		m.wifiAppeared = true
	}
	m.wifiDev, m.ethDev = wd, ed
	m.mu.Unlock()
	for _, d := range devs {
		if d.State == "connecting" {
			s.anyConnecting = true
		}
	}
	if s.hasWifi && s.wifi.State == "connected" {
		o, _ := m.nmcli(ctx, "-g", "IP4.ADDRESS", "device", "show", wd)
		s.wifiIP = parseIPv4(o)
	}
	if s.hasEth && s.eth.State == "connected" {
		o, _ := m.nmcli(ctx, "-g", "IP4.ADDRESS", "device", "show", ed)
		s.ethIP = parseIPv4(o)
	}
	if s.hasWifi && s.wifi.State == "connected" && s.wifi.Conn != "" && !isEMMConn(s.wifi.Conn) {
		o, _ := m.nmcli(ctx, "-g", "802-11-wireless.ssid", "connection", "show", "id", s.wifi.Conn)
		line, _, _ := strings.Cut(o, "\n")
		s.wifiSSID = unescapeTerse(strings.TrimSuffix(line, "\r"))
	}
	return s, nil
}

func wifiConnected(s snapshot) bool {
	return s.hasWifi && s.wifi.State == "connected" && s.wifi.Conn != apConnID
}

func (m *Manager) ethOnline(s snapshot) bool {
	m.mu.Lock()
	off := m.eff.Eth.Mode == "off"
	m.mu.Unlock()
	return !off && s.hasEth && s.eth.State == "connected" && s.ethIP != ""
}

func ethLink(s snapshot) bool {
	return s.hasEth && s.eth.State != "unavailable" && s.eth.State != "unmanaged"
}

// applySnapshot folds a probe into the status. In setup mode and while a join
// is running the mode is owned by those flows and only the wired fields move.
func (m *Manager) applySnapshot(s snapshot) {
	ethOK := m.ethOnline(s)
	m.set(func() {
		m.eth.Link = ethLink(s)
		m.eth.IP = ""
		if ethOK {
			m.eth.IP = s.ethIP
		}
		if m.setup || m.joining {
			return
		}
		switch {
		case wifiConnected(s):
			m.mode, m.ip = "client", s.wifiIP
			m.ssid, m.adopted = m.eff.SSID, false
			if c := s.wifi.Conn; c != "" && c != wifiConnID {
				// A profile somebody else made (Imager, nmtui, ...): report what
				// NetworkManager says, and remember it as the way back should a
				// join through the UI fail.
				m.ssid = c
				if s.wifiSSID != "" {
					m.ssid = s.wifiSSID
				}
				if !isEMMConn(c) {
					m.adopted, m.foreignWifi = true, c
				}
			}
		case ethOK:
			m.mode, m.ip, m.ssid = "ethernet", s.ethIP, ""
			if m.lastErr == ErrNoWifi.Error() {
				m.lastErr = "" // a box with no radio that is online by cable has nothing to report
			}
		case s.anyConnecting:
			m.mode, m.ip, m.ssid = "connecting", "", ""
		default:
			m.mode, m.ip, m.ssid = "offline", "", ""
		}
	})
}

// ---- public read API -------------------------------------------------------

// Status returns the current status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Mode: m.mode, SetupMode: m.setup, Joining: m.joining,
		SSID: m.ssid, IP: m.ip, Hostname: m.hostnameLocked(), APSSID: m.apSSID,
		Adopted:   m.adopted && m.mode == "client" && !m.setup,
		LastError: m.lastErr, Ethernet: m.eth, ScanAge: -1,
		Warnings: append(append([]string(nil), m.boot.Warnings...), m.cfgWarn...),
	}
	if !m.scanAt.IsZero() {
		st.ScanAge = int(m.now().Sub(m.scanAt).Seconds())
		if st.ScanAge < 0 {
			st.ScanAge = 0
		}
	}
	if m.boot.Hostname != "" {
		st.BootManaged = append(st.BootManaged, "hostname")
	}
	if m.boot.WifiSSID != "" {
		st.BootManaged = append(st.BootManaged, "wifi")
	}
	if m.boot.Ethernet != "" {
		st.BootManaged = append(st.BootManaged, "ethernet")
	}
	if m.boot.WifiCountry != "" {
		st.BootManaged = append(st.BootManaged, "wifi_country")
	}
	return st
}

// InSetupMode reports whether the setup access point is (meant to be) up.
func (m *Manager) InSetupMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setup
}

// PortalURL is where a browser should be sent: the setup page on the AP
// address in setup mode, otherwise the mDNS name.
func (m *Manager) PortalURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setup {
		return "http://" + APAddress + "/"
	}
	return "http://" + m.hostnameLocked() + ".local/"
}

// hostnameLocked is the name the box answers to: the one emm.txt or the UI set,
// else the machine's own. It is never empty (the web layer builds its Host
// allow-list from it).
func (m *Manager) hostnameLocked() string {
	switch {
	case m.eff.Hostname != "":
		return m.eff.Hostname
	case m.sysHost != "":
		return m.sysHost
	}
	return DefaultHostname
}

// Boot returns what emm.txt said.
func (m *Manager) Boot() BootConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.boot
	b.EthDNS = append([]string(nil), b.EthDNS...)
	b.Warnings = append([]string(nil), b.Warnings...)
	if b.RTP != nil {
		v := *b.RTP
		b.RTP = &v
	}
	if b.BLE != nil {
		v := *b.BLE
		b.BLE = &v
	}
	return b
}

// ---- Run / startup ---------------------------------------------------------

// Run configures the network at boot and then keeps the status fresh until ctx
// ends.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()

	m.opMu.Lock()
	m.startup(ctx)
	m.opMu.Unlock()

	for {
		if err := m.sleep(ctx, m.t.Monitor); err != nil {
			m.bg.Wait()
			return ctx.Err()
		}
		m.monitorTick(ctx)
	}
}

func (m *Manager) loadBoot() BootConfig {
	if m.o.BootDir == "" {
		return BootConfig{}
	}
	b, err := LoadBootConfig(m.o.BootDir)
	if err != nil {
		m.log.Warn("could not read boot config", "err", err)
		b.Warnings = append(b.Warnings, fmt.Sprintf("%s could not be read: %v", BootFileName, err))
	}
	for _, w := range b.Warnings {
		m.log.Warn("emm.txt", "warning", w)
	}
	return b
}

// takeResetFile reports whether reset-network exists, and removes it
// (best effort: the FAT partition may be read-only).
func (m *Manager) takeResetFile() bool {
	if m.o.BootDir == "" {
		return false
	}
	p := filepath.Join(m.o.BootDir, ResetFileName)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	if err := os.Remove(p); err != nil {
		m.log.Warn("could not delete reset-network", "err", err)
	}
	return true
}

func (m *Manager) waitNM(ctx context.Context) bool {
	deadline := m.now().Add(m.t.NMReady)
	for {
		_, err := m.nmcli(ctx, "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device")
		if err == nil {
			return true
		}
		if isNotFound(err) || ctx.Err() != nil || !m.now().Before(deadline) {
			m.log.Warn("NetworkManager is not available; network left unmanaged", "err", err)
			return false
		}
		if m.sleep(ctx, m.t.Poll) != nil {
			return false
		}
	}
}

// startup runs the boot sequence. opMu is held.
func (m *Manager) startup(ctx context.Context) {
	defer func() {
		// A device seen during boot was dealt with by the boot sequence itself.
		m.mu.Lock()
		m.wifiAppeared = false
		m.mu.Unlock()
	}()
	boot := m.loadBoot()
	reset := m.takeResetFile()
	var saved Saved
	if m.o.Load != nil {
		saved = m.o.Load()
	}
	m.set(func() {
		m.boot, m.saved = boot, saved
		m.eff = mergeConfig(boot, saved)
		m.eth.Mode = m.eff.Eth.Mode
		m.mode, m.setup, m.joining, m.lastErr = "connecting", false, false, ""
		m.adopted = false
		m.cfgWarn = nil
		m.forcedSetup = reset
	})
	if reset {
		m.log.Info("reset-network found: starting in setup mode")
	}

	if !m.waitNM(ctx) {
		m.set(func() { m.mode = "unmanaged" })
		return
	}
	if _, err := m.probe(ctx); err != nil {
		m.log.Warn("device probe failed", "err", err)
	}
	m.refreshHostname(ctx)

	m.mu.Lock()
	eff := m.eff
	m.mu.Unlock()

	// Regulatory domain and rfkill first: WiFi (and especially the AP) needs
	// them. Without a WiFi device yet they wait for it (syncRadio).
	m.syncRadio(ctx)
	// The host name is changed only when emm.txt or the UI chose one: a Pi OS
	// box keeps the name its owner gave it.
	if eff.Hostname != "" {
		m.applyHostname(ctx, eff.Hostname)
	}

	// Derived profiles: start from a clean slate (on Raspberry Pi OS /etc persists).
	// Only EMM's own emm-* profiles are ever touched, never somebody else's.
	m.removeProfile(apConnID)
	m.removeDNSSnippet()
	m.renderEthernetProfile(ctx, eff)
	wifiCfg := !reset && eff.SSID != ""
	if wifiCfg {
		if content, err := m.renderWifi(eff); err != nil {
			m.log.Warn("WiFi settings unusable", "err", err)
			m.set(func() {
				m.lastErr = "WiFi settings unusable: " + err.Error()
				m.cfgWarn = append(m.cfgWarn, m.lastErr)
			})
			wifiCfg = false
			m.removeProfile(wifiConnID)
		} else if err := m.writeProfile(wifiConnID, content); err != nil {
			m.log.Warn("could not write WiFi profile", "err", err)
			wifiCfg = false
		}
	} else {
		m.removeProfile(wifiConnID)
	}
	m.reload(ctx)
	if eff.Eth.Mode == "off" {
		m.disconnectEthernet(ctx)
	}

	m.settle(ctx, waitPlan{
		forced:  reset,
		wifiCfg: wifiCfg,
		// Nothing of EMM's own to bring up: whatever connection is already there
		// (Imager's WiFi, an nmtui profile, a cable) is used as it is.
		adopt: !reset && eff.SSID == "",
	})
}

// waitPlan says what the boot sequence (or a WiFi device appearing late) is
// waiting for.
type waitPlan struct {
	wifiCfg bool // EMM's own WiFi profile is in place: wait for it to join
	adopt   bool // EMM configures no WiFi: a foreign WiFi connection counts as online
	forced  bool // reset-network: setup mode whatever is online; only the radio is waited for
}

// settle waits for connectivity and enters setup mode when there is none. opMu
// is held.
func (m *Manager) settle(ctx context.Context, p waitPlan) {
	if p.forced {
		m.waitWifiDevice(ctx)
		if ctx.Err() == nil {
			m.noWifiIsOver()
			m.enterSetupMode(ctx)
		}
		return
	}
	m.loadForeignWifi(ctx)
	snap, online := m.waitOnline(ctx, p)
	if ctx.Err() != nil {
		return
	}
	if online {
		m.set(func() { m.setup, m.lastErr = false, "" })
		m.applySnapshot(snap)
		return
	}
	m.mu.Lock()
	haveWifi := m.wifiDev != ""
	fromFile := m.boot.WifiSSID != ""
	m.mu.Unlock()
	m.noWifiIsOver()
	if p.wifiCfg && haveWifi {
		msg := fmt.Sprintf("Could not join WiFi network %q", m.SavedSSID())
		if fromFile {
			msg += " (set in emm.txt)"
		}
		m.set(func() { m.lastErr = msg })
	}
	// Without a WiFi device enterSetupMode says so (ErrNoWifi) instead.
	m.enterSetupMode(ctx)
}

// noWifiIsOver drops the "no WiFi adapter" complaint of an earlier pass once
// there is a device.
func (m *Manager) noWifiIsOver() {
	m.set(func() {
		if m.wifiDev != "" && m.lastErr == ErrNoWifi.Error() {
			m.lastErr = ""
		}
	})
}

// waitOnline waits for something to bring the box online: a cable's DHCP lease,
// the configured WiFi, or (when nothing is configured) a WiFi connection that
// somebody else made. It also waits for the WiFi device to exist, as its driver
// may still be loading when emmd starts (Timing.WifiDeviceWait): Ethernet being
// online ends the wait at once, and a device that turns up later is picked up
// by the monitor loop.
func (m *Manager) waitOnline(ctx context.Context, p waitPlan) (snapshot, bool) {
	start := m.now()
	var devAt time.Time // when the WiFi device was first seen; WiFi waits count from there
	for {
		snap, err := m.probe(ctx)
		if err == nil {
			m.applySnapshot(snap)
			m.syncRadio(ctx)
			if snap.hasWifi && devAt.IsZero() {
				devAt = m.now()
			}
			if m.ethOnline(snap) || ((p.wifiCfg || p.adopt) && wifiConnected(snap)) {
				return snap, true
			}
		}
		if ctx.Err() != nil {
			return snap, false
		}
		now := m.now()
		el := now.Sub(start)
		sinceDev := el
		if !devAt.IsZero() {
			sinceDev = now.Sub(devAt)
		}
		switch {
		case err == nil && !snap.hasWifi:
			// No radio (yet): whatever is configured for WiFi has to wait for it.
			if el >= m.t.WifiDeviceWait {
				return snap, false
			}
		case p.wifiCfg:
			if sinceDev >= m.t.WifiWait {
				return snap, false
			}
		default:
			limit := m.t.EthernetWait
			if err == nil && !ethLink(snap) {
				limit = m.t.EthernetNoCarrier
			}
			joining := p.adopt && err == nil && m.wifiMayJoin(snap) && sinceDev < m.t.WifiWait
			if el >= limit && !joining {
				return snap, false
			}
		}
		if m.sleep(ctx, m.t.Poll) != nil {
			return snap, false
		}
	}
}

// wifiMayJoin reports whether NetworkManager is (or soon will be) bringing up a
// WiFi connection that EMM did not make: the device is activating one, or there
// is a profile with autoconnect on. Without this a Pi that boots into its own
// WiFi would be given up on after the 5 s a missing cable is worth.
func (m *Manager) wifiMayJoin(s snapshot) bool {
	if !s.hasWifi || s.wifi.State == "unmanaged" {
		return false
	}
	if s.wifi.State == "connecting" && s.wifi.Conn != apConnID {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.foreignAuto) > 0
}

// listProfiles asks NetworkManager for its connection profiles.
func (m *Manager) listProfiles(ctx context.Context) ([]nmProfile, error) {
	out, err := m.nmcli(ctx, "-t", "-f", "NAME,TYPE,AUTOCONNECT", "connection", "show")
	if err != nil {
		return nil, err
	}
	return parseProfiles(out), nil
}

// loadForeignWifi notes the WiFi profiles somebody else made that
// NetworkManager may autoconnect (see wifiMayJoin). A failure only means the
// wait is not extended.
func (m *Manager) loadForeignWifi(ctx context.Context) {
	ps, err := m.listProfiles(ctx)
	if err != nil {
		m.log.Debug("could not list connection profiles", "err", err)
		return
	}
	names := foreignWifiProfiles(ps)
	m.mu.Lock()
	m.foreignAuto = names
	if m.foreignWifi == "" && len(names) > 0 {
		m.foreignWifi = names[0]
	}
	m.mu.Unlock()
}

// waitWifiDevice polls for the WiFi device, for at most Timing.WifiDeviceWait.
// It is all reset-network has to wait for: setup mode is forced, whatever else
// is online.
func (m *Manager) waitWifiDevice(ctx context.Context) {
	start := m.now()
	for {
		snap, err := m.probe(ctx)
		if err == nil {
			m.syncRadio(ctx)
			if snap.hasWifi {
				return
			}
		}
		if ctx.Err() != nil || m.now().Sub(start) >= m.t.WifiDeviceWait {
			return
		}
		if m.sleep(ctx, m.t.Poll) != nil {
			return
		}
	}
}

// syncRadio unblocks rfkill and sets the regulatory domain once per WiFi
// device: right away when there is one, otherwise when it appears (cfg80211 and
// the rfkill switch exist only then; before that both commands would fail).
func (m *Manager) syncRadio(ctx context.Context) {
	m.mu.Lock()
	need := m.wifiDev != "" && !m.radioApplied
	country := m.eff.Country
	if need {
		m.radioApplied = true
	}
	m.mu.Unlock()
	if !need {
		return
	}
	_, _ = m.run(ctx, "rfkill", "unblock", "wifi")
	if country != "" {
		if _, err := m.run(ctx, "iw", "reg", "set", country); err != nil {
			m.log.Warn("could not set regulatory domain", "country", country, "err", err)
		}
	}
}

// lateWifi runs when a WiFi device appears after boot (a driver module that
// loaded late, a dongle plugged in): it does what the boot sequence would have
// done with the device there. opMu is held.
func (m *Manager) lateWifi(ctx context.Context) {
	m.mu.Lock()
	eff, forced := m.eff, m.forcedSetup
	m.mu.Unlock()
	m.log.Info("a WiFi device appeared")
	p := waitPlan{forced: forced}
	if !forced {
		p.adopt = eff.SSID == ""
		if eff.SSID != "" {
			if content, err := m.renderWifi(eff); err == nil {
				if changed, err := m.ensureProfile(wifiConnID, content); err == nil {
					p.wifiCfg = true
					if changed {
						m.reload(ctx)
					}
				}
			}
		}
	}
	m.settle(ctx, p)
}

// renderEthernetProfile writes EMM's Ethernet profile, emm-eth, at boot. Its
// autoconnect priority (100) beats anything a user made, so on a box that has a
// profile of its own for the cable (a static address set with nmtui, say) the
// default one must stay away or it would take over at the next boot. It is
// written when
//
//   - Ethernet is explicitly configured (eff.EthExplicit), or
//   - NetworkManager has no Ethernet profile of anybody else's: the appliance
//     image has none, and there the default DHCP profile is what makes a cable
//     just work.
//
// Otherwise it is not written, and a stale emm-eth of the default kind is
// removed. If the profiles cannot be listed, emm-eth is left exactly as it is.
func (m *Manager) renderEthernetProfile(ctx context.Context, eff effective) {
	if !eff.EthExplicit {
		ps, err := m.listProfiles(ctx)
		if err != nil {
			m.log.Warn("could not list connection profiles; leaving the Ethernet profile as it is", "err", err)
			return
		}
		if names := foreignEthernetProfiles(ps); len(names) > 0 {
			m.log.Info("Ethernet is set up by another profile; not writing "+ethConnID, "profiles", strings.Join(names, ", "))
			m.dropDefaultEthernetProfile()
			return
		}
	}
	content, err := renderEthernet(eff.Eth)
	if err != nil {
		m.log.Warn("ethernet settings unusable, falling back to DHCP", "err", err)
		m.set(func() { m.cfgWarn = append(m.cfgWarn, "Ethernet settings unusable (using DHCP): "+err.Error()) })
		content, _ = renderEthernet(EthernetSettings{Mode: "auto"})
	}
	if err := m.writeProfile(ethConnID, content); err != nil {
		m.log.Warn("could not write Ethernet profile", "err", err)
	}
}

// dropDefaultEthernetProfile removes an emm-eth that an earlier start wrote when
// nothing else had the cable, now that somebody else does. Only the default
// profile (what renderEthernet makes for an unconfigured box) goes: one that
// carries settings (a static address, DHCP-required, off) was somebody's
// choice and stays.
func (m *Manager) dropDefaultEthernetProfile() {
	cur, err := os.ReadFile(filepath.Join(m.o.NMDir, ethConnID+".nmconnection"))
	if err != nil {
		return
	}
	def, _ := renderEthernet(EthernetSettings{Mode: "auto"})
	if string(cur) != def {
		m.log.Info(ethConnID + " carries settings of its own; leaving it")
		return
	}
	m.log.Info("removing the default " + ethConnID)
	m.removeProfile(ethConnID)
}

func (m *Manager) disconnectEthernet(ctx context.Context) {
	m.mu.Lock()
	dev := m.ethDev
	m.mu.Unlock()
	if dev == "" {
		return
	}
	if _, err := m.nmcli(ctx, "device", "disconnect", dev); err != nil {
		m.log.Debug("ethernet disconnect", "err", err)
	}
}

// applyHostname gives the machine the name h (one that emm.txt or the UI chose).
func (m *Manager) applyHostname(ctx context.Context, h string) {
	if !ValidHostname(h) {
		return
	}
	cur, _ := m.run(ctx, "hostname")
	if strings.TrimSpace(cur) == h {
		m.set(func() { m.sysHost = h })
		return
	}
	if _, err := m.run(ctx, "hostnamectl", "set-hostname", h); err != nil {
		if _, err2 := m.run(ctx, "hostname", h); err2 != nil {
			m.log.Warn("could not set host name", "err", err, "err2", err2)
			return
		}
	}
	m.set(func() { m.sysHost = h })
	m.syncHostsFile(h)
	// Make Avahi announce the new name (best effort).
	if _, err := m.run(ctx, "systemctl", "restart", "avahi-daemon"); err != nil {
		m.log.Debug("avahi restart", "err", err)
	}
}

// hostsAlias is the address Debian and Raspberry Pi OS give the machine's own
// name in /etc/hosts.
const hostsAlias = "127.0.1.1"

// syncHostsFile makes the 127.0.1.1 line of the hosts file name the machine h
// after a rename: without it sudo prints "unable to resolve host" until the
// next reboot (and keeps doing so on a Pi OS box whose /etc persists). Best
// effort, like the avahi restart: whatever goes wrong is only logged, and no
// line but the 127.0.1.1 one is ever touched. A hosts file that cannot be read
// is left alone, not created.
func (m *Manager) syncHostsFile(h string) {
	path := m.o.HostsFile
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved // replace what the link points to, not the link
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		m.log.Debug("hosts file not updated", "path", path, "err", err)
		return
	}
	next := hostsWithName(string(cur), h)
	if next == string(cur) {
		return
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := writeFileAtomic(path, []byte(next), perm); err != nil {
		m.log.Debug("hosts file not updated", "path", path, "err", err)
	}
}

// hostsWithName returns content with the names of its first 127.0.1.1 line
// replaced by name (the address, its spacing, the line ending and a trailing
// comment stay), or with "127.0.1.1<TAB>name" appended if there is no such
// line. Every other byte is kept as it was.
func hostsWithName(content, name string) string {
	lines := strings.SplitAfter(content, "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		trimmed := strings.TrimLeft(body, " \t")
		indent := body[:len(body)-len(trimmed)]
		if !strings.HasPrefix(trimmed, hostsAlias) {
			continue
		}
		rest := trimmed[len(hostsAlias):]
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue // 127.0.1.10, not 127.0.1.1
		}
		sep := rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
		if sep == "" {
			sep = "\t"
		}
		comment := ""
		if j := strings.IndexByte(rest, '#'); j >= 0 {
			comment = " " + rest[j:]
		}
		lines[i] = indent + hostsAlias + sep + name + comment + eol
		return strings.Join(lines, "")
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + hostsAlias + "\t" + name + "\n"
}

// systemHostname reads the machine's own host name through the Runner (so the
// fake can answer): the lower-case first label, or "" when it cannot be told or
// is not a usable name (like the kernel's "(none)"). On a real system whose
// `hostname` command is missing the kernel's answer is used.
func (m *Manager) systemHostname(ctx context.Context) string {
	out, err := m.run(ctx, "hostname")
	if err != nil {
		if _, onHost := m.o.Runner.(ExecRunner); !onHost {
			return ""
		}
		if out, err = os.Hostname(); err != nil {
			return ""
		}
	}
	return cleanHostname(out)
}

// cleanHostname reduces what `hostname` prints (possibly a fully qualified,
// mixed-case name) to a lower-case first label, or "" if that is not a valid
// host name.
func cleanHostname(s string) string {
	h, _, _ := strings.Cut(strings.TrimSpace(s), ".")
	h = strings.ToLower(h)
	if !ValidHostname(h) {
		return ""
	}
	return h
}

// refreshHostname re-reads the machine's own name, which somebody may have
// changed behind our back. It matters only while nothing in emm.txt or the UI
// chooses the name.
func (m *Manager) refreshHostname(ctx context.Context) {
	m.mu.Lock()
	chosen := m.eff.Hostname != ""
	m.mu.Unlock()
	if chosen {
		return
	}
	if h := m.systemHostname(ctx); h != "" {
		m.set(func() { m.sysHost = h })
	}
}

// ---- setup mode ------------------------------------------------------------

func (m *Manager) apName(ctx context.Context, eff effective) string {
	if eff.APSSID != "" {
		return eff.APSSID
	}
	m.mu.Lock()
	dev := m.wifiDev
	m.mu.Unlock()
	suffix := ""
	if dev != "" {
		if out, err := m.nmcli(ctx, "-g", "GENERAL.HWADDR", "device", "show", dev); err == nil {
			suffix = macSuffix(parseMAC(out))
		}
	}
	if suffix == "" {
		suffix = "0000"
	}
	return "EMM-Setup-" + suffix
}

// enterSetupMode scans, then brings the access point up. opMu is held.
func (m *Manager) enterSetupMode(ctx context.Context) {
	m.mu.Lock()
	dev := m.wifiDev
	eff := m.eff
	m.mu.Unlock()
	if dev == "" {
		// No radio, so no access point and nothing to serve a setup page on:
		// this is not setup mode. Reporting it as such would switch off the web
		// password and send browsers to an address nobody can reach. The device
		// is simply offline until a cable brings it up (monitorTick's snapshots
		// move the mode to "ethernet" then).
		m.set(func() {
			m.mode, m.setup, m.joining = "offline", false, false
			m.ssid, m.ip, m.apSSID = "", "", ""
			if m.lastErr == "" {
				m.lastErr = ErrNoWifi.Error()
			}
		})
		return
	}

	// No client profile while in setup mode, or NetworkManager may retry it
	// underneath a phone that is configuring us. Dropping it first also stops
	// a half-finished join, which would otherwise make the scan below fail.
	m.removeProfile(wifiConnID)
	_, _ = m.nmcli(ctx, "connection", "delete", wifiConnID)

	// A radio cannot scan reliably while it is serving an AP: scan first.
	if _, err := m.scanLocked(ctx, true); err != nil {
		m.log.Warn("pre-AP WiFi scan failed", "err", err)
	}
	_ = m.startAP(ctx, dev, eff)
}

// startAP writes the AP profile and captive DNS snippet and brings the access
// point up. On failure the status says so (fail) and the error is returned; the
// monitor loop then keeps trying (recoverAP). opMu is held.
func (m *Manager) startAP(ctx context.Context, dev string, eff effective) error {
	name := m.apName(ctx, eff)
	content, err := renderAP(name, eff.APPSK)
	if err != nil { // can't happen for validated input, but never leave the box unreachable
		m.log.Warn("AP settings unusable, using defaults", "err", err)
		name = m.apName(ctx, effective{})
		content, _ = renderAP(name, "")
	}
	if err := m.writeProfile(apConnID, content); err != nil {
		m.fail(fmt.Sprintf("could not write the setup network profile: %v", err), name)
		return err
	}
	if err := m.writeDNSSnippet(); err != nil {
		m.log.Warn("could not write captive DNS snippet", "err", err)
	}
	m.reload(ctx)
	if err := m.apUp(ctx, dev); err != nil {
		m.fail(fmt.Sprintf("could not start the setup network: %s", friendlyNMError(err)), name)
		return err
	}
	m.log.Info("setup mode: access point up", "ssid", name)
	m.set(func() {
		m.mode, m.setup, m.joining = "ap", true, false
		m.ssid, m.ip, m.apSSID = "", APAddress, name
		m.setupChk = m.now()
		m.setupRetryAt = m.now()
		m.apFails = 0
	})
	return nil
}

func (m *Manager) fail(msg, apName string) {
	m.log.Error("setup mode failed", "err", msg)
	m.set(func() {
		m.mode, m.setup, m.joining = "offline", true, false
		m.lastErr, m.apSSID, m.ip = msg, apName, ""
		m.failMsg = msg
		m.setupRetryAt = m.now() // the WiFi retry waits a full period after any trouble
	})
}

func (m *Manager) apUp(ctx context.Context, dev string) error {
	_, err := m.nmcli(ctx, "--wait", "30", "connection", "up", apConnID, "ifname", dev)
	return err
}

func (m *Manager) apDown(ctx context.Context) {
	if _, err := m.nmcli(ctx, "connection", "down", apConnID); err != nil {
		m.log.Debug("ap down", "err", err)
	}
}

// leaveSetup takes the AP down and cleans up its profile and DNS snippet.
func (m *Manager) leaveSetup(ctx context.Context) {
	m.apDown(ctx)
	m.removeProfile(apConnID)
	m.removeDNSSnippet()
	m.set(func() { m.apSSID = "" })
}

// apServing reports whether the probe shows the access point up (or coming up).
func apServing(s snapshot) bool {
	return s.hasWifi && s.wifi.Conn == apConnID && (s.wifi.State == "connected" || s.wifi.State == "connecting")
}

// apBackoff is the pause before the n-th (n >= 1) attempt to restore the AP.
func apBackoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < time.Minute; i++ {
		d *= 2
	}
	return min(d, time.Minute)
}

// recoverAP brings the access point back when it should be up and is not (a
// failed start or restore). Attempts back off from 5 s to 1 min. opMu is held.
func (m *Manager) recoverAP(ctx context.Context) {
	m.mu.Lock()
	dev, eff, at := m.wifiDev, m.eff, m.apRetryAt
	m.mu.Unlock()
	if dev == "" || m.now().Before(at) {
		return
	}
	m.log.Warn("setup mode: the access point is not up, starting it again")
	if err := m.startAP(ctx, dev, eff); err != nil {
		m.set(func() {
			m.apFails++
			m.apRetryAt = m.now().Add(apBackoff(m.apFails))
		})
		return
	}
	m.set(func() {
		if m.lastErr == m.failMsg {
			m.lastErr = "" // the error fail() reported is over
		}
	})
}

// retryJoinWait caps how long a setup-mode WiFi retry waits for the link: the
// AP is down meanwhile, and a network that is there joins within seconds.
const retryJoinWait = 20 * time.Second

// maybeRetryWifi tries the configured WiFi (emm.txt or saved) again from setup
// mode, so a box whose router booted slower than it did (power cut) finds its
// way back without a visit. It never disturbs anyone: the retry only happens
// when no station is associated with the access point, and a round in which one
// is only restarts the clock, so a phone that briefly drops gets a full period
// of grace. A retry that fails puts the access point back. Nothing is retried
// when setup mode was forced with reset-network, which promises to ignore the
// configured WiFi for that boot. opMu is held.
func (m *Manager) maybeRetryWifi(ctx context.Context) {
	m.mu.Lock()
	eff, dev, forced, last := m.eff, m.wifiDev, m.forcedSetup, m.setupRetryAt
	m.mu.Unlock()
	if eff.SSID == "" || dev == "" || forced || m.now().Sub(last) < m.t.SetupRetry {
		return
	}
	m.set(func() { m.setupRetryAt = m.now() })
	if _, err := m.renderWifi(eff); err != nil {
		return // unusable settings (startup said so): taking the AP down would achieve nothing
	}

	out, err := m.run(ctx, "iw", "dev", dev, "station", "dump")
	if err != nil {
		// Cannot tell whether somebody is connected: do not take the risk.
		m.log.Warn("setup mode: cannot list the stations on the access point, not retrying WiFi", "err", err)
		return
	}
	if n := countStations(out); n > 0 {
		m.log.Debug("setup mode: a client is connected, not retrying WiFi", "stations", n)
		return
	}

	m.log.Info("setup mode: nobody is connected, trying the configured WiFi again", "ssid", eff.SSID)
	m.set(func() { m.mode, m.ip = "connecting", "" })
	m.leaveSetup(ctx)
	err = m.applyWifiWait(ctx, min(m.t.WifiWait, retryJoinWait))
	if err == nil {
		m.log.Info("setup mode: joined the configured WiFi", "ssid", eff.SSID)
		m.set(func() { m.setup, m.forcedSetup, m.lastErr = false, false, "" })
		m.refresh(ctx)
		return
	}
	if ctx.Err() != nil {
		return
	}
	m.log.Info("setup mode: the configured WiFi is still not there", "ssid", eff.SSID, "err", err)
	m.removeProfile(wifiConnID)
	_, _ = m.nmcli(ctx, "connection", "delete", wifiConnID)
	m.mu.Lock()
	fromFile := m.boot.WifiSSID != ""
	m.mu.Unlock()
	msg := fmt.Sprintf("Could not join WiFi network %q", eff.SSID)
	if fromFile {
		msg += " (set in emm.txt)"
	}
	m.set(func() { m.lastErr = msg + ": " + err.Error() + ". Trying again later." })
	m.enterSetupMode(ctx)
}

// countStations counts the stations in `iw dev X station dump` output.
func countStations(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "station ") {
			n++
		}
	}
	return n
}

// monitorTick refreshes status. It notices a WiFi device that appears after
// boot (lateWifi) and, in setup mode, a plugged-in cable; it restores an access
// point that is down and retries the configured WiFi. It never starts setup
// mode on its own because a connection dropped: NetworkManager reconnects, and
// an access point would only take the radio away from it.
func (m *Manager) monitorTick(ctx context.Context) {
	if !m.opMu.TryLock() {
		return // a transition is running; it updates the status itself
	}
	defer m.opMu.Unlock()
	m.mu.Lock()
	mode := m.mode
	m.mu.Unlock()
	if mode == "unmanaged" {
		return
	}
	snap, err := m.probe(ctx)
	if err != nil {
		return
	}
	m.applySnapshot(snap)
	m.syncRadio(ctx)
	m.refreshHostname(ctx)

	m.mu.Lock()
	setup, joining, last := m.setup, m.joining, m.setupChk
	appeared := m.wifiAppeared
	m.wifiAppeared = false
	m.mu.Unlock()
	if appeared && !setup && !joining {
		m.lateWifi(ctx)
		return
	}
	if !setup {
		return
	}
	if m.now().Sub(last) >= m.t.SetupEthCheck {
		m.set(func() { m.setupChk = m.now() })
		if m.ethOnline(snap) {
			m.leaveSetupForEthernet(ctx, snap)
			return
		}
	}
	if joining {
		return // a Connect is about to switch networks; leave the AP to it
	}
	if !apServing(snap) {
		m.recoverAP(ctx)
		return
	}
	m.maybeRetryWifi(ctx)
}

// leaveSetupForEthernet ends setup mode because a cable is up. opMu is held.
func (m *Manager) leaveSetupForEthernet(ctx context.Context, snap snapshot) {
	m.log.Info("Ethernet came up in setup mode: leaving setup mode")
	m.leaveSetup(ctx)
	m.mu.Lock()
	eff := m.eff
	m.mu.Unlock()
	// Let NetworkManager try the configured WiFi again in the background.
	if eff.SSID != "" {
		if content, err := m.renderWifi(eff); err == nil {
			if m.writeProfile(wifiConnID, content) == nil {
				m.reload(ctx)
			}
		}
	}
	m.set(func() { m.setup, m.forcedSetup, m.lastErr = false, false, "" })
	m.applySnapshot(snap)
}

// ---- scanning --------------------------------------------------------------

// Scan returns the visible WiFi networks.
//
// In setup mode the cached list (taken just before the access point went up)
// is returned unless force is true; a forced scan takes the access point down
// for a few seconds, which drops the connected phone, so callers should only
// force it on an explicit user request.
func (m *Manager) Scan(ctx context.Context, force bool) ([]Network, error) {
	m.mu.Lock()
	setup := m.setup
	dev := m.wifiDev
	fresh := !m.scanAt.IsZero() && m.now().Sub(m.scanAt) < 10*time.Second
	m.mu.Unlock()

	if !force && (setup || fresh) {
		return m.cachedScan(), nil
	}
	if dev == "" {
		return nil, ErrNoWifi
	}
	if force {
		m.opMu.Lock()
	} else if !m.opMu.TryLock() {
		return m.cachedScan(), nil
	}
	defer m.opMu.Unlock()

	m.mu.Lock()
	setup = m.setup
	m.mu.Unlock()
	if setup {
		// Waiting for opMu may have used up the caller's time. Nothing has been
		// touched yet, so leave the AP alone rather than fail halfway.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Take the AP down for the scan and put it back afterwards. The way back
		// must not depend on the caller: its context ends with its HTTP request
		// (or its timeout), possibly in the middle of the scan, and a dead
		// context would leave the AP down. Whatever fails despite this, the
		// monitor loop restores it (recoverAP).
		m.apDown(ctx)
		nets, err := m.scanLocked(ctx, true)
		upCtx, cancel := context.WithTimeout(m.baseCtx(), apRestoreTimeout)
		defer cancel()
		if uerr := m.apUp(upCtx, dev); uerr != nil {
			m.mu.Lock()
			name := m.apSSID
			m.mu.Unlock()
			m.fail(fmt.Sprintf("could not restart the setup network: %s", friendlyNMError(uerr)), name)
		}
		return nets, err
	}
	return m.scanLocked(ctx, force)
}

func (m *Manager) cachedScan() []Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Network(nil), m.scan...)
}

// apRestoreTimeout bounds putting the AP back after a forced scan.
const apRestoreTimeout = 45 * time.Second

// scanLocked performs a scan. opMu is held.
func (m *Manager) scanLocked(ctx context.Context, rescan bool) ([]Network, error) {
	m.mu.Lock()
	dev := m.wifiDev
	m.mu.Unlock()
	if dev == "" {
		return nil, ErrNoWifi
	}
	mode := "auto"
	if rescan {
		mode = "yes"
	}
	out, err := m.nmcli(ctx, "-t", "-f", "IN-USE,SSID,SIGNAL,SECURITY", "device", "wifi", "list", "ifname", dev, "--rescan", mode)
	if err != nil {
		return nil, fmt.Errorf("scan failed: %s", friendlyNMError(err))
	}
	nets := parseWifiList(out)
	m.set(func() { m.scan, m.scanAt = nets, m.now() })
	return append([]Network(nil), nets...), nil
}

// ---- changing settings -----------------------------------------------------

// Connect joins a WiFi network and remembers it.
//
// In setup mode it returns as soon as the credentials are validated and saved;
// the switch happens ConnectDelay later in the background so the HTTP response
// can reach the phone (which then drops off the setup network, as expected).
// If the join fails the setup network comes back and Status().LastError says
// why. In client mode it applies immediately and reports failures.
// A failed attempt never overwrites previously saved credentials. A second
// Connect while a join from setup mode is pending is refused with ErrBusy.
func (m *Manager) Connect(ssid, password string) error {
	if err := validateSSID(ssid); err != nil {
		return err
	}
	if err := m.checkPSK(password); err != nil {
		return err
	}
	m.mu.Lock()
	managed := m.boot.WifiSSID != ""
	m.mu.Unlock()
	if managed {
		return ErrBootManaged
	}

	m.opMu.Lock()
	m.mu.Lock()
	if m.joining {
		// A join from setup mode is still pending (it starts ConnectDelay after
		// Connect returned). A second Connect now would take the first one's
		// unproven credentials as the state to roll back to.
		m.mu.Unlock()
		m.opMu.Unlock()
		return ErrBusy
	}
	prev := m.saved
	setup := m.setup
	// A network that is not in the scan is probably hidden (the UI's "Other
	// network..."): its profile must ask NetworkManager to probe for it.
	hidden := !scanHas(m.scan, ssid)
	m.mu.Unlock()
	next := prev
	next.WifiSSID, next.WifiPassword, next.WifiHidden = ssid, password, hidden
	if err := m.persist(next); err != nil {
		m.opMu.Unlock()
		return fmt.Errorf("saving settings: %w", err)
	}
	m.updateSaved(next)
	m.set(func() { m.lastErr, m.joining = "", true })

	if setup {
		m.bg.Add(1)
		go m.joinFromSetup(ssid, prev)
		m.opMu.Unlock()
		return nil
	}
	defer m.opMu.Unlock()

	ctx := m.baseCtx()
	err := m.applyWifi(ctx)
	if err != nil {
		m.revertWifi(ctx, prev)
	}
	m.set(func() { m.joining = false })
	m.refresh(ctx)
	if err != nil {
		return fmt.Errorf("could not join %q: %w", ssid, err)
	}
	return nil
}

// refresh probes and folds the result into the status.
func (m *Manager) refresh(ctx context.Context) {
	if snap, err := m.probe(ctx); err == nil {
		m.applySnapshot(snap)
	}
}

// restoreWifi puts the WiFi fields of prev back after a failed join. Only
// those: whatever else changed in the meantime (Ethernet, host name, country)
// stays as it is now.
func (m *Manager) restoreWifi(prev Saved) {
	m.mu.Lock()
	cur := m.saved
	m.mu.Unlock()
	cur.WifiSSID, cur.WifiPassword, cur.WifiHidden = prev.WifiSSID, prev.WifiPassword, prev.WifiHidden
	if err := m.persist(cur); err != nil {
		m.log.Warn("could not restore saved settings", "err", err)
	}
	m.updateSaved(cur)
}

// revertWifi restores the previous saved WiFi after a failed join.
func (m *Manager) revertWifi(ctx context.Context, prev Saved) {
	m.restoreWifi(prev)
	m.mu.Lock()
	eff := m.eff
	m.mu.Unlock()
	m.removeProfile(wifiConnID)
	_, _ = m.nmcli(ctx, "connection", "delete", wifiConnID)
	if eff.SSID != "" {
		if content, err := m.renderWifi(eff); err == nil && m.writeProfile(wifiConnID, content) == nil {
			m.reload(ctx)
			_, _ = m.nmcli(ctx, "--wait", "20", "connection", "up", wifiConnID)
		}
		return
	}
	// Nothing of EMM's own to go back to: the join took the radio from the
	// connection somebody else made, so give it back.
	m.restoreForeign(ctx)
}

// restoreForeign brings the foreign WiFi connection back that a join through
// the UI displaced (EMM has no WiFi of its own to go back to). It reports
// whether the box is on WiFi again. opMu is held.
func (m *Manager) restoreForeign(ctx context.Context) bool {
	m.mu.Lock()
	name, dev, own := m.foreignWifi, m.wifiDev, m.eff.SSID
	m.mu.Unlock()
	if name == "" || dev == "" || own != "" {
		return false
	}
	m.log.Info("going back to the WiFi connection that was there before", "connection", name)
	if _, err := m.nmcli(ctx, "--wait", "20", "connection", "up", "id", name, "ifname", dev); err != nil {
		m.log.Warn("could not bring the earlier WiFi connection back", "connection", name, "err", err)
		return false
	}
	snap, err := m.probe(ctx)
	if err != nil || !wifiConnected(snap) {
		return false
	}
	m.applySnapshot(snap)
	return true
}

func (m *Manager) joinFromSetup(ssid string, prev Saved) {
	defer m.bg.Done()
	ctx := m.baseCtx()
	if err := m.sleep(ctx, m.t.ConnectDelay); err != nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.set(func() { m.mode, m.setup, m.ip, m.ssid = "connecting", false, "", "" })
	m.leaveSetup(ctx)
	err := m.applyWifi(ctx)
	if err == nil {
		m.log.Info("joined WiFi from setup mode", "ssid", ssid)
		m.set(func() { m.joining, m.lastErr, m.forcedSetup = false, "", false })
		m.refresh(ctx)
		return
	}
	m.log.Warn("join failed, back to setup mode", "ssid", ssid, "err", err)
	m.restoreWifi(prev)
	m.removeProfile(wifiConnID)
	_, _ = m.nmcli(ctx, "connection", "delete", wifiConnID)
	m.set(func() {
		m.lastErr = fmt.Sprintf("Could not join %q: %v", ssid, err)
		m.joining = false
	})
	m.enterSetupMode(ctx)
}

// applyWifi (re)writes the WiFi profile from the effective settings and
// activates it, waiting for the link. opMu is held.
func (m *Manager) applyWifi(ctx context.Context) error {
	return m.applyWifiWait(ctx, m.t.WifiWait)
}

// applyWifiWait is applyWifi with its own limit on the wait for the link.
func (m *Manager) applyWifiWait(ctx context.Context, waitFor time.Duration) error {
	m.mu.Lock()
	eff := m.eff
	dev := m.wifiDev
	m.mu.Unlock()
	if dev == "" {
		return ErrNoWifi
	}
	content, err := m.renderWifi(eff)
	if err != nil {
		return err
	}
	if err := m.writeProfile(wifiConnID, content); err != nil {
		return err
	}
	m.reload(ctx)

	upCtx, cancel := context.WithTimeout(ctx, waitFor+10*time.Second)
	defer cancel()
	wait := strconv.Itoa(int(waitFor.Seconds()))
	if _, err := m.nmcli(upCtx, "--wait", wait, "connection", "up", wifiConnID, "ifname", dev); err != nil {
		return errors.New(friendlyNMError(err))
	}
	start := m.now()
	for {
		snap, perr := m.probe(ctx)
		if perr == nil && wifiConnected(snap) {
			m.applySnapshot(snap)
			return nil
		}
		if m.now().Sub(start) >= waitFor {
			return errors.New("timed out waiting for the connection")
		}
		if err := m.sleep(ctx, m.t.Poll); err != nil {
			return err
		}
	}
}

// Forget removes the WiFi network saved through the UI. With nothing saved it
// does nothing: a connection that was there before EMM (Status.Adopted) is not
// EMM's to forget. A box that was on such a connection before the saved network
// went onto it goes back to it.
func (m *Manager) Forget() error {
	m.mu.Lock()
	managed := m.boot.WifiSSID != ""
	m.mu.Unlock()
	if managed {
		return ErrBootManaged
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx := m.baseCtx()

	m.mu.Lock()
	next := m.saved
	m.mu.Unlock()
	if next.WifiSSID == "" {
		// Nothing of EMM's to forget. A WiFi connection that is up anyway was
		// made by somebody else (see Status.Adopted) and is none of our business.
		return nil
	}
	next.WifiSSID, next.WifiPassword, next.WifiHidden = "", "", false
	if err := m.persist(next); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	m.updateSaved(next)
	m.removeProfile(wifiConnID)
	_, _ = m.nmcli(ctx, "connection", "delete", wifiConnID)
	m.reload(ctx)

	m.mu.Lock()
	setup := m.setup
	m.mu.Unlock()
	if setup {
		return nil
	}
	snap, err := m.probe(ctx)
	if err == nil && m.ethOnline(snap) {
		m.applySnapshot(snap)
		return nil
	}
	if m.restoreForeign(ctx) {
		return nil
	}
	m.set(func() { m.lastErr = "" })
	m.enterSetupMode(ctx)
	return nil
}

// SetEthernet changes the wired configuration.
func (m *Manager) SetEthernet(e EthernetSettings) error {
	e, err := normalizeEthernet(e)
	if err != nil {
		return err
	}
	m.mu.Lock()
	managed := m.boot.Ethernet != ""
	m.mu.Unlock()
	if managed {
		return ErrBootManaged
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx := m.baseCtx()

	m.mu.Lock()
	next := m.saved
	m.mu.Unlock()
	next.Ethernet = e
	if err := m.persist(next); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	m.updateSaved(next)
	content, err := renderEthernet(e)
	if err != nil {
		return err
	}
	if err := m.writeProfile(ethConnID, content); err != nil {
		return err
	}
	m.reload(ctx)
	if e.Mode == "off" {
		m.disconnectEthernet(ctx)
	} else {
		upCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if _, err := m.nmcli(upCtx, "--wait", "15", "connection", "up", ethConnID); err != nil {
			// No cable is normal; the profile is in place for when there is one.
			m.log.Debug("ethernet activate", "err", err)
		}
	}
	if snap, err := m.probe(ctx); err == nil {
		m.applySnapshot(snap)
	}
	return nil
}

// SetHostname changes the host name (and so the .local name).
func (m *Manager) SetHostname(h string) error {
	h = strings.ToLower(strings.TrimSpace(h))
	if !ValidHostname(h) {
		return invalid("%q is not a valid host name: use a-z, 0-9 and '-', at most 63 characters, not starting or ending with '-'", h)
	}
	m.mu.Lock()
	managed := m.boot.Hostname != ""
	m.mu.Unlock()
	if managed {
		return ErrBootManaged
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx := m.baseCtx()

	m.mu.Lock()
	next := m.saved
	m.mu.Unlock()
	next.Hostname = h
	if err := m.persist(next); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	m.updateSaved(next)
	m.applyHostname(ctx, h)
	return nil
}

// SetWifiCountry changes the WiFi regulatory domain (two-letter code).
func (m *Manager) SetWifiCountry(cc string) error {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if !validCountry(cc) {
		return invalid("%q is not a two-letter country code", cc)
	}
	m.mu.Lock()
	managed := m.boot.WifiCountry != ""
	m.mu.Unlock()
	if managed {
		return ErrBootManaged
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx := m.baseCtx()

	m.mu.Lock()
	next := m.saved
	m.mu.Unlock()
	next.WifiCountry = cc
	if err := m.persist(next); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	m.updateSaved(next)
	if _, err := m.run(ctx, "iw", "reg", "set", cc); err != nil {
		m.log.Warn("could not set regulatory domain", "err", err)
	}
	return nil
}
