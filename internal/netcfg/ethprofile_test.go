package netcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// EMM's Ethernet profile emm-eth has autoconnect priority 100, so on a box where
// somebody made a profile for the cable (a static address from nmtui, the
// "Wired connection 1" of Raspberry Pi OS) it must not be written unless
// Ethernet was explicitly configured, or it would win at the next boot. The
// appliance image has no such profile, and there the default one is written.

// withForeignEthernet adds an Ethernet profile somebody else made.
func withForeignEthernet(id string, autoconnect bool) envOpt {
	return withWorld(func(w *fakeWorld) {
		w.foreign = append(w.foreign, fakeProfile{ID: id, Type: "ethernet", Autoconnect: autoconnect})
	})
}

// ethConn is the connection active on the Ethernet device ("" if none).
func (e *env) ethConn() string {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	_, conn := e.w.ethStateLocked()
	return conn
}

// stashProfile puts a profile file into the NetworkManager directory before the
// boot sequence, as an earlier run on a box whose /etc persists would have.
func (e *env) stashProfile(id, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.nm, id+".nmconnection"), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func defaultEthernetProfile(t *testing.T) string {
	t.Helper()
	c, err := renderEthernet(EthernetSettings{Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestForeignEthernetProfileIsNotHijacked(t *testing.T) {
	cases := map[string]struct {
		id          string
		autoconnect bool
	}{
		"Wired connection 1 (autoconnect)":    {"Wired connection 1", true},
		"static profile from nmtui":           {"office-lan", true},
		"profile that does not autoconnect":   {"manual-lan", false},
		"name with a colon (terse escaping)":  {"lan:main", true},
		"emm-eth in the middle of a name":     {"my emm-eth", true},
		"emm in the name, but not its prefix": {"not-emm-eth", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withForeignEthernet(c.id, c.autoconnect), withCable())
			e.start()
			if prof, ok := e.profile(ethConnID); ok {
				t.Fatalf("emm-eth was written over somebody else's profile:\n%s", prof)
			}
			if c.autoconnect {
				// The user's own profile serves the cable, as it did before EMM.
				if got := e.ethConn(); got != c.id {
					t.Fatalf("the cable is on %q, want %q", got, c.id)
				}
				if st := e.m.Status(); st.Mode != "ethernet" || st.SetupMode || st.IP == "" {
					t.Fatalf("status %+v", st)
				}
			}
			e.foreignUntouched(c.id)
		})
	}
}

func TestForeignWifiProfileDoesNotKeepTheDefaultEthernetProfileAway(t *testing.T) {
	// Only Ethernet profiles count: Imager's WiFi profile says nothing about the cable.
	e := newEnv(t, withForeignWifi("preconfigured", "HomeNet-5G", true), withCable())
	e.start()
	if prof, ok := e.profile(ethConnID); !ok || prof != defaultEthernetProfile(t) {
		t.Fatalf("emm-eth: %v\n%s", ok, prof)
	}
}

func TestOwnEarlierEthernetProfileIsNotSomebodyElses(t *testing.T) {
	// On a box whose /etc persists, last boot's emm-eth is in NetworkManager's
	// list. It must not count as a reason to drop itself.
	e := newEnv(t, withCable())
	e.stashProfile(ethConnID, defaultEthernetProfile(t))
	e.start()
	if prof, ok := e.profile(ethConnID); !ok || prof != defaultEthernetProfile(t) {
		t.Fatalf("emm-eth (present: %v):\n%s", ok, prof)
	}
	if got := e.ethConn(); got != ethConnID {
		t.Fatalf("the cable is on %q", got)
	}
}

func TestNoForeignEthernetProfileWritesTheDefaultDHCPProfile(t *testing.T) {
	e := newEnv(t, withCable())
	e.start()
	prof, ok := e.profile(ethConnID)
	if !ok {
		t.Fatal("emm-eth not written: a cable would not work on the appliance image")
	}
	if prof != defaultEthernetProfile(t) || !strings.Contains(prof, "autoconnect-priority=100") || !strings.Contains(prof, "[ipv4]\nmethod=auto") {
		t.Fatalf("emm-eth:\n%s", prof)
	}
	if e.cmdIndex("connection show") < 0 {
		t.Fatalf("NetworkManager was never asked what profiles it has: %v", e.w.cmds)
	}
	if got := e.ethConn(); got != ethConnID {
		t.Fatalf("the cable is on %q", got)
	}
	if st := e.m.Status(); st.Mode != "ethernet" || st.Ethernet.Mode != "auto" {
		t.Fatalf("status %+v", st)
	}
}

func TestExplicitEthernetIsWrittenWhateverElseIsThere(t *testing.T) {
	cases := []struct {
		name string
		opts []envOpt
		want string // substring of emm-eth
		off  bool   // the device is disconnected, so the cable is on nothing
	}{
		{"saved static", []envOpt{withSaved(Saved{Ethernet: EthernetSettings{Mode: "static", Address: "192.168.1.60/24", Gateway: "192.168.1.1"}})},
			"address1=192.168.1.60/24,192.168.1.1\n", false},
		{"saved dhcp", []envOpt{withSaved(Saved{Ethernet: EthernetSettings{Mode: "dhcp"}})}, "may-fail=false", false},
		{"saved off", []envOpt{withSaved(Saved{Ethernet: EthernetSettings{Mode: "off"}})}, "autoconnect=false", true},
		{"emm.txt static", []envOpt{withBootFile("ethernet = static\neth_address = 10.1.2.3/24\n")}, "address1=10.1.2.3/24\n", false},
		{"emm.txt auto (a choice, unlike the UI's default)", []envOpt{withBootFile("ethernet = auto\n")}, "autoconnect-priority=100", false},
		{"emm.txt dhcp beats saved static", []envOpt{withBootFile("ethernet = dhcp\n"),
			withSaved(Saved{Ethernet: EthernetSettings{Mode: "static", Address: "192.168.1.60/24"}})}, "may-fail=false", false},
		{"emm.txt static without an address falls back to DHCP", []envOpt{withBootFile("ethernet = static\n")}, "[ipv4]\nmethod=auto\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := append([]envOpt{withForeignEthernet("Wired connection 1", true), withCable()}, c.opts...)
			e := newEnv(t, opts...)
			e.start()
			prof, ok := e.profile(ethConnID)
			if !ok || !strings.Contains(prof, c.want) {
				t.Fatalf("emm-eth (written: %v) lacks %q:\n%s", ok, c.want, prof)
			}
			if c.off {
				return
			}
			// What the user configured outranks the profile that was there.
			if got := e.ethConn(); got != ethConnID {
				t.Fatalf("the cable is on %q", got)
			}
			e.foreignUntouched("Wired connection 1")
		})
	}
}

