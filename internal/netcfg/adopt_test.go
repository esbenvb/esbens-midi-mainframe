package netcfg

import (
	"strings"
	"testing"
	"time"
)

// A Pi OS box that is already online (WiFi from Raspberry Pi Imager, nmtui or
// raspi-config) must not lose that connection to the setup access point just
// because EMM was installed on it. EMM adopts what is there and leaves every
// profile that is not its own (emm-*) alone.

// withForeignWifi adds a profile somebody else made. Imager calls its profile
// "preconfigured", which is not the network name. With active the connection is
// up before emmd starts; otherwise NetworkManager has to bring it up (see
// activateForeign).
func withForeignWifi(id, ssid string, active bool) envOpt {
	return withWorld(func(w *fakeWorld) {
		w.foreign = append(w.foreign, fakeProfile{ID: id, SSID: ssid, Autoconnect: true})
		if active {
			if err := w.tryForeignLocked(bg, id); err != nil {
				panic(err)
			}
		}
	})
}

// foreignUntouched fails the test unless the foreign profile id still exists and
// no command was aimed at it that would change or remove it.
func (e *env) foreignUntouched(id string) {
	e.t.Helper()
	e.w.mu.Lock()
	_, ok := e.w.foreignLocked(id)
	e.w.mu.Unlock()
	if !ok {
		e.t.Fatalf("the profile %q was deleted", id)
	}
	for _, c := range []string{"connection delete " + id, "connection down " + id, "connection modify " + id} {
		if e.cmdIndex(c) >= 0 {
			e.t.Fatalf("%q was run: %v", c, e.w.cmds)
		}
	}
}

func (e *env) activeWifi() string {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	return e.w.wifiConn
}

func TestAdoptsTheWifiThatIsAlreadyUp(t *testing.T) {
	for _, ssid := range []string{"HomeNet-5G", "Studio:5G"} {
		t.Run(ssid, func(t *testing.T) {
			// No emm.txt WiFi, nothing saved, no cable: the installer's situation.
			e := newEnv(t, withForeignWifi("preconfigured", ssid, true))
			e.start()
			st := e.m.Status()
			if st.Mode != "client" || st.SSID != ssid || st.IP == "" || st.SetupMode || !st.Adopted || st.LastError != "" {
				t.Fatalf("status %+v", st)
			}
			if e.m.InSetupMode() || e.apUps() != 0 || e.cmdCount("emm-setup-ap") != 0 || e.cmdCount("wifi list") != 0 {
				t.Fatalf("the access point or a scan was started: %v", e.w.cmds)
			}
			e.foreignUntouched("preconfigured")
			if e.activeWifi() != "preconfigured" {
				t.Fatalf("the connection was replaced by %q", e.activeWifi())
			}
			if _, ok := e.profile(wifiConnID); ok {
				t.Fatal("EMM must not create a WiFi profile of its own for a network it did not configure")
			}
			if e.elapsed() > 5*time.Second {
				t.Fatalf("took %v although the connection was up already", e.elapsed())
			}
			if e.m.SavedSSID() != "" || len(e.saves) != 0 {
				t.Fatalf("nothing was configured, nothing may be saved: %q %v", e.m.SavedSSID(), e.saves)
			}

			// And it keeps monitoring: the status follows the connection.
			for i := 0; i < 3; i++ {
				e.advance(time.Minute)
				e.m.monitorTick(bg)
			}
			if st := e.m.Status(); st.Mode != "client" || st.SSID != ssid || !st.Adopted || st.SetupMode {
				t.Fatalf("after monitoring: %+v", st)
			}
			if e.apUps() != 0 {
				t.Fatal("the access point came up")
			}
		})
	}
}

