package netcfg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers ---------------------------------------------------------------

// hookRunner lets a test look at (and react to) every command.
type hookRunner struct {
	Runner
	before func(name string, args []string)
}

func (h hookRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if h.before != nil {
		h.before(name, args)
	}
	return h.Runner.Run(ctx, name, args...)
}

// routerDown makes joining ssid fail as if its access point were off.
func routerDown(ssid string) envOpt {
	return withWorld(func(w *fakeWorld) { w.failSSIDs[ssid] = true })
}

func (e *env) routerUp(ssid string) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	delete(e.w.failSSIDs, ssid)
}

func (e *env) setStations(macs ...string) {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.stations = macs
}

func (e *env) apUps() int {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	return e.w.apUps
}

func withHiddenNetwork(ssid string) envOpt {
	return withWorld(func(w *fakeWorld) {
		w.networks = append(w.networks, fakeNet{SSID: ssid, Signal: 55, Security: "WPA2"})
		w.hidden[ssid] = true
	})
}

var bg = context.Background()

// ---- R2: hidden networks ---------------------------------------------------

func TestConnectHiddenNetworkMarksProfileHiddenAndSurvivesReboot(t *testing.T) {
	e := newEnv(t, withHiddenNetwork("Secret Studio"))
	e.start()
	if nets, _ := e.m.Scan(bg, false); scanHas(nets, "Secret Studio") {
		t.Fatal("precondition: the hidden network must not be in the scan")
	}

	if err := e.m.Connect("Secret Studio", "password1"); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "Secret Studio" {
		t.Fatalf("status %+v", st)
	}
	if prof, _ := e.profile(wifiConnID); !strings.Contains(prof, "\nhidden=true\n") {
		t.Fatalf("profile of a network that is not in the scan must be hidden:\n%s", prof)
	}
	saved := e.savedNow()
	if !saved.WifiHidden {
		t.Fatalf("the decision must be saved: %+v", saved)
	}

	// After a reboot nobody has scanned, and NetworkManager autoconnects: that
	// only finds a hidden network when the profile says so.
	e2 := newEnv(t, withSaved(saved), withHiddenNetwork("Secret Studio"))
	e2.start()
	if st := e2.m.Status(); st.Mode != "client" || st.SSID != "Secret Studio" {
		t.Fatalf("after reboot: %+v", st)
	}
	// Control: without the flag the same boot ends in setup mode.
	saved.WifiHidden = false
	e3 := newEnv(t, withSaved(saved), withHiddenNetwork("Secret Studio"))
	e3.start()
	if st := e3.m.Status(); !st.SetupMode {
		t.Fatalf("control: %+v", st)
	}

	// Forgetting the network forgets the flag too.
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	if got := e.savedNow(); got.WifiHidden || got.WifiSSID != "" {
		t.Fatalf("after Forget: %+v", got)
	}
}

func TestConnectVisibleNetworkIsNotHidden(t *testing.T) {
	e := newEnv(t)
	e.start()
	if err := e.m.Connect("HomeNet-5G", "secret123"); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	if prof, _ := e.profile(wifiConnID); strings.Contains(prof, "hidden") {
		t.Fatalf("a network in the scan must not be marked hidden:\n%s", prof)
	}
	if e.savedNow().WifiHidden {
		t.Fatal("saved as hidden")
	}
}

func TestBootManagedHiddenNetworkIsJudgedByTheScan(t *testing.T) {
	// emm.txt has no record of how the network was chosen. With no scan yet it
	// is written as visible; once there is a scan without it, it is hidden.
	e := newEnv(t, withBootFile("wifi_ssid = Secret Studio\nwifi_password = password1\n"), withHiddenNetwork("Secret Studio"))
	e.start()
	if prof, _ := e.profile(wifiConnID); strings.Contains(prof, "hidden") {
		t.Fatalf("no scan yet, no guess:\n%s", prof)
	}
	if !e.m.Status().SetupMode {
		t.Fatalf("autoconnect cannot find it without hidden=true: %+v", e.m.Status())
	}
	// Setup mode scanned; three minutes later the retry renders the profile again.
	e.clk.advance(4 * time.Minute)
	e.m.monitorTick(bg)
	if st := e.m.Status(); st.Mode != "client" || st.SetupMode {
		t.Fatalf("retry did not join: %+v", st)
	}
	if prof, _ := e.profile(wifiConnID); !strings.Contains(prof, "hidden=true") {
		t.Fatalf("profile:\n%s", prof)
	}
}