func TestEthernetOffStillDisconnectsTheDevice(t *testing.T) {
	for name, opts := range map[string][]envOpt{
		"with a profile of the user's own": {withForeignEthernet("Wired connection 1", true)},
		"without":                          nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, append(opts, withSaved(Saved{Ethernet: EthernetSettings{Mode: "off"}}), withCable())...)
			e.start()
			prof, ok := e.profile(ethConnID)
			if !ok || !strings.Contains(prof, "autoconnect=false") || !strings.Contains(prof, "[ipv4]\nmethod=disabled") {
				t.Fatalf("off profile (written: %v):\n%s", ok, prof)
			}
			if e.cmdIndex("device disconnect eth0") < 0 {
				t.Fatalf("the device was not disconnected: %v", e.w.cmds)
			}
			st := e.m.Status()
			if st.Ethernet.Mode != "off" || st.Ethernet.IP != "" || st.Mode == "ethernet" || e.ethConn() != "" {
				t.Fatalf("status %+v, cable on %q", st, e.ethConn())
			}
		})
	}
}

func TestSavedPlainAutoIsNotAnEthernetChoice(t *testing.T) {
	for name, s := range map[string]Saved{
		"nothing saved":     {},
		"saved auto":        {Ethernet: EthernetSettings{Mode: "auto"}},
		"auto in odd case":  {Ethernet: EthernetSettings{Mode: " Auto "}},
		"unrelated setting": {WifiCountry: "DK"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withSaved(s), withForeignEthernet("Wired connection 1", true), withCable())
			e.start()
			if _, ok := e.profile(ethConnID); ok {
				t.Fatal("emm-eth was written for the default settings over somebody else's profile")
			}
		})
	}
}

func TestStaleDefaultEthernetProfileIsRemovedWhenSomebodyElseHasTheCable(t *testing.T) {
	// An earlier start (before the user made a profile of their own) wrote the
	// default emm-eth, and on Pi OS it is still there.
	e := newEnv(t, withForeignEthernet("Wired connection 1", true), withCable())
	e.stashProfile(ethConnID, defaultEthernetProfile(t))
	e.start()
	if prof, ok := e.profile(ethConnID); ok {
		t.Fatalf("the stale default emm-eth stayed:\n%s", prof)
	}
	if e.cmdIndex("connection reload") < 0 {
		t.Fatal("NetworkManager was not told to reload")
	}
	if got := e.ethConn(); got != "Wired connection 1" {
		t.Fatalf("the cable is on %q", got)
	}
	e.foreignUntouched("Wired connection 1")
}