func TestAdoptWaitsForNetworkManagerToJoinItsOwnProfile(t *testing.T) {
	// The boot race: emmd starts as soon as NetworkManager runs, seconds before it
	// has joined the profile Imager made. A missing cable is worth 5 s, a known
	// WiFi profile 45.
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", false))
	e.at(12*time.Second, func() {
		if err := e.w.activateForeign(bg, "preconfigured"); err != nil {
			t.Error(err)
		}
	})
	e.start()
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || !st.Adopted {
		t.Fatalf("status %+v", st)
	}
	if e.apUps() != 0 || e.cmdCount("emm-setup-ap") != 0 {
		t.Fatalf("the access point was started while NetworkManager was still joining: %v", e.w.cmds)
	}
	if e.elapsed() < 12*time.Second || e.elapsed() > 20*time.Second {
		t.Fatalf("elapsed %v", e.elapsed())
	}
	e.foreignUntouched("preconfigured")
}

func TestAdoptWaitsWhileTheDeviceIsConnecting(t *testing.T) {
	// The profile listing is only a hint: a device that is activating something
	// is waited for too.
	e := newEnv(t)
	e.m.o.Runner = failingRunner{Runner: e.w, fail: func(name string, args []string) bool {
		return name == "nmcli" && len(args) > 0 && args[len(args)-1] == "show" && strings.Contains(strings.Join(args, " "), "NAME,TYPE")
	}}
	// Something other than EMM has the device stuck in "connecting".
	e.w.mu.Lock()
	e.w.connecting = true
	e.w.mu.Unlock()
	e.start()
	if e.elapsed() < 45*time.Second {
		t.Fatalf("gave up after %v on a device that is connecting", e.elapsed())
	}
}

func TestAdoptGivesUpWhenTheForeignWifiNeverComes(t *testing.T) {
	// The Pi was moved: the network of the profile is not here. After the usual
	// waits (the WiFi wait, 45 s) there is no connectivity, so setup mode.
	e := newEnv(t, withForeignWifi("preconfigured", "Not Here", false))
	e.start()
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.Adopted || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	if e.elapsed() < 45*time.Second || e.elapsed() > 50*time.Second {
		t.Fatalf("elapsed %v", e.elapsed())
	}
	e.foreignUntouched("preconfigured")
	if st.LastError != "" {
		t.Fatalf("EMM had no WiFi to fail with: %q", st.LastError)
	}
}

func TestNothingToAdoptStillGoesStraightToSetup(t *testing.T) {
	// A foreign profile that cannot autoconnect is no reason to wait.
	e := newEnv(t, withWorld(func(w *fakeWorld) {
		w.foreign = append(w.foreign, fakeProfile{ID: "manual-only", SSID: "HomeNet-5G"})
	}))
	e.start()
	if st := e.m.Status(); st.Mode != "ap" || !st.SetupMode {
		t.Fatalf("status %+v", st)
	}
	if e.elapsed() > 10*time.Second {
		t.Fatalf("took %v", e.elapsed())
	}
	e.foreignUntouched("manual-only")
}

func TestResetNetworkAdoptsNothing(t *testing.T) {
	// reset-network means setup mode for this boot, whatever is online.
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true), withBootFlag(ResetFileName))
	e.start()
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.Adopted || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	// Nor is the old connection retried later.
	e.advance(30 * time.Minute)
	e.m.monitorTick(bg)
	if !e.m.InSetupMode() || e.cmdCount("connection up id preconfigured") != 0 || e.cmdCount("station dump") != 0 {
		t.Fatalf("setup mode was left or retried: %v", e.w.cmds)
	}
	e.foreignUntouched("preconfigured")
}

func TestForgetWithNothingSavedLeavesTheAdoptedWifiAlone(t *testing.T) {
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true))
	e.start()
	cmds := len(e.w.cmds)
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	if len(e.w.cmds) != cmds || len(e.saves) != 0 {
		t.Fatalf("Forget did something: %v", e.w.cmds[cmds:])
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || !st.Adopted || e.apUps() != 0 {
		t.Fatalf("status %+v", st)
	}
	e.foreignUntouched("preconfigured")
}

