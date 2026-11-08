package netcfg

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The host name is changed only when emm.txt or the saved UI settings choose
// one. A Pi OS box keeps the name its owner gave it (the installer's "keeping
// your hostname" and --no-hostname), and Status reports the real one.

func withHostname(h string) envOpt { return withWorld(func(w *fakeWorld) { w.hostname = h }) }

// hostnameChanged reports whether anything tried to rename the machine.
func (e *env) hostnameChanged() bool {
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	for _, c := range e.w.cmds {
		if strings.HasPrefix(c, "hostnamectl") || strings.HasPrefix(c, "hostname ") && c != "hostname " ||
			strings.Contains(c, "restart avahi-daemon") {
			return true
		}
	}
	return false
}

func TestHostnameIsLeftAloneWhenNothingChoosesOne(t *testing.T) {
	cases := map[string][]envOpt{
		"cable":            {withCable()},
		"no cable (setup)": nil,
		"saved wifi":       {withSaved(wifiHome)},
		"saved, not hostname": {withSaved(Saved{WifiCountry: "DK", Ethernet: EthernetSettings{Mode: "dhcp"}}),
			withBootFile("ap_ssid = MySetup\n"), withCable()},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			// What `hostname` prints may be a fully qualified, mixed-case name.
			e := newEnv(t, append(opts, withHostname("Studio-Pi.localdomain"))...)
			if st := e.m.Status(); st.Hostname != "studio-pi" {
				t.Fatalf("before the boot sequence: %q", st.Hostname)
			}
			e.start()
			st := e.m.Status()
			if st.Hostname != "studio-pi" {
				t.Fatalf("Status.Hostname %q: the machine's own name, lower-cased first label", st.Hostname)
			}
			if e.hostnameChanged() || e.w.hostname != "Studio-Pi.localdomain" {
				t.Fatalf("the machine was renamed to %q: %v", e.w.hostname, e.w.cmds)
			}
			if e.m.PortalURL() != "http://studio-pi.local/" && !st.SetupMode {
				t.Fatalf("portal %q", e.m.PortalURL())
			}
			for _, b := range st.BootManaged {
				if b == "hostname" {
					t.Fatal("the host name is not managed by emm.txt")
				}
			}
		})
	}
}

func TestHostnameFromTheSavedSettingsIsApplied(t *testing.T) {
	e := newEnv(t, withSaved(Saved{Hostname: "kitchen"}), withCable(), withHostname("raspberrypi"))
	e.start()
	if e.w.hostname != "kitchen" || e.cmdIndex("hostnamectl set-hostname kitchen") < 0 || e.cmdIndex("systemctl restart avahi-daemon") < 0 {
		t.Fatalf("host name %q: %v", e.w.hostname, e.w.cmds)
	}
	if st := e.m.Status(); st.Hostname != "kitchen" || len(st.BootManaged) != 0 {
		t.Fatalf("status %+v", st)
	}
	if e.m.PortalURL() != "http://kitchen.local/" {
		t.Fatalf("portal %q", e.m.PortalURL())
	}
}

func TestHostnameFromTheBootFileIsAppliedAndWins(t *testing.T) {
	e := newEnv(t, withSaved(Saved{Hostname: "from-ui"}), withBootFile("hostname = from-file\n"), withCable(), withHostname("raspberrypi"))
	e.start()
	if e.w.hostname != "from-file" {
		t.Fatalf("host name %q", e.w.hostname)
	}
	if st := e.m.Status(); st.Hostname != "from-file" || strings.Join(st.BootManaged, ",") != "hostname" {
		t.Fatalf("status %+v", st)
	}
	if err := e.m.SetHostname("other"); err == nil {
		t.Fatal("boot-managed protection lost")
	}
}

func TestHostnameAlreadyRightIsNotTouched(t *testing.T) {
	e := newEnv(t, withSaved(Saved{Hostname: "kitchen"}), withCable(), withHostname("kitchen"))
	e.start()
	if e.hostnameChanged() {
		t.Fatalf("renamed to the name it has: %v", e.w.cmds)
	}
}

func TestStatusHostnameIsNeverEmpty(t *testing.T) {
	for name, h := range map[string]string{"none": "(none)", "empty": "", "not a name": "under_score", "leading dot": ".local"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withCable(), withHostname(h))
			if got := e.m.Status().Hostname; got != "emm" {
				t.Fatalf("before the boot sequence: %q", got)
			}
			e.start()
			if got := e.m.Status().Hostname; got != "emm" {
				t.Fatalf("Status.Hostname %q: the web layer builds its Host allow-list from it", got)
			}
			if e.m.PortalURL() != "http://emm.local/" {
				t.Fatalf("portal %q", e.m.PortalURL())
			}
			if e.hostnameChanged() {
				t.Fatal("the machine was renamed because its name could not be read")
			}
		})
	}
	// A system where `hostname` itself cannot run.
	w := newFakeWorld(t.TempDir())
	m := NewManager(Options{
		NMDir: t.TempDir(), DNSMasqDir: t.TempDir(), HostsFile: filepath.Join(t.TempDir(), "hosts"),
		Runner: failingRunner{Runner: w, fail: func(name string, args []string) bool { return name == "hostname" }},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if got := m.Status().Hostname; got != "emm" {
		t.Fatalf("hostname command failing: %q", got)
	}
}

func TestStatusHostnameFollowsTheSystemWhileNothingChoosesIt(t *testing.T) {
	e := newEnv(t, withCable(), withHostname("raspberrypi"))
	e.start()
	changes := len(e.history)
	e.w.mu.Lock()
	e.w.hostname = "Renamed"
	e.w.mu.Unlock()
	e.advance(time.Minute)
	e.m.monitorTick(bg)
	if got := e.m.Status().Hostname; got != "renamed" {
		t.Fatalf("Status.Hostname %q", got)
	}
	if len(e.history) <= changes {
		t.Fatal("OnStatusChange was not called for the new name")
	}

	// A name that emm.txt or the UI chose is not overridden by whatever the system says.
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	e.w.mu.Lock()
	e.w.hostname = "somebody-else"
	e.w.mu.Unlock()
	e.advance(time.Minute)
	e.m.monitorTick(bg)
	if got := e.m.Status().Hostname; got != "kitchen" {
		t.Fatalf("Status.Hostname %q", got)
	}
}

func TestDefaultHostnameIsKeptForCallersButNotApplied(t *testing.T) {
	if DefaultHostname != "emm" {
		t.Fatalf("DefaultHostname %q", DefaultHostname)
	}
	e := newEnv(t, withCable(), withHostname("raspberrypi"))
	e.start()
	if e.w.hostname == DefaultHostname {
		t.Fatal("DefaultHostname was applied")
	}
}

func TestCleanHostname(t *testing.T) {
	cases := map[string]string{
		"studio-pi\n":           "studio-pi",
		"Studio-Pi.localdomain": "studio-pi",
		"  emm  ":               "emm",
		"raspberrypi.local.":    "raspberrypi",
		"":                      "",
		"(none)":                "",
		"under_score":           "",
		".local":                "",
		"-lead":                 "",
		"trail-":                "",
	}
	for in, want := range cases {
		if got := cleanHostname(in); got != want {
			t.Errorf("cleanHostname(%q) = %q, want %q", in, got, want)
		}
	}
}