func TestEthernetProfileWithSettingsIsNeverRemoved(t *testing.T) {
	for _, s := range []EthernetSettings{
		{Mode: "static", Address: "192.168.1.60/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}},
		{Mode: "dhcp"},
		{Mode: "off"},
	} {
		t.Run(s.Mode, func(t *testing.T) {
			// Written for an earlier configuration that is gone now (nothing saved,
			// nothing in emm.txt) while a profile of somebody else's exists.
			old, err := renderEthernet(s)
			if err != nil {
				t.Fatal(err)
			}
			e := newEnv(t, withForeignEthernet("Wired connection 1", true), withCable())
			e.stashProfile(ethConnID, old)
			e.start()
			if prof, ok := e.profile(ethConnID); !ok || prof != old {
				t.Fatalf("emm-eth with %s settings was removed or changed (present: %v):\n%s", s.Mode, ok, prof)
			}
		})
	}
}

func TestEthernetProfileIsLeftAsItIsWhenProfilesCannotBeListed(t *testing.T) {
	list := func(name string, args []string) bool {
		return name == "nmcli" && strings.Contains(strings.Join(args, " "), "NAME,TYPE")
	}

	t.Run("stale default stays", func(t *testing.T) {
		e := newEnv(t, withForeignEthernet("Wired connection 1", true), withCable())
		e.m.o.Runner = failingRunner{Runner: e.w, fail: list}
		e.stashProfile(ethConnID, defaultEthernetProfile(t))
		e.start()
		if prof, ok := e.profile(ethConnID); !ok || prof != defaultEthernetProfile(t) {
			t.Fatalf("emm-eth changed although nothing is known about other profiles (present: %v)", ok)
		}
	})
	t.Run("nothing is written", func(t *testing.T) {
		// Not knowing is not a reason to risk outranking somebody's profile.
		e := newEnv(t, withForeignEthernet("Wired connection 1", true), withCable())
		e.m.o.Runner = failingRunner{Runner: e.w, fail: list}
		e.start()
		if _, ok := e.profile(ethConnID); ok {
			t.Fatal("emm-eth written without knowing what else is there")
		}
	})
	t.Run("explicit settings do not need the list", func(t *testing.T) {
		e := newEnv(t, withSaved(Saved{Ethernet: EthernetSettings{Mode: "dhcp"}}), withCable())
		e.m.o.Runner = failingRunner{Runner: e.w, fail: list}
		e.start()
		if prof, ok := e.profile(ethConnID); !ok || !strings.Contains(prof, "may-fail=false") {
			t.Fatalf("emm-eth (written: %v):\n%s", ok, prof)
		}
	})
}

func TestSetEthernetFromTheUIStillWritesTheProfile(t *testing.T) {
	// The UI's Ethernet form is an explicit choice made at run time.
	e := newEnv(t, withForeignEthernet("Wired connection 1", true), withCable())
	e.start()
	if _, ok := e.profile(ethConnID); ok {
		t.Fatal("emm-eth written at boot")
	}
	if err := e.m.SetEthernet(EthernetSettings{Mode: "static", Address: "192.168.1.60/24", Gateway: "192.168.1.1"}); err != nil {
		t.Fatal(err)
	}
	if prof, ok := e.profile(ethConnID); !ok || !strings.Contains(prof, "address1=192.168.1.60/24,192.168.1.1\n") {
		t.Fatalf("emm-eth (written: %v):\n%s", ok, prof)
	}
	// ... and it is still the user's choice at the next boot.
	e2 := newEnv(t, withSaved(e.savedNow()), withForeignEthernet("Wired connection 1", true), withCable())
	e2.start()
	if prof, ok := e2.profile(ethConnID); !ok || !strings.Contains(prof, "method=manual") {
		t.Fatalf("after reboot (written: %v):\n%s", ok, prof)
	}
}

func TestMergeConfigEthExplicit(t *testing.T) {
	cases := []struct {
		name string
		b    BootConfig
		s    Saved
		want bool
	}{
		{"nothing", BootConfig{}, Saved{}, false},
		{"saved auto", BootConfig{}, Saved{Ethernet: EthernetSettings{Mode: "auto"}}, false},
		{"saved dhcp", BootConfig{}, Saved{Ethernet: EthernetSettings{Mode: "dhcp"}}, true},
		{"saved static", BootConfig{}, Saved{Ethernet: EthernetSettings{Mode: "static", Address: "10.0.0.2/24"}}, true},
		{"saved off", BootConfig{}, Saved{Ethernet: EthernetSettings{Mode: "off"}}, true},
		{"emm.txt auto", BootConfig{Ethernet: "auto"}, Saved{}, true},
		{"emm.txt static", BootConfig{Ethernet: "static", EthAddress: "10.0.0.2/24"}, Saved{}, true},
		{"emm.txt auto over saved static", BootConfig{Ethernet: "auto"}, Saved{Ethernet: EthernetSettings{Mode: "static"}}, true},
		{"other emm.txt keys", BootConfig{Hostname: "x", WifiSSID: "y"}, Saved{}, false},
	}
	for _, c := range cases {
		if got := mergeConfig(c.b, c.s).EthExplicit; got != c.want {
			t.Errorf("%s: EthExplicit = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestForeignEthernetProfiles(t *testing.T) {
	out := `Wired connection 1:ethernet:yes
old-style:802-3-ethernet:no
lan\:main:ethernet:yes
preconfigured:802-11-wireless:yes
home:wifi:yes
emm-eth:ethernet:yes
emm-wifi:wifi:yes
br0:bridge:yes
lo:loopback:no
`
	want := []string{"Wired connection 1", "old-style", "lan:main"}
	if got := foreignEthernetProfiles(parseProfiles(out)); !reflect.DeepEqual(got, want) {
		t.Errorf("foreignEthernetProfiles = %q, want %q", got, want)
	}
	if foreignEthernetProfiles(nil) != nil {
		t.Error("nothing in, nothing out")
	}
}