func TestConnectFromTheAdoptedStateCreatesEmmsOwnProfile(t *testing.T) {
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true))
	e.start()
	if err := e.m.Connect("Fiber-3F21", "newpassword"); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "Fiber-3F21" || st.Joining || st.Adopted || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	if prof, ok := e.profile(wifiConnID); !ok || !strings.Contains(prof, "ssid=Fiber-3F21\n") {
		t.Fatalf("EMM's own profile:\n%s", prof)
	}
	if got := e.savedNow(); got.WifiSSID != "Fiber-3F21" || got.WifiPassword != "newpassword" {
		t.Fatalf("saved %+v", got)
	}
	e.foreignUntouched("preconfigured")

	// After a reboot the saved network is EMM's WiFi like any other.
	e2 := newEnv(t, withSaved(e.savedNow()), withForeignWifi("preconfigured", "HomeNet-5G", true))
	e2.start()
	if st := e2.m.Status(); st.Mode != "client" || st.SetupMode {
		t.Fatalf("after reboot: %+v", st)
	}
	if prof, _ := e2.profile(wifiConnID); !strings.Contains(prof, "ssid=Fiber-3F21\n") {
		t.Fatalf("EMM's profile after reboot:\n%s", prof)
	}
}

func TestFailedConnectFromTheAdoptedStateGoesBackToIt(t *testing.T) {
	// `connection up` of the new profile takes the radio from the adopted
	// connection. A headless box must not be left without a network when the
	// password was wrong.
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true))
	e.start()
	err := e.m.Connect("linksys", "wrongpassword")
	if err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("err %v", err)
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || !st.Adopted || st.Joining || st.SetupMode {
		t.Fatalf("not back on the adopted network: %+v", st)
	}
	if e.activeWifi() != "preconfigured" {
		t.Fatalf("active connection %q", e.activeWifi())
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("the failed profile stayed")
	}
	if got := e.savedNow(); got.WifiSSID != "" || got.WifiPassword != "" {
		t.Fatalf("failed credentials stayed saved: %+v", got)
	}
	e.foreignUntouched("preconfigured")
}

func TestForgetAfterConnectGoesBackToTheAdoptedWifi(t *testing.T) {
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true))
	e.start()
	if err := e.m.Connect("Fiber-3F21", "newpassword"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || !st.Adopted || st.SetupMode || e.apUps() != 0 {
		t.Fatalf("status %+v", st)
	}
	if got := e.savedNow(); got.WifiSSID != "" {
		t.Fatalf("saved %+v", got)
	}
	if _, ok := e.profile(wifiConnID); ok {
		t.Fatal("EMM's profile stayed")
	}
	e.foreignUntouched("preconfigured")
}

func TestForgetStillEntersSetupModeWithoutAnythingToGoBackTo(t *testing.T) {
	// The adoption must not change what Forget has always done for EMM's own WiFi.
	e := newEnv(t, withSaved(wifiHome))
	e.start()
	if err := e.m.Forget(); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(); st.Mode != "ap" || !st.SetupMode {
		t.Fatalf("status %+v", st)
	}
}

func TestAdoptedWifiDroppingDoesNotStartSetupMode(t *testing.T) {
	// The router reboots. NetworkManager brings the connection back by itself; an
	// access point would only get in its way.
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true))
	e.start()
	e.w.mu.Lock()
	e.w.wifiConn = ""
	e.w.mu.Unlock()
	for i := 0; i < 10; i++ {
		e.advance(time.Minute)
		e.m.monitorTick(bg)
	}
	if st := e.m.Status(); st.Mode != "offline" || st.SetupMode || st.Adopted || e.apUps() != 0 {
		t.Fatalf("status %+v", st)
	}
	if err := e.w.activateForeign(bg, "preconfigured"); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.m.monitorTick(bg)
	if st := e.m.Status(); st.Mode != "client" || st.SSID != "HomeNet-5G" || !st.Adopted {
		t.Fatalf("status %+v", st)
	}
}

func TestEmmDoesNotAdoptWhenItHasWifiOfItsOwn(t *testing.T) {
	// With a WiFi network in emm.txt or the saved settings EMM joins that one, and
	// when it cannot the box falls back to setup mode: as before.
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"), withForeignWifi("preconfigured", "linksys", false))
	e.start()
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || !strings.Contains(st.LastError, `"HomeNet-5G"`) {
		t.Fatalf("status %+v", st)
	}
	if e.elapsed() < 45*time.Second {
		t.Fatalf("gave up after %v", e.elapsed())
	}
	e.foreignUntouched("preconfigured")
}
