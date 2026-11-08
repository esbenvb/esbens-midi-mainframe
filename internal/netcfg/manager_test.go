package netcfg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- harness ---------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t    *testing.T
	w    *fakeWorld
	m    *Manager
	clk  *fakeClock
	nm   string
	dns  string
	boot string
	// hosts is the hosts file the Manager is pointed at: a path in a temp
	// directory (no file until a test writes one), never the real /etc/hosts.
	hosts string

	mu         sync.Mutex
	saved      Saved
	saves      []Saved
	history    []Status
	sleeps     int
	blockAfter int           // when >0, Sleep blocks (until ctx ends) after this many calls
	gate       chan struct{} // when set, the ConnectDelay sleep (2 s) waits for it
	saveErr    error
	events     []event // things that happen when the fake clock reaches them (see at)
}

// event is something the world does at a given time: a driver finishing its
// load, NetworkManager joining a profile.
type event struct {
	at time.Duration // since the clock started
	fn func()
}

type envOpt func(*env)

func withSaved(s Saved) envOpt            { return func(e *env) { e.saved = s } }
func withCable() envOpt                   { return func(e *env) { e.w.ethCable = true } }
func withWorld(f func(*fakeWorld)) envOpt { return func(e *env) { f(e.w) } }
func withBootFile(content string) envOpt {
	return func(e *env) {
		if err := os.WriteFile(filepath.Join(e.boot, BootFileName), []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}
func withBootFlag(name string) envOpt {
	return func(e *env) {
		if err := os.WriteFile(filepath.Join(e.boot, name), nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	e := &env{t: t, clk: newClock(), nm: t.TempDir(), dns: t.TempDir(), boot: t.TempDir()}
	e.hosts = filepath.Join(t.TempDir(), "hosts")
	e.w = newFakeWorld(e.nm)
	for _, o := range opts {
		o(e)
	}
	e.m = NewManager(Options{
		BootDir: e.boot, NMDir: e.nm, DNSMasqDir: e.dns, HostsFile: e.hosts,
		Runner: e.w,
		Load:   func() Saved { e.mu.Lock(); defer e.mu.Unlock(); return e.saved },
		Save: func(s Saved) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.saveErr != nil {
				return e.saveErr
			}
			e.saved = s
			e.saves = append(e.saves, s)
			return nil
		},
		OnStatusChange: func() {
			st := e.m.Status()
			e.mu.Lock()
			e.history = append(e.history, st)
			e.mu.Unlock()
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    e.clk.Now,
		Sleep:  e.sleep,
	})
	return e
}

func (e *env) sleep(ctx context.Context, d time.Duration) error {
	e.mu.Lock()
	e.sleeps++
	n, gate, block := e.sleeps, e.gate, e.blockAfter
	e.mu.Unlock()
	if gate != nil && d == 2*time.Second {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if block > 0 && n > block {
		<-ctx.Done()
		return ctx.Err()
	}
	e.clk.advance(d)
	e.fire()
	return ctx.Err()
}

// at schedules fn for when the fake clock has run for d (boot sequence waits
// and advance both move it).
func (e *env) at(d time.Duration, fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event{d, fn})
}

// fire runs the events whose time has come.
func (e *env) fire() {
	e.mu.Lock()
	var due []func()
	keep := e.events[:0]
	for _, ev := range e.events {
		if e.elapsed() >= ev.at {
			due = append(due, ev.fn)
		} else {
			keep = append(keep, ev)
		}
	}
	e.events = keep
	e.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

// advance moves the fake clock outside the boot sequence (a monitor tick later).
func (e *env) advance(d time.Duration) {
	e.clk.advance(d)
	e.fire()
}

// start runs the boot sequence synchronously.
func (e *env) start() {
	e.t.Helper()
	e.m.opMu.Lock()
	e.m.startup(context.Background())
	e.m.opMu.Unlock()
}

// elapsed is how much fake time has passed since the clock started.
func (e *env) elapsed() time.Duration { return e.clk.Now().Sub(newClock().Now()) }

func (e *env) savedNow() Saved { e.mu.Lock(); defer e.mu.Unlock(); return e.saved }

func (e *env) cmdIndex(sub string) int {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	for i, c := range e.w.cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func (e *env) cmdCount(sub string) int {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	n := 0
	for _, c := range e.w.cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (e *env) profile(id string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(e.nm, id+".nmconnection"))
	return string(b), err == nil
}

func (e *env) dnsSnippet() (string, bool) {
	b, err := os.ReadFile(filepath.Join(e.dns, dnsmasqSnippet))
	return string(b), err == nil
}

func (e *env) apUp() bool {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	return e.w.apUp
}

func (e *env) sawStatus(pred func(Status) bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.history {
		if pred(s) {
			return true
		}
	}
	return false
}

var wifiHome = Saved{WifiSSID: "HomeNet-5G", WifiPassword: "secret123"}

// ---- boot sequence ---------------------------------------------------------

func TestBootEthernetOnlineSkipsAP(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	st := e.m.Status()
	if st.Mode != "ethernet" || st.SetupMode || st.IP != "192.168.1.50" || !st.Ethernet.Link || st.Ethernet.IP != "192.168.1.50" {
		t.Fatalf("status %+v", st)
	}
	if e.w.apUps != 0 || e.cmdCount("wifi list") != 0 {
		t.Fatalf("AP or scan was used although Ethernet is up (apUps=%d)", e.w.apUps)
	}
	if e.m.InSetupMode() {
		t.Fatal("InSetupMode")
	}
	if _, ok := e.profile(ethConnID); !ok {
		t.Fatal("ethernet profile not written")
	}
	// Nothing in emm.txt or the saved settings names the machine: it keeps its name.
	if e.w.hostname != "raspberrypi" || e.cmdIndex("hostnamectl") >= 0 || e.cmdIndex("systemctl restart avahi-daemon") >= 0 {
		t.Fatalf("host name %q, commands %v", e.w.hostname, e.w.cmds)
	}
	if e.m.PortalURL() != "http://raspberrypi.local/" {
		t.Fatalf("portal %q", e.m.PortalURL())
	}
}

func TestBootWifiConfiguredJoins(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.IP == "" || st.SetupMode {
		t.Fatalf("status %+v", st)
	}
	prof, ok := e.profile(wifiConnID)
	if !ok || !strings.Contains(prof, "ssid=HomeNet-5G\n") || !strings.Contains(prof, "psk=secret123\n") {
		t.Fatalf("wifi profile:\n%s", prof)
	}
	if e.w.apUps != 0 {
		t.Fatal("AP raised although WiFi worked")
	}
	if e.cmdIndex("iw reg set") >= 0 {
		t.Fatal("no regulatory domain configured, none should be set")
	}
}

func TestBootRegulatoryDomainAndRfkill(t *testing.T) {
	e := newEnv(t, withSaved(Saved{WifiCountry: "SE"}), withBootFile("wifi_country = DK\n"), withCable())
	e.start()
	if e.cmdIndex("rfkill unblock wifi") < 0 {
		t.Fatal("rfkill not unblocked")
	}
	if e.cmdIndex("iw reg set DK") < 0 || e.cmdIndex("iw reg set SE") >= 0 {
		t.Fatalf("emm.txt must win: %v", e.w.cmds)
	}
	if e.cmdIndex("rfkill") > e.cmdIndex("connection reload") {
		t.Fatal("rfkill must be handled before profiles are loaded")
	}
}

func TestBootWifiFailureFallsBackToSetupMode(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withWorld(func(w *fakeWorld) { w.wifiNeverConnects = true }))
	e.start()

	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.APSSID != "EMM-Setup-A1C2" || st.IP != APAddress {
		t.Fatalf("status %+v", st)
	}
	if !strings.Contains(st.LastError, `"HomeNet-5G"`) {
		t.Fatalf("LastError %q", st.LastError)
	}
	if e.elapsed() < 45*time.Second {
		t.Fatalf("gave up after only %v; should wait 45 s for a configured network", e.elapsed())
	}
	if !e.apUp() {
		t.Fatal("AP not up")
	}
	if e.w.scansDuringAP != 0 {
		t.Fatal("scanned while the AP was up")
	}
	scan, up := e.cmdIndex("wifi list"), e.cmdIndex("connection up emm-setup-ap")
	if scan < 0 || up < 0 || scan > up {
		t.Fatalf("must scan BEFORE bringing the AP up (scan=%d up=%d): %v", scan, up, e.w.cmds)
	}
	if snip, ok := e.dnsSnippet(); !ok || !strings.Contains(snip, "address=/#/10.42.0.1") {
		t.Fatalf("captive DNS snippet missing or wrong: %q", snip)
	}
	ap, ok := e.profile(apConnID)
	if !ok || !strings.Contains(ap, "ssid=EMM-Setup-A1C2\n") || strings.Contains(ap, "psk=") {
		t.Fatalf("AP profile:\n%s", ap)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("client profile must not exist while in setup mode")
	}
	nets, err := e.m.Scan(context.Background(), false)
	if err != nil || len(nets) != 8 {
		t.Fatalf("cached scan: %d networks, err %v", len(nets), err)
	}
	if e.m.PortalURL() != "http://10.42.0.1/" {
		t.Fatalf("portal %q", e.m.PortalURL())
	}
	// The saved credentials are untouched: a failed boot is not a reason to forget them.
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" {
		t.Fatalf("saved changed: %+v", got)
	}
}

func TestBootNothingConfiguredNoCableGoesStraightToSetup(t *testing.T) {
	e := newEnv(t)
	e.start()
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	if e.elapsed() > 10*time.Second {
		t.Fatalf("took %v: without a cable there is no reason to wait 15 s", e.elapsed())
	}
}

func TestBootEthernetWithoutDHCPWaitsThenFallsBack(t *testing.T) {
	// A cable that is plugged in but never gets an address.
	e := newEnv(t, withCable(), withWorld(func(w *fakeWorld) { w.ethIP = "" }))
	e.start()
	if st := e.m.Status(); st.Mode != "ap" || !st.SetupMode {
		t.Fatalf("status %+v", st)
	}
	if e.elapsed() < 15*time.Second {
		t.Fatalf("gave up after %v; a plugged-in cable gets 15 s", e.elapsed())
	}
}

func TestBootResetFileForcesSetupMode(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withBootFlag(ResetFileName))
	e.start()
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode {
		t.Fatalf("status %+v", st)
	}
	if e.cmdIndex("connection up emm-wifi") >= 0 {
		t.Fatal("must not try the saved WiFi after reset-network")
	}
	if _, err := os.Stat(filepath.Join(e.boot, ResetFileName)); !os.IsNotExist(err) {
		t.Fatal("reset-network must be deleted after use")
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || len(e.saves) != 0 {
		t.Fatalf("reset-network must not erase saved settings: %+v (saves %d)", got, len(e.saves))
	}
}

func TestBootResetFileOnReadOnlyBootDirStillWorks(t *testing.T) {
	// Removal failing is only logged. Simulate with a directory instead of a file.
	e := newEnv(t, withSaved(wifiHome))
	if err := os.MkdirAll(filepath.Join(e.boot, ResetFileName, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.start()
	if !e.m.Status().SetupMode {
		t.Fatal("expected setup mode")
	}
}

func TestBootUsesApSettingsFromEmmTxt(t *testing.T) {
	e := newEnv(t, withBootFile("ap_ssid = MySetup\nap_password = mainframe1\n"))
	e.start()
	st := e.m.Status()
	if st.APSSID != "MySetup" {
		t.Fatalf("AP SSID %q", st.APSSID)
	}
	ap, _ := e.profile(apConnID)
	if !strings.Contains(ap, "ssid=MySetup\n") || !strings.Contains(ap, "psk=mainframe1\n") {
		t.Fatalf("AP profile:\n%s", ap)
	}
}

func TestBootBadWifiSettingsFromFileReportedNotFatal(t *testing.T) {
	e := newEnv(t, withBootFile("wifi_ssid = Net\nwifi_password = abc\n"))
	e.start()
	st := e.m.Status()
	if !st.SetupMode || !strings.Contains(st.LastError, "unusable") {
		t.Fatalf("status %+v", st)
	}
	if len(st.Warnings) == 0 {
		t.Fatal("emm.txt warnings must be surfaced in Status")
	}
}

func TestBootInvalidStaticEthernetFallsBackToDHCP(t *testing.T) {
	e := newEnv(t, withBootFile("ethernet = static\n"), withCable())
	e.start()
	st := e.m.Status()
	if st.Mode != "ethernet" {
		t.Fatalf("should still come up on DHCP: %+v", st)
	}
	var warned bool
	for _, w := range st.Warnings {
		warned = warned || strings.Contains(w, "Ethernet settings unusable")
	}
	if !warned || st.LastError != "" {
		t.Fatalf("warnings %v, LastError %q", st.Warnings, st.LastError)
	}
	prof, _ := e.profile(ethConnID)
	if !strings.Contains(prof, "[ipv4]\nmethod=auto") {
		t.Fatalf("fallback profile:\n%s", prof)
	}
}

func TestBootEthernetOff(t *testing.T) {
	e := newEnv(t, withSaved(Saved{Ethernet: EthernetSettings{Mode: "off"}}), withCable())
	e.start()
	st := e.m.Status()
	if st.Ethernet.Mode != "off" || st.Ethernet.IP != "" || st.Mode == "ethernet" {
		t.Fatalf("status %+v", st)
	}
	prof, _ := e.profile(ethConnID)
	if !strings.Contains(prof, "autoconnect=false") || e.cmdIndex("device disconnect eth0") < 0 {
		t.Fatalf("ethernet not switched off: %s / %v", prof, e.w.cmds)
	}
}

func TestBootNoWifiAdapter(t *testing.T) {
	e := newEnv(t, withWorld(func(w *fakeWorld) { w.wifiDev = "" }))
	e.start()
	st := e.m.Status()
	if st.Mode != "offline" || !strings.Contains(st.LastError, "no WiFi adapter") {
		t.Fatalf("status %+v", st)
	}
	if _, err := e.m.Scan(context.Background(), true); !errors.Is(err, ErrNoWifi) {
		t.Fatalf("scan: %v", err)
	}
}

func TestBootNetworkManagerMissingIsUnmanaged(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withWorld(func(w *fakeWorld) { w.nmDown = true }))
	e.start()
	if st := e.m.Status(); st.Mode != "unmanaged" {
		t.Fatalf("status %+v", st)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("must not write profiles when NetworkManager is unavailable")
	}
}

func TestBootStatusChangeCallback(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	if !e.sawStatus(func(s Status) bool { return s.Mode == "connecting" }) ||
		!e.sawStatus(func(s Status) bool { return s.Mode == "client" && s.SSID == "HomeNet-5G" }) {
		t.Fatalf("callback history: %+v", e.history)
	}
}

// ---- setup mode: Connect ---------------------------------------------------

func TestConnectInSetupModeSucceeds(t *testing.T) {
	e := newEnv(t)
	e.start()
	if !e.m.InSetupMode() {
		t.Fatal("precondition: setup mode")
	}
	e.gate = make(chan struct{}) // hold the switch back so we can observe the interim state

	if err := e.m.Connect("HomeNet-5G", "secret123"); err != nil {
		t.Fatal(err)
	}
	// Connect has returned; the AP must still be up for the HTTP response to get out.
	st := e.m.Status()
	if !st.Joining || st.Mode != "ap" || !e.apUp() {
		t.Fatalf("interim status %+v apUp=%v", st, e.apUp())
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || got.WifiPassword != "secret123" {
		t.Fatalf("credentials not persisted before returning: %+v", got)
	}

	close(e.gate)
	e.m.bg.Wait()

	st = e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || st.Joining || st.LastError != "" || st.IP == "" {
		t.Fatalf("final status %+v", st)
	}
	if e.apUp() {
		t.Fatal("AP still up")
	}
	if _, ok := e.dnsSnippet(); ok {
		t.Fatal("captive DNS snippet must be removed when leaving setup mode")
	}
	if _, ok := e.profile(apConnID); ok {
		t.Fatal("AP profile must be removed")
	}
	if prof, ok := e.profile(wifiConnID); !ok || !strings.Contains(prof, "ssid=HomeNet-5G") {
		t.Fatalf("wifi profile: %q", prof)
	}
	if e.m.PortalURL() != "http://raspberrypi.local/" {
		t.Fatalf("portal %q", e.m.PortalURL())
	}
	if !e.sawStatus(func(s Status) bool { return s.Joining && s.SetupMode }) {
		t.Fatal("UI never saw the joining state")
	}
}

func TestConnectInSetupModeFailureRestoresSetupMode(t *testing.T) {
	e := newEnv(t)
	e.start()
	apUpsBefore := e.w.apUps

	if err := e.m.Connect("HomeNet-5G", "wrongpassword"); err != nil {
		t.Fatalf("Connect must return nil in setup mode, the failure is reported via Status: %v", err)
	}
	e.m.bg.Wait()

	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.Joining || !e.apUp() {
		t.Fatalf("setup mode not restored: %+v", st)
	}
	if !strings.Contains(st.LastError, `Could not join "HomeNet-5G"`) || !strings.Contains(st.LastError, "wrong password") {
		t.Fatalf("LastError %q", st.LastError)
	}
	if e.w.apUps != apUpsBefore+1 {
		t.Fatalf("AP should have been brought back exactly once (before=%d after=%d)", apUpsBefore, e.w.apUps)
	}
	if got := e.savedNow(); got.WifiSSID != "" {
		t.Fatalf("failed credentials must not stay saved: %+v", got)
	}
	if len(e.saves) != 2 || e.saves[0].WifiSSID != "HomeNet-5G" || e.saves[1].WifiSSID != "" {
		t.Fatalf("save sequence: %+v", e.saves)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("failed wifi profile must be removed")
	}
	if e.w.scansDuringAP != 0 {
		t.Fatal("scanned while the AP was up")
	}
	if nets, _ := e.m.Scan(context.Background(), false); len(nets) != 8 {
		t.Fatalf("scan cache lost: %d", len(nets))
	}
}

func TestConnectFailureKeepsPreviousSavedNetwork(t *testing.T) {
	// Boot failed for the saved network, user tries another one that also fails:
	// the originally saved credentials must survive both.
	e := newEnv(t, withSaved(wifiHome), withWorld(func(w *fakeWorld) { w.failSSIDs["HomeNet-5G"] = true }))
	e.start()
	if !e.m.InSetupMode() {
		t.Fatal("precondition")
	}
	if err := e.m.Connect("linksys", "wrongpassword"); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || got.WifiPassword != "secret123" {
		t.Fatalf("previous credentials lost: %+v", got)
	}
}

func TestConnectValidation(t *testing.T) {
	e := newEnv(t)
	e.start()
	for _, c := range []struct{ ssid, pw string }{
		{"", "password1"},
		{strings.Repeat("x", 33), ""},
		{"Net", "short"},
	} {
		if err := e.m.Connect(c.ssid, c.pw); !errors.Is(err, ErrInvalid) {
			t.Errorf("Connect(%q,%q) = %v, want ErrInvalid", c.ssid, c.pw, err)
		}
	}
	if len(e.saves) != 0 {
		t.Fatal("nothing may be persisted for invalid input")
	}
}

func TestConnectSaveFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.saveErr = errors.New("disk full")
	err := e.m.Connect("HomeNet-5G", "secret123")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
	e.m.bg.Wait()
	if !e.m.InSetupMode() || !e.apUp() {
		t.Fatal("a failed save must leave setup mode untouched")
	}
}

func TestConnectOpenNetwork(t *testing.T) {
	e := newEnv(t)
	e.start()
	if err := e.m.Connect("CoffeeShop Open", ""); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	if st := e.m.Status(); st.Mode != "client" || st.SSID != "CoffeeShop Open" {
		t.Fatalf("status %+v", st)
	}
}

func TestConnectSSIDWithColonAndSpaces(t *testing.T) {
	e := newEnv(t)
	e.start()
	if err := e.m.Connect("Studio:5G", "pass word 1"); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	if st := e.m.Status(); st.Mode != "client" || st.SSID != "Studio:5G" {
		t.Fatalf("status %+v / %s", st, st.LastError)
	}
}

// ---- client mode: Connect / Forget -----------------------------------------

func TestConnectInClientModeSwitchesNetworks(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	if err := e.m.Connect("Fiber-3F21", "newpassword"); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "Fiber-3F21" || st.Joining {
		t.Fatalf("status %+v", st)
	}
	if got := e.savedNow(); got.WifiSSID != "Fiber-3F21" || got.WifiPassword != "newpassword" {
		t.Fatalf("saved %+v", got)
	}
}

func TestConnectInClientModeFailureRevertsToPreviousNetwork(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	err := e.m.Connect("linksys", "wrongpassword")
	if err == nil || !strings.Contains(err.Error(), "wrong password") || !strings.Contains(err.Error(), "linksys") {
		t.Fatalf("err %v", err)
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.Joining {
		t.Fatalf("should be back on the old network: %+v", st)
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || got.WifiPassword != "secret123" {
		t.Fatalf("saved %+v", got)
	}
	prof, _ := e.profile(wifiConnID)
	if !strings.Contains(prof, "ssid=HomeNet-5G\n") {
		t.Fatalf("old profile not restored:\n%s", prof)
	}
}

func TestForgetWithoutEthernetEntersSetupMode(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	if got := e.savedNow(); got.WifiSSID != "" || got.WifiPassword != "" {
		t.Fatalf("saved %+v", got)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("profile must be gone")
	}
}

func TestForgetWithEthernetStaysOnline(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withCable())
	e.start()
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status()
	if st.Mode != "ethernet" || st.SetupMode || e.apUp() {
		t.Fatalf("status %+v", st)
	}
}

// ---- setup mode: Ethernet appears, scanning --------------------------------

func TestSetupModeLeavesWhenEthernetComesUp(t *testing.T) {
	e := newEnv(t)
	e.start()
	if !e.m.InSetupMode() {
		t.Fatal("precondition")
	}
	e.w.ethCable = true // someone plugs a cable in

	// Too early: the check runs every SetupEthCheck (30 s).
	e.clk.advance(10 * time.Second)
	e.m.monitorTick(context.Background())
	if !e.m.InSetupMode() || !e.apUp() {
		t.Fatal("left setup mode too early")
	}

	e.clk.advance(30 * time.Second)
	e.m.monitorTick(context.Background())
	st := e.m.Status()
	if st.Mode != "ethernet" || st.SetupMode || e.apUp() {
		t.Fatalf("status %+v apUp=%v", st, e.apUp())
	}
	if _, ok := e.dnsSnippet(); ok {
		t.Fatal("captive DNS snippet must be removed")
	}
	if _, ok := e.profile(apConnID); ok {
		t.Fatal("AP profile must be removed")
	}
}

func TestSetupModeLeaveRestoresConfiguredWifiProfile(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withWorld(func(w *fakeWorld) { w.failSSIDs["HomeNet-5G"] = true; w.wifiNeverConnects = true }))
	e.start()
	if !e.m.InSetupMode() {
		t.Fatal("precondition")
	}
	e.w.mu.Lock()
	e.w.ethCable = true
	e.w.wifiNeverConnects = false
	e.w.mu.Unlock()
	e.clk.advance(31 * time.Second)
	e.m.monitorTick(context.Background())
	if e.m.InSetupMode() {
		t.Fatal("should have left setup mode")
	}
	if _, ok := e.profile(wifiConnID); !ok {
		t.Fatal("the configured WiFi profile should be put back so NetworkManager can retry it")
	}
}

func TestScanInSetupModeUsesCacheUnlessForced(t *testing.T) {
	e := newEnv(t)
	e.start()
	scans := e.cmdCount("wifi list")
	ups := e.w.apUps

	nets, err := e.m.Scan(context.Background(), false)
	if err != nil || len(nets) != 8 {
		t.Fatalf("cached: %d %v", len(nets), err)
	}
	if e.cmdCount("wifi list") != scans || e.w.apUps != ups {
		t.Fatal("a plain Scan in setup mode must not touch the radio")
	}
	// Strongest first, duplicates merged, hidden network dropped, colon intact.
	if nets[0].SSID != "HomeNet-5G" || nets[0].Signal != 82 {
		t.Fatalf("first %+v", nets[0])
	}
	var sawColon, sawOpen bool
	for _, n := range nets {
		sawColon = sawColon || n.SSID == "Studio:5G"
		sawOpen = sawOpen || (n.SSID == "CoffeeShop Open" && n.Open())
		if n.SSID == "" {
			t.Fatal("hidden network leaked")
		}
	}
	if !sawColon || !sawOpen {
		t.Fatalf("colon=%v open=%v in %+v", sawColon, sawOpen, nets)
	}

	e.clk.advance(90 * time.Second)
	if age := e.m.Status().ScanAge; age < 90 {
		t.Fatalf("ScanAge %d", age)
	}

	nets, err = e.m.Scan(context.Background(), true)
	if err != nil || len(nets) != 8 {
		t.Fatalf("forced: %d %v", len(nets), err)
	}
	if e.cmdCount("wifi list") != scans+1 {
		t.Fatalf("forced scan did not scan")
	}
	if e.w.apUps != ups+1 || !e.apUp() {
		t.Fatalf("AP must be back up after a forced scan (ups %d→%d, up=%v)", ups, e.w.apUps, e.apUp())
	}
	if e.w.scansDuringAP != 0 {
		t.Fatal("scanned while the AP was up")
	}
	if age := e.m.Status().ScanAge; age != 0 {
		t.Fatalf("ScanAge %d after rescan", age)
	}
	if !e.m.InSetupMode() {
		t.Fatal("still in setup mode after the rescan")
	}
}

func TestScanInClientModeCachesBriefly(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	ctx := context.Background()

	nets, err := e.m.Scan(ctx, true)
	if err != nil || len(nets) != 8 {
		t.Fatalf("%d %v", len(nets), err)
	}
	if !nets[0].InUse || nets[0].SSID != "HomeNet-5G" {
		t.Fatalf("in-use flag missing: %+v", nets[0])
	}
	if e.cmdIndex("--rescan yes") < 0 {
		t.Fatal("forced scan must ask for a rescan")
	}
	n := e.cmdCount("wifi list")
	if _, err := e.m.Scan(ctx, false); err != nil || e.cmdCount("wifi list") != n {
		t.Fatal("a fresh cache must be served without scanning")
	}
	e.clk.advance(11 * time.Second)
	if _, err := e.m.Scan(ctx, false); err != nil || e.cmdCount("wifi list") != n+1 {
		t.Fatal("a stale cache must trigger a scan")
	}
	if e.cmdIndex("--rescan auto") < 0 {
		t.Fatal("an unforced scan should let NetworkManager decide (auto)")
	}
}

// ---- other settings --------------------------------------------------------

func TestSetHostname(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	if err := e.m.SetHostname("Kitchen-Synth"); err != nil {
		t.Fatal(err)
	}
	if e.w.hostname != "kitchen-synth" {
		t.Fatalf("host name %q", e.w.hostname)
	}
	if got := e.savedNow(); got.Hostname != "kitchen-synth" {
		t.Fatalf("saved %+v", got)
	}
	if e.m.Status().Hostname != "kitchen-synth" || e.m.PortalURL() != "http://kitchen-synth.local/" {
		t.Fatalf("status %+v portal %s", e.m.Status(), e.m.PortalURL())
	}
	saves := len(e.saves)
	for _, bad := range []string{"", "-x", "has space", "under_score", strings.Repeat("a", 64)} {
		if err := e.m.SetHostname(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetHostname(%q) = %v", bad, err)
		}
	}
	if len(e.saves) != saves {
		t.Fatal("invalid host names must not be persisted")
	}
}

func TestSetHostnameFallsBackToHostnameCommand(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	// A system without hostnamectl (no systemd): the plain hostname command does it.
	e.m.o.Runner = failingRunner{Runner: e.w, fail: func(name string, args []string) bool { return name == "hostnamectl" }}
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	if e.w.hostname != "kitchen" || e.cmdIndex("hostname kitchen") < 0 {
		t.Fatalf("host name %q, commands %v", e.w.hostname, e.w.cmds)
	}
}

func TestSetEthernetStatic(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	err := e.m.SetEthernet(EthernetSettings{Mode: "static", Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	prof, _ := e.profile(ethConnID)
	if !strings.Contains(prof, "address1=192.168.1.50/24,192.168.1.1\n") || !strings.Contains(prof, "method=manual") {
		t.Fatalf("profile:\n%s", prof)
	}
	if got := e.savedNow().Ethernet; got.Mode != "static" || got.Address != "192.168.1.50/24" {
		t.Fatalf("saved %+v", got)
	}
	if e.m.Status().Ethernet.Mode != "static" {
		t.Fatalf("status %+v", e.m.Status())
	}
	if err := e.m.SetEthernet(EthernetSettings{Mode: "static", Address: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid static address: %v", err)
	}
}

func TestSetEthernetOffAndBack(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	if err := e.m.SetEthernet(EthernetSettings{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	if e.cmdIndex("device disconnect eth0") < 0 {
		t.Fatal("device not disconnected")
	}
	if st := e.m.Status(); st.Ethernet.IP != "" || st.Ethernet.Mode != "off" {
		t.Fatalf("status %+v", st)
	}
	if err := e.m.SetEthernet(EthernetSettings{Mode: "auto"}); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(); st.Ethernet.IP != "192.168.1.50" || st.Mode != "ethernet" {
		t.Fatalf("status %+v", st)
	}
}

func TestSetWifiCountry(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	if err := e.m.SetWifiCountry("se"); err != nil {
		t.Fatal(err)
	}
	if e.cmdIndex("iw reg set SE") < 0 || e.savedNow().WifiCountry != "SE" {
		t.Fatal("country not applied/saved")
	}
	if err := e.m.SetWifiCountry("Sweden"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestBootManagedSettingsAreProtected(t *testing.T) {
	e := newEnv(t, withBootFile(`hostname = studio
wifi_country = DK
wifi_ssid = HomeNet-5G
wifi_password = secret123
ethernet = auto
rtp = off
ble_name = Studio BLE
`))
	e.start()
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.Hostname != "studio" {
		t.Fatalf("status %+v", st)
	}
	want := []string{"hostname", "wifi", "ethernet", "wifi_country"}
	if strings.Join(st.BootManaged, ",") != strings.Join(want, ",") {
		t.Fatalf("BootManaged %v", st.BootManaged)
	}
	if e.w.hostname != "studio" {
		t.Fatalf("host name %q", e.w.hostname)
	}
	if err := e.m.Connect("Fiber-3F21", "newpassword"); !errors.Is(err, ErrBootManaged) {
		t.Errorf("Connect: %v", err)
	}
	if err := e.m.Forget(); !errors.Is(err, ErrBootManaged) {
		t.Errorf("Forget: %v", err)
	}
	if err := e.m.SetHostname("other"); !errors.Is(err, ErrBootManaged) {
		t.Errorf("SetHostname: %v", err)
	}
	if err := e.m.SetEthernet(EthernetSettings{Mode: "off"}); !errors.Is(err, ErrBootManaged) {
		t.Errorf("SetEthernet: %v", err)
	}
	if err := e.m.SetWifiCountry("SE"); !errors.Is(err, ErrBootManaged) {
		t.Errorf("SetWifiCountry: %v", err)
	}
	if len(e.saves) != 0 {
		t.Fatal("nothing may be saved for boot-managed settings")
	}
	b := e.m.Boot()
	if b.RTP == nil || *b.RTP || b.BLEName != "Studio BLE" || b.WifiPassword != "secret123" {
		t.Fatalf("Boot() %+v", b)
	}
	// Boot() hands out copies.
	*b.RTP = true
	if b2 := e.m.Boot(); *b2.RTP {
		t.Fatal("Boot() leaked internal pointer")
	}
}

func TestBootFileWinsOverSavedSettingsButOthersMerge(t *testing.T) {
	saved := Saved{Hostname: "from-ui", WifiSSID: "linksys", WifiPassword: "uipassword",
		Ethernet: EthernetSettings{Mode: "dhcp"}}
	e := newEnv(t, withSaved(saved), withBootFile("hostname = from-file\n"), withCable())
	e.start()
	st := e.m.Status()
	if st.Hostname != "from-file" {
		t.Fatalf("hostname %q", st.Hostname)
	}
	// Ethernet and WiFi are not in emm.txt, so the UI's values stand.
	prof, _ := e.profile(ethConnID)
	if !strings.Contains(prof, "may-fail=false") {
		t.Fatalf("saved ethernet mode ignored:\n%s", prof)
	}
}

func TestMergeConfig(t *testing.T) {
	yes := true
	_ = yes
	e := mergeConfig(BootConfig{}, Saved{})
	if e.Hostname != "" || e.Eth.Mode != "auto" || e.SSID != "" {
		t.Fatalf("defaults: %+v (no host name unless emm.txt or the UI chose one)", e)
	}
	// A boot SSID never inherits the saved password.
	e = mergeConfig(BootConfig{WifiSSID: "A"}, Saved{WifiSSID: "B", WifiPassword: "secret-b-1"})
	if e.SSID != "A" || e.PSK != "" {
		t.Fatalf("boot ssid must bring its own password: %+v", e)
	}
	e = mergeConfig(BootConfig{Ethernet: "static", EthAddress: "10.0.0.2/24"}, Saved{Ethernet: EthernetSettings{Mode: "dhcp"}})
	if e.Eth.Mode != "static" || e.Eth.Address != "10.0.0.2/24" {
		t.Fatalf("%+v", e.Eth)
	}
}

// ---- Save/Load hooks -------------------------------------------------------

func TestLoadHookFeedsInitialState(t *testing.T) {
	e := newEnv(t, withSaved(Saved{Hostname: "persisted", Ethernet: EthernetSettings{Mode: "static", Address: "10.1.1.2/24"}}))
	if st := e.m.Status(); st.Hostname != "persisted" || st.Ethernet.Mode != "static" {
		t.Fatalf("status before Run: %+v", st)
	}
}

func TestSavedJSONRoundTrip(t *testing.T) {
	in := Saved{Hostname: "h", WifiSSID: "s", WifiPassword: "p", WifiCountry: "DK",
		Ethernet: EthernetSettings{Mode: "static", Address: "1.2.3.4/24", Gateway: "1.2.3.1", DNS: []string{"1.1.1.1"}}}
	b, err := jsonMarshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"hostname"`, `"wifi_ssid"`, `"wifi_password"`, `"wifi_country"`, `"ethernet"`, `"mode"`, `"address"`, `"gateway"`, `"dns"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("missing %s in %s", key, b)
		}
	}
}

// ---- Run -------------------------------------------------------------------

func TestRunBootsMonitorsAndStopsOnCancel(t *testing.T) {
	e := newEnv(t, withCable())
	e.blockAfter = 3 // two monitor ticks, then block until cancelled
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.m.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for e.m.Status().Mode != "ethernet" {
		if time.Now().After(deadline) {
			t.Fatalf("never came up: %+v", e.m.Status())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// ---- fake ------------------------------------------------------------------

func TestNewFakeSetupThenConnect(t *testing.T) {
	m := NewFake(Options{
		NMDir: t.TempDir(), DNSMasqDir: t.TempDir(),
		Timing: Timing{NMReady: time.Second, EthernetWait: 50 * time.Millisecond, EthernetNoCarrier: 10 * time.Millisecond,
			WifiWait: 3 * time.Second, Poll: 5 * time.Millisecond, Monitor: 20 * time.Millisecond,
			SetupEthCheck: time.Hour, ConnectDelay: 10 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m.o.Runner.(*fakeWorld).joinDelay = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, m.Status())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitFor("setup mode", func() bool { return m.Status().Mode == "ap" })
	if nets, err := m.Scan(ctx, false); err != nil || len(nets) != 8 {
		t.Fatalf("scan: %d %v", len(nets), err)
	}
	if err := m.Connect("Fiber-3F21", "wrong"); err != nil {
		t.Fatal(err)
	}
	waitFor("failure reported", func() bool { s := m.Status(); return s.Mode == "ap" && !s.Joining && s.LastError != "" })
	if err := m.Connect("Fiber-3F21", "password1"); err != nil {
		t.Fatal(err)
	}
	waitFor("client mode", func() bool { s := m.Status(); return s.Mode == "client" && s.SSID == "Fiber-3F21" })
	if m.InSetupMode() || m.Status().LastError != "" {
		t.Fatalf("status %+v", m.Status())
	}
}

func TestNewFakeStartsAsClientWhenWifiSaved(t *testing.T) {
	var held = Saved{WifiSSID: "Some Other Net", WifiPassword: "password1"}
	m := NewFake(Options{
		NMDir: t.TempDir(), DNSMasqDir: t.TempDir(),
		Load:   func() Saved { return held },
		Save:   func(s Saved) error { held = s; return nil },
		Timing: Timing{NMReady: time.Second, EthernetWait: 50 * time.Millisecond, EthernetNoCarrier: 10 * time.Millisecond, WifiWait: 3 * time.Second, Poll: 5 * time.Millisecond, Monitor: time.Hour},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	m.o.Runner.(*fakeWorld).joinDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for m.Status().Mode != "client" {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", m.Status())
		}
		time.Sleep(2 * time.Millisecond)
	}
}
