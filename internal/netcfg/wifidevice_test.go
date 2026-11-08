package netcfg

import (
	"strings"
	"testing"
	"time"
)

// On the appliance image the WiFi driver is a module that udev loads, and the
// wlan device may appear seconds after emmd's first look. The boot sequence
// waits for it (Timing.WifiDeviceWait) and the monitor loop picks up one that
// comes later still.

func noWifiDevice() envOpt { return withWorld(func(w *fakeWorld) { w.wifiDev = "" }) }

// wifiDeviceAt makes the adapter appear when the fake clock reaches d, and
// returns a function that tells how many commands had run by then.
func (e *env) wifiDeviceAt(d time.Duration) func() int {
	var cmdsBefore int
	e.at(d, func() {
		e.w.mu.Lock()
		cmdsBefore = len(e.w.cmds)
		e.w.mu.Unlock()
		e.w.addWifiDevice(bg, "wlan0")
	})
	return func() int { return cmdsBefore }
}

func (e *env) ethernetCableIn() {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	e.w.ethCable = true
}

func TestBootWaitsForTheWifiDeviceAndJoinsTheConfiguredWifi(t *testing.T) {
	for name, opts := range map[string][]envOpt{
		"saved":   {withSaved(wifiHome), withBootFile("wifi_country = DK\n")},
		"emm.txt": {withBootFile("wifi_country = DK\nwifi_ssid = HomeNet-5G\nwifi_password = secret123\n")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, append(opts, noWifiDevice())...)
			cmdsAtAppear := e.wifiDeviceAt(9 * time.Second)
			e.start()

			st := e.m.Status()
			if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || st.LastError != "" || st.IP == "" {
				t.Fatalf("status %+v", st)
			}
			if e.apUps() != 0 || e.cmdCount("emm-setup-ap") != 0 {
				t.Fatalf("the access point was started: %v", e.w.cmds)
			}
			if e.elapsed() < 9*time.Second || e.elapsed() > 20*time.Second {
				t.Fatalf("elapsed %v", e.elapsed())
			}
			if prof, ok := e.profile(wifiConnID); !ok || !strings.Contains(prof, "ssid=HomeNet-5G\n") {
				t.Fatalf("the configured WiFi was dropped: %q", prof)
			}
			// rfkill and the regulatory domain can only be handled once the device is
			// there: they ran then, and once.
			for _, c := range []string{"rfkill unblock wifi", "iw reg set DK"} {
				if n, i := e.cmdCount(c), e.cmdIndex(c); n != 1 || i < cmdsAtAppear() {
					t.Fatalf("%q ran %d times, first at %d (the device appeared after command %d)", c, n, i, cmdsAtAppear())
				}
			}
		})
	}
}

func TestBootWifiDeviceThatNeverComesEndsOffline(t *testing.T) {
	for name, wait := range map[string]time.Duration{"default 30 s": 0, "Timing.WifiDeviceWait": 10 * time.Second} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withSaved(wifiHome), noWifiDevice())
			if wait == 0 {
				wait = 30 * time.Second
			} else {
				e.m.t.WifiDeviceWait = wait
			}
			e.start()
			st := e.m.Status()
			if st.Mode != "offline" || st.SetupMode || !strings.Contains(st.LastError, "no WiFi adapter") {
				t.Fatalf("status %+v", st)
			}
			if e.elapsed() < wait || e.elapsed() > wait+5*time.Second {
				t.Fatalf("elapsed %v, want about %v", e.elapsed(), wait)
			}
			if e.cmdCount("rfkill") != 0 || e.cmdCount("iw reg") != 0 {
				t.Fatalf("radio commands without a radio: %v", e.w.cmds)
			}
		})
	}
	if got := (Timing{}).withDefaults().WifiDeviceWait; got != 30*time.Second {
		t.Fatalf("default WifiDeviceWait %v", got)
	}
}

func TestBootWifiDeviceWaitIsCheap(t *testing.T) {
	// Polling is one `nmcli device` per Timing.Poll, nothing heavier.
	e := newEnv(t, noWifiDevice())
	e.start()
	polls := e.cmdCount("nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device")
	if polls < 25 || polls > 45 {
		t.Fatalf("%d polls in %v", polls, e.elapsed())
	}
	for _, c := range []string{"wifi list", "connection up", "station dump", "device show"} {
		if e.cmdCount(c) != 0 {
			t.Fatalf("%q during the wait: %v", c, e.w.cmds)
		}
	}
}