// ---- R5: setup mode retries the configured WiFi ----------------------------

func TestSetupRetryJoinsWhenTheRouterAppears(t *testing.T) {
	for name, opt := range map[string]envOpt{
		"saved":   withSaved(wifiHome),
		"emm.txt": withBootFile("wifi_ssid = HomeNet-5G\nwifi_password = secret123\n"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opt, routerDown("HomeNet-5G"))
			e.start()
			if st := e.m.Status(); st.Mode != "ap" || !st.SetupMode || !strings.Contains(st.LastError, "HomeNet-5G") {
				t.Fatalf("precondition: %+v", st)
			}

			// Too early: not even a look at the stations.
			e.clk.advance(time.Minute)
			e.m.monitorTick(bg)
			if e.cmdCount("station dump") != 0 || !e.m.InSetupMode() {
				t.Fatalf("retried after one minute: %v", e.w.cmds)
			}

			e.routerUp("HomeNet-5G") // the router finished booting
			e.clk.advance(3 * time.Minute)
			e.m.monitorTick(bg)

			st := e.m.Status()
			if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || st.LastError != "" || st.IP == "" {
				t.Fatalf("status %+v", st)
			}
			if e.m.InSetupMode() || e.apUp() {
				t.Fatal("still in setup mode")
			}
			if _, ok := e.profile(apConnID); ok {
				t.Fatal("AP profile must be removed")
			}
			if _, ok := e.dnsSnippet(); ok {
				t.Fatal("captive DNS snippet must be removed")
			}
			if prof, ok := e.profile(wifiConnID); !ok || !strings.Contains(prof, "ssid=HomeNet-5G\n") {
				t.Fatalf("wifi profile %q", prof)
			}
			dump, down := e.cmdIndex("iw dev wlan0 station dump"), e.cmdIndex("connection down emm-setup-ap")
			if dump < 0 || down < 0 || dump > down {
				t.Fatalf("must look at the stations before taking the AP down (dump=%d down=%d): %v", dump, down, e.w.cmds)
			}
		})
	}
}

func TestSetupRetrySkippedWhileAClientIsConnected(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"))
	e.start()
	before, errBefore := e.apUps(), e.m.Status().LastError
	e.setStations("9a:3c:11:22:33:44")

	e.routerUp("HomeNet-5G")
	e.clk.advance(3*time.Minute + time.Second)
	e.m.monitorTick(bg)
	st := e.m.Status()
	if !st.SetupMode || st.Mode != "ap" || !e.apUp() || e.apUps() != before {
		t.Fatalf("a phone is on the AP, it must be left alone: %+v", st)
	}
	if e.cmdIndex("iw dev wlan0 station dump") < 0 {
		t.Fatal("the stations were never checked")
	}
	if e.cmdCount("connection up emm-wifi") != 0 || e.cmdCount("connection down emm-setup-ap") != 0 {
		t.Fatalf("the radio was touched: %v", e.w.cmds)
	}
	if st.LastError != errBefore {
		t.Fatalf("LastError changed: %q -> %q", errBefore, st.LastError)
	}

	// The phone leaves. A skipped round restarts the clock, so there is a full
	// period of grace before the retry.
	e.setStations()
	e.clk.advance(time.Minute)
	e.m.monitorTick(bg)
	if !e.m.InSetupMode() {
		t.Fatal("retried right after the client left")
	}
	e.clk.advance(3 * time.Minute)
	e.m.monitorTick(bg)
	if st := e.m.Status(); st.Mode != "client" || st.SetupMode {
		t.Fatalf("status %+v", st)
	}
}

func TestSetupRetrySkippedWhenStationsCannotBeListed(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"))
	e.start()
	// `iw` fails (missing, or the interface is not an AP): nobody can tell who is connected.
	e.m.o.Runner = failingRunner{Runner: e.w, fail: func(name string, args []string) bool {
		return name == "iw" && len(args) > 2 && args[2] == "station"
	}}
	e.routerUp("HomeNet-5G")
	e.clk.advance(4 * time.Minute)
	e.m.monitorTick(bg)
	if !e.m.InSetupMode() || e.cmdCount("connection down emm-setup-ap") != 0 {
		t.Fatalf("without knowing who is connected the AP must stay: %+v", e.m.Status())
	}
}

type failingRunner struct {
	Runner
	fail func(name string, args []string) bool
}