func TestEthernetOnlineEndsTheWifiDeviceWaitAtOnce(t *testing.T) {
	for name, opts := range map[string][]envOpt{
		"nothing configured": nil,
		"saved wifi":         {withSaved(wifiHome)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, append(opts, withCable(), noWifiDevice())...)
			e.start()
			st := e.m.Status()
			if st.Mode != "ethernet" || st.SetupMode || st.IP != "192.168.1.50" {
				t.Fatalf("status %+v", st)
			}
			if e.elapsed() > 5*time.Second {
				t.Fatalf("waited %v for a WiFi device the box does not need", e.elapsed())
			}

			// The device comes later, the monitor loop notices.
			e.wifiDeviceAt(time.Minute)
			e.advance(2 * time.Minute)
			e.m.monitorTick(bg)
			if e.cmdCount("rfkill unblock wifi") != 1 {
				t.Fatalf("rfkill was not unblocked once the device was there: %v", e.w.cmds)
			}
			st = e.m.Status()
			if e.m.InSetupMode() || e.apUps() != 0 {
				t.Fatalf("setup mode although the cable is up: %+v", st)
			}
			if name == "saved wifi" {
				// NetworkManager joined the configured profile when the device appeared.
				if st.Mode != "client" || st.SSID != "HomeNet-5G" {
					t.Fatalf("status %+v", st)
				}
			} else if st.Mode != "ethernet" {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

func TestMonitorStartsTheAPWhenTheDeviceComesAfterTheWait(t *testing.T) {
	e := newEnv(t, noWifiDevice())
	e.wifiDeviceAt(40 * time.Second)
	e.start()
	if st := e.m.Status(); st.Mode != "offline" || st.SetupMode || !strings.Contains(st.LastError, "no WiFi adapter") {
		t.Fatalf("precondition: %+v (elapsed %v)", st, e.elapsed())
	}
	e.advance(5 * time.Second) // not there yet
	e.m.monitorTick(bg)
	if e.apUp() {
		t.Fatal("AP without a device")
	}
	e.advance(10 * time.Second)
	e.m.monitorTick(bg)
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || st.APSSID != "EMM-Setup-A1C2" || st.IP != APAddress || st.LastError != "" || !e.apUp() {
		t.Fatalf("status %+v", st)
	}
	scan, up := e.cmdIndex("wifi list"), e.cmdIndex("connection up emm-setup-ap")
	if scan < 0 || up < 0 || scan > up || e.w.scansDuringAP != 0 {
		t.Fatalf("must scan BEFORE bringing the AP up (scan=%d up=%d): %v", scan, up, e.w.cmds)
	}
	if e.cmdCount("rfkill unblock wifi") != 1 {
		t.Fatalf("rfkill: %v", e.w.cmds)
	}
	if nets, _ := e.m.Scan(bg, false); len(nets) != 8 {
		t.Fatalf("scan cache: %d", len(nets))
	}
}

func TestMonitorJoinsTheConfiguredWifiWhenTheDeviceComesAfterTheWait(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), noWifiDevice())
	e.wifiDeviceAt(40 * time.Second)
	e.start()
	if st := e.m.Status(); st.Mode != "offline" || !strings.Contains(st.LastError, "no WiFi adapter") {
		t.Fatalf("precondition: %+v", st)
	}
	e.advance(15 * time.Second)
	e.m.monitorTick(bg)
	st := e.m.Status()
	if st.Mode != "client" || st.SSID != "HomeNet-5G" || st.SetupMode || st.LastError != "" || st.IP == "" {
		t.Fatalf("status %+v", st)
	}
	if e.apUps() != 0 {
		t.Fatal("AP")
	}
}

func TestMonitorFallsBackToSetupModeWhenTheLateDeviceCannotJoin(t *testing.T) {
	e := newEnv(t, withSaved(wifiHome), routerDown("HomeNet-5G"), noWifiDevice())
	e.wifiDeviceAt(40 * time.Second)
	e.start()
	e.advance(15 * time.Second)
	e.m.monitorTick(bg)
	st := e.m.Status()
	if st.Mode != "ap" || !st.SetupMode || !e.apUp() || !strings.Contains(st.LastError, `Could not join WiFi network "HomeNet-5G"`) {
		t.Fatalf("status %+v", st)
	}
	if got := e.savedNow(); got.WifiSSID != "HomeNet-5G" {
		t.Fatalf("saved %+v", got)
	}
}

func TestResetNetworkWaitsForTheWifiDeviceThenStartsTheAP(t *testing.T) {
	t.Run("during boot", func(t *testing.T) {
		e := newEnv(t, withSaved(wifiHome), withBootFlag(ResetFileName), noWifiDevice())
		e.wifiDeviceAt(8 * time.Second)
		e.start()
		st := e.m.Status()
		if st.Mode != "ap" || !st.SetupMode || !e.apUp() {
			t.Fatalf("status %+v", st)
		}
		if e.elapsed() < 8*time.Second || e.elapsed() > 15*time.Second {
			t.Fatalf("elapsed %v", e.elapsed())
		}
		if e.cmdCount("connection up emm-wifi") != 0 {
			t.Fatal("the old WiFi was tried after reset-network")
		}
	})
	t.Run("after the wait", func(t *testing.T) {
		e := newEnv(t, withSaved(wifiHome), withBootFlag(ResetFileName), noWifiDevice())
		e.wifiDeviceAt(40 * time.Second)
		e.start()
		if st := e.m.Status(); st.SetupMode || st.Mode != "offline" {
			t.Fatalf("precondition: %+v", st)
		}
		e.advance(15 * time.Second)
		e.m.monitorTick(bg)
		st := e.m.Status()
		if st.Mode != "ap" || !st.SetupMode || !e.apUp() || st.LastError != "" {
			t.Fatalf("status %+v", st)
		}
		if e.cmdCount("connection up emm-wifi") != 0 {
			t.Fatal("the old WiFi was tried after reset-network")
		}
	})
}

func TestWifiDevicePresentFromTheStartCostsNoWaiting(t *testing.T) {
	e := newEnv(t)
	e.start()
	if e.elapsed() > 10*time.Second {
		t.Fatalf("elapsed %v", e.elapsed())
	}
	// And the monitor does not mistake it for one that just appeared.
	apUps := e.apUps()
	scans := e.cmdCount("wifi list")
	for i := 0; i < 3; i++ {
		e.advance(time.Minute)
		e.m.monitorTick(bg)
	}
	if e.apUps() != apUps || e.cmdCount("wifi list") != scans {
		t.Fatalf("the monitor disturbed the AP: %v", e.w.cmds)
	}
}