func (f failingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if f.fail(name, args) {
		return "", &RunError{Cmd: name, Err: errors.New("exit status 1"), Stderr: "command failed\n"}
	}
	return f.Runner.Run(ctx, name, args...)
}

func TestSetupRetryFailureRestoresTheAP(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"))
	e.start()
	before := e.apUps()

	e.clk.advance(3*time.Minute + time.Second)
	e.m.monitorTick(bg)

	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || !e.apUp() || e.apUps() != before+1 || st.APSSID != "EMM-Setup-A1C2" || st.IP != APAddress {
		t.Fatalf("AP not restored: %+v (apUps %d -> %d)", st, before, e.apUps())
	}
	if !strings.Contains(st.LastError, `"HomeNet-5G"`) || !strings.Contains(st.LastError, "could not be found") || !strings.Contains(st.LastError, "later") {
		t.Fatalf("LastError %q", st.LastError)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("client profile must not stay while in setup mode")
	}
	if _, ok := e.profile(apConnID); !ok {
		t.Fatal("AP profile missing")
	}
	if _, ok := e.dnsSnippet(); !ok {
		t.Fatal("captive DNS snippet missing")
	}
	if e.w.scansDuringAP != 0 {
		t.Fatal("scanned while the AP was up")
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || len(e.saves) != 0 {
		t.Fatalf("a failed retry must not touch the saved network: %+v (saves %d)", got, len(e.saves))
	}

	// The next attempt is a full period later, and works once the router is back.
	e.clk.advance(time.Minute)
	e.m.monitorTick(bg)
	if !e.m.InSetupMode() || e.apUps() != before+1 {
		t.Fatal("retried too soon")
	}
	e.routerUp("HomeNet-5G")
	e.clk.advance(3 * time.Minute)
	e.m.monitorTick(bg)
	if st := e.m.Status(); st.Mode != "client" || st.SetupMode || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestSetupRetryDoesNotWaitAsLongAsBoot(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withWorld(func(w *fakeWorld) { w.wifiNeverConnects = true }))
	e.start()
	e.clk.advance(4 * time.Minute)
	t0 := e.clk.Now()
	e.m.monitorTick(bg)
	if took := e.clk.Now().Sub(t0); took > 30*time.Second {
		t.Fatalf("the AP was down for %v; a retry must give up long before the 45 s boot wait", took)
	}
	if st := e.m.Status(); st.Mode != "ap" || !e.apUp() || !strings.Contains(st.LastError, "timed out") {
		t.Fatalf("status %+v", st)
	}
}

func TestSetupRetryNotAfterResetNetwork(t *testing.T) {
	// reset-network promises to ignore the configured WiFi for that boot.
	e := newEnv(t, withSaved(wifiHome), withBootFlag(ResetFileName))
	e.start()
	e.clk.advance(30 * time.Minute)
	e.m.monitorTick(bg)
	if !e.m.InSetupMode() || !e.apUp() {
		t.Fatalf("left setup mode: %+v", e.m.Status())
	}
	if e.cmdCount("station dump") != 0 || e.cmdCount("connection up emm-wifi") != 0 {
		t.Fatalf("the old WiFi was tried: %v", e.w.cmds)
	}
}

func TestSetupRetryNotWhileAJoinIsPending(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"))
	e.start()
	e.gate = make(chan struct{})
	if err := e.m.Connect("linksys", "password1"); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(5 * time.Minute)
	e.m.monitorTick(bg)
	if e.cmdCount("station dump") != 0 || e.cmdCount("connection down emm-setup-ap") != 0 {
		t.Fatalf("the monitor interfered with a pending join: %v", e.w.cmds)
	}
	close(e.gate)
	e.m.bg.Wait()
	if st := e.m.Status(); st.Mode != "client" || st.SSID != "linksys" {
		t.Fatalf("status %+v", st)
	}
}

func TestSetupRetryNotWithUnusableWifiSettings(t *testing.T) {
	// A too-short password in emm.txt can never join: do not bounce the AP for it.
	e := newEnv(t, withBootFile("wifi_ssid = HomeNet-5G\nwifi_password = abc\n"))
	e.start()
	if st := e.m.Status(); !st.SetupMode || !strings.Contains(st.LastError, "unusable") {
		t.Fatalf("precondition: %+v", st)
	}
	before := e.apUps()
	e.clk.advance(10 * time.Minute)
	e.m.monitorTick(bg)
	if e.cmdCount("station dump") != 0 || e.cmdCount("connection down emm-setup-ap") != 0 || e.apUps() != before {
		t.Fatalf("the AP was disturbed for settings that cannot work: %v", e.w.cmds)
	}
}

func TestSetupRetryNoWifiConfiguredDoesNothing(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.clk.advance(time.Hour)
	e.m.monitorTick(bg)
	if e.cmdCount("station dump") != 0 || !e.apUp() {
		t.Fatalf("nothing to retry, nothing to do: %v", e.w.cmds)
	}
}

// ---- R6: the AP always comes back -----------------------------------------

func TestForcedScanLeavesTheAPAloneWhenTheCallerHasGivenUp(t *testing.T) {
	e := newEnv(t)
	e.start()
	cmds := len(e.w.cmds)
	ctx, cancel := context.WithCancel(bg)
	cancel() // e.g. the request timed out while waiting for opMu
	if _, err := e.m.Scan(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.LastError != "" || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	if len(e.w.cmds) != cmds {
		t.Fatalf("the radio was touched: %v", e.w.cmds[cmds:])
	}
}

func TestForcedScanRestoresTheAPEvenIfTheCallerEndsMidScan(t *testing.T) {
	e := newEnv(t)
	e.start()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var once sync.Once
	e.m.o.Runner = hookRunner{Runner: e.w, before: func(name string, args []string) {
		if name == "nmcli" && strings.Contains(strings.Join(args, " "), "wifi list") {
			once.Do(cancel) // the HTTP request ends during the scan
		}
	}}
	if _, err := e.m.Scan(ctx, true); err == nil {
		t.Fatal("the scan should report that it was cut short")
	}
	st := e.m.Status()
	if !e.apUp() || st.Mode != "ap" || !st.SetupMode || st.LastError != "" {
		t.Fatalf("the AP is down or the status is wrong: %+v apUp=%v", st, e.apUp())
	}
}

func TestMonitorRestoresAnAPThatFailedToComeBack(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.w.mu.Lock()
	e.w.apUpFails = 2 // the restore after the scan fails, and so does the first retry
	e.w.mu.Unlock()
	if _, err := e.m.Scan(bg, true); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(); st.Mode != "offline" || !strings.Contains(st.LastError, "could not restart the setup network") || e.apUp() {
		t.Fatalf("precondition: %+v apUp=%v", st, e.apUp())
	}
	ups := e.cmdCount("connection up emm-setup-ap")

	e.m.monitorTick(bg) // attempt 1: fails
	if e.cmdCount("connection up emm-setup-ap") != ups+1 || e.apUp() {
		t.Fatalf("the monitor did not try: %v", e.w.cmds)
	}
	e.m.monitorTick(bg) // too soon: backs off
	if e.cmdCount("connection up emm-setup-ap") != ups+1 {
		t.Fatal("no backoff between attempts")
	}
	e.clk.advance(6 * time.Second)
	e.m.monitorTick(bg) // attempt 2: works
	st := e.m.Status()
	if !e.apUp() || st.Mode != "ap" || !st.SetupMode || st.LastError != "" || st.IP != APAddress || st.APSSID == "" {
		t.Fatalf("not recovered: %+v apUp=%v", st, e.apUp())
	}
	if _, ok := e.dnsSnippet(); !ok {
		t.Fatal("captive DNS snippet missing")
	}
}

func TestMonitorRestoresAnAPThatFailedAtBoot(t *testing.T) {
	e := newEnv(t, withWorld(func(w *fakeWorld) { w.apUpFails = 1 }))
	e.start()
	if st := e.m.Status(); st.Mode != "offline" || !st.SetupMode || e.apUp() {
		t.Fatalf("precondition: %+v", st)
	}
	e.m.monitorTick(bg)
	if st := e.m.Status(); !e.apUp() || st.Mode != "ap" || st.LastError != "" {
		t.Fatalf("not recovered: %+v", st)
	}
}

func TestMonitorLeavesAHealthyAPAlone(t *testing.T) {
	e := newEnv(t)
	e.start()
	ups := e.apUps()
	for i := 0; i < 5; i++ {
		e.clk.advance(time.Minute)
		e.m.monitorTick(bg)
	}
	if e.apUps() != ups {
		t.Fatalf("the AP was restarted although it was fine (%d -> %d)", ups, e.apUps())
	}
}

// ---- R7: overlapping Connect calls ----------------------------------------

func TestConnectWhileAJoinIsPendingIsRefused(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.gate = make(chan struct{})

	if err := e.m.Connect("HomeNet-5G", "wrongpassword"); err != nil {
		t.Fatal(err)
	}
	err := e.m.Connect("linksys", "wrongpassword")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second Connect: %v, want ErrBusy", err)
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" || len(e.saves) != 1 {
		t.Fatalf("the refused call must not touch anything: %+v (saves %d)", got, len(e.saves))
	}

	close(e.gate)
	e.m.bg.Wait()
	st := e.m.Status()
	if !st.SetupMode || st.Joining || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	if !strings.Contains(st.LastError, `Could not join "HomeNet-5G"`) || strings.Contains(st.LastError, "empty") {
		t.Fatalf("LastError %q", st.LastError)
	}
	if got := e.savedNow(); got.WifiSSID != "" || got.WifiPassword != "" {
		t.Fatalf("the failed credentials were left behind: %+v", got)
	}
	// Once it is over, Connect works again.
	if err := e.m.Connect("CoffeeShop Open", ""); err != nil {
		t.Fatal(err)
	}
	e.m.bg.Wait()
	if st := e.m.Status(); st.Mode != "client" || st.SSID != "CoffeeShop Open" {
		t.Fatalf("status %+v", st)
	}
}

func TestFailedJoinOnlyRollsBackTheWifiFields(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.gate = make(chan struct{})
	if err := e.m.Connect("HomeNet-5G", "wrongpassword"); err != nil {
		t.Fatal(err)
	}
	// Meanwhile somebody changes the Ethernet settings and the host name.
	if err := e.m.SetEthernet(EthernetSettings{Mode: "static", Address: "192.168.1.50/24"}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	close(e.gate)
	e.m.bg.Wait()

	got := e.savedNow()
	if got.WifiSSID != "" || got.WifiPassword != "" {
		t.Fatalf("wifi not rolled back: %+v", got)
	}
	if got.Ethernet.Mode != "static" || got.Hostname != "kitchen" {
		t.Fatalf("the rollback reverted unrelated settings: %+v", got)
	}
	if e.m.Status().Hostname != "kitchen" || e.m.EthernetSettings().Mode != "static" {
		t.Fatalf("effective settings were reverted: %+v", e.m.Status())
	}
}

// ---- R8: no WiFi adapter is not setup mode ---------------------------------

func TestNoWifiAdapterIsNotSetupMode(t *testing.T) {
	e := newEnv(t, withWorld(func(w *fakeWorld) { w.wifiDev = "" }))
	e.start()
	st := e.m.Status()
	if st.Mode != "offline" || !strings.Contains(st.LastError, "no WiFi adapter") {
		t.Fatalf("status %+v", st)
	}
	if st.SetupMode || e.m.InSetupMode() {
		t.Fatal("setup mode without an access point would switch off the web password for nothing")
	}
	if st.APSSID != "" || st.IP != "" {
		t.Fatalf("status claims an AP: %+v", st)
	}
	if e.m.PortalURL() != "http://raspberrypi.local/" {
		t.Fatalf("portal %q: nobody can reach the AP address", e.m.PortalURL())
	}
	if e.apUps() != 0 || e.cmdCount("emm-setup-ap") != 0 {
		t.Fatalf("an AP was attempted: %v", e.w.cmds)
	}
	if _, ok := e.dnsSnippet(); ok {
		t.Fatal("captive DNS snippet written")
	}

	// A cable plugged in later is picked up like on any other box.
	e.w.mu.Lock()
	e.w.ethCable = true
	e.w.mu.Unlock()
	e.clk.advance(time.Minute)
	e.m.monitorTick(bg)
	st = e.m.Status()
	if st.Mode != "ethernet" || st.IP != "192.168.1.50" || st.SetupMode || st.LastError != "" {
		t.Fatalf("after the cable: %+v", st)
	}
}

func TestNoWifiAdapterWithConfiguredWifiAndResetStaysOutOfSetupMode(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), withBootFlag(ResetFileName), withWorld(func(w *fakeWorld) { w.wifiDev = "" }))
	e.start()
	if st := e.m.Status(); st.SetupMode || st.Mode != "offline" {
		t.Fatalf("status %+v", st)
	}
	// Forget with no radio must not invent a setup mode either.
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	if e.m.InSetupMode() {
		t.Fatal("setup mode after Forget")
	}
}
