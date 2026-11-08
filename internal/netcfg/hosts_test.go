package netcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After a rename the 127.0.1.1 line of /etc/hosts follows, or sudo prints
// "unable to resolve host" on Raspberry Pi OS. It is best effort and touches no
// other line. Every test here points the Manager at a temp file (Options.HostsFile).

// piHosts is what Raspberry Pi OS ships (the host name is raspberrypi).
const piHosts = "127.0.0.1\tlocalhost\n" +
	"::1\t\tlocalhost ip6-localhost ip6-loopback\n" +
	"ff02::1\t\tip6-allnodes\n" +
	"ff02::2\t\tip6-allrouters\n" +
	"\n" +
	"127.0.1.1\t\traspberrypi\n"

func (e *env) writeHosts(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.hosts, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(e.hosts, 0o644); err != nil { // whatever the umask says
		e.t.Fatal(err)
	}
}

func (e *env) hostsNow() string {
	e.t.Helper()
	b, err := os.ReadFile(e.hosts)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) wantHosts(want string) {
	e.t.Helper()
	if got := e.hostsNow(); got != want {
		e.t.Fatalf("hosts file:\n%q\nwant:\n%q", got, want)
	}
}

// onlyHostsFile fails the test if the hosts file's directory holds anything else
// (a temp file left behind by the atomic write).
func (e *env) onlyHostsFile() {
	e.t.Helper()
	ents, err := os.ReadDir(filepath.Dir(e.hosts))
	if err != nil {
		e.t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != filepath.Base(e.hosts) {
		var names []string
		for _, en := range ents {
			names = append(names, en.Name())
		}
		e.t.Fatalf("files next to the hosts file: %v", names)
	}
}

func TestSetHostnameReplacesTheHostsLine(t *testing.T) {
	e := newEnv(t, withCable())
	e.writeHosts(piHosts)
	e.start()
	e.wantHosts(piHosts) // nothing named the machine: nothing to follow
	before, err := os.Stat(e.hosts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	e.wantHosts(strings.TrimSuffix(piHosts, "raspberrypi\n") + "kitchen\n")
	e.onlyHostsFile()
	// Written to a temp file and renamed over the old one, never rewritten in
	// place: a reader sees the old file or the new one, nothing in between.
	if after, err := os.Stat(e.hosts); err != nil || os.SameFile(before, after) {
		t.Fatalf("the hosts file was rewritten in place (stat error: %v)", err)
	}
	if fi, err := os.Stat(e.hosts); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, %v", fi, err)
	}
	if e.w.hostname != "kitchen" {
		t.Fatalf("host name %q", e.w.hostname)
	}
	// A second rename replaces the line again; it never accumulates.
	if err := e.m.SetHostname("studio"); err != nil {
		t.Fatal(err)
	}
	e.wantHosts(strings.TrimSuffix(piHosts, "raspberrypi\n") + "studio\n")
}

func TestHostsLineFollowsTheRenameAtBoot(t *testing.T) {
	for name, opts := range map[string][]envOpt{
		"saved settings": {withSaved(Saved{Hostname: "kitchen"})},
		"emm.txt":        {withBootFile("hostname = kitchen\n")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, append(opts, withCable())...)
			e.writeHosts(piHosts)
			e.start()
			e.wantHosts(strings.TrimSuffix(piHosts, "raspberrypi\n") + "kitchen\n")
		})
	}
}

func TestHostsLineFollowsTheRenameWithTheHostnameCommand(t *testing.T) {
	e := newEnv(t, withCable())
	e.writeHosts(piHosts)
	e.start()
	e.m.o.Runner = failingRunner{Runner: e.w, fail: func(name string, args []string) bool { return name == "hostnamectl" }}
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	if e.w.hostname != "kitchen" || e.cmdIndex("hostname kitchen") < 0 {
		t.Fatalf("host name %q: %v", e.w.hostname, e.w.cmds)
	}
	e.wantHosts(strings.TrimSuffix(piHosts, "raspberrypi\n") + "kitchen\n")
}

func TestHostsLineIsAppendedWhenThereIsNone(t *testing.T) {
	e := newEnv(t, withCable())
	base := strings.TrimSuffix(piHosts, "127.0.1.1\t\traspberrypi\n")
	e.writeHosts(base)
	e.start()
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	e.wantHosts(base + "127.0.1.1\tkitchen\n")
	e.onlyHostsFile()
}

func TestHostsFileWithoutTrailingNewline(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		e := newEnv(t, withCable())
		e.writeHosts("127.0.0.1\tlocalhost\n::1\tlocalhost")
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatal(err)
		}
		// The last line is not glued to the new one.
		e.wantHosts("127.0.0.1\tlocalhost\n::1\tlocalhost\n127.0.1.1\tkitchen\n")
	})
	t.Run("replace the last line", func(t *testing.T) {
		e := newEnv(t, withCable())
		e.writeHosts("127.0.0.1\tlocalhost\n127.0.1.1\traspberrypi")
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatal(err)
		}
		e.wantHosts("127.0.0.1\tlocalhost\n127.0.1.1\tkitchen")
	})
	t.Run("empty file", func(t *testing.T) {
		e := newEnv(t, withCable())
		e.writeHosts("")
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatal(err)
		}
		e.wantHosts("127.0.1.1\tkitchen\n")
	})
}

func TestHostsOtherLinesAreUntouched(t *testing.T) {
	// Look-alikes that are not the 127.0.1.1 line stay exactly as they are, in
	// the same order, wherever the real one is.
	others := "# Your system has configured 'manage_etc_hosts' as True.\n" +
		"127.0.0.1\tlocalhost raspberrypi\n" +
		"#127.0.1.1\tcommented-out\n" +
		"# 127.0.1.1 also a comment\n" +
		"127.0.1.10\tnot-the-same-address\n" +
		"127.0.1.11 nor-this\n" +
		"192.168.1.9\tnas.local nas   \n" +
		"\n" +
		"::1 localhost ip6-localhost\n"
	e := newEnv(t, withCable())
	e.writeHosts(others + "127.0.1.1\traspberrypi raspberrypi.local\n" + "10.0.0.1\tgw\n")
	e.start()
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	e.wantHosts(others + "127.0.1.1\tkitchen\n" + "10.0.0.1\tgw\n")

	t.Run("no line at all", func(t *testing.T) {
		e := newEnv(t, withCable())
		e.writeHosts(others)
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatal(err)
		}
		e.wantHosts(others + "127.0.1.1\tkitchen\n")
	})
	t.Run("CRLF file", func(t *testing.T) {
		// Somebody's editor saved it with Windows line endings: the other lines
		// keep theirs, and the replaced one its own.
		e := newEnv(t, withCable())
		e.writeHosts("127.0.0.1\tlocalhost\r\n127.0.1.1\traspberrypi\r\n::1\tlocalhost\r\n")
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatal(err)
		}
		e.wantHosts("127.0.0.1\tlocalhost\r\n127.0.1.1\tkitchen\r\n::1\tlocalhost\r\n")
	})
}

func TestHostsFailureNeverFailsTheRename(t *testing.T) {
	check := func(t *testing.T, e *env) {
		t.Helper()
		e.start()
		if err := e.m.SetHostname("kitchen"); err != nil {
			t.Fatalf("the rename failed because of the hosts file: %v", err)
		}
		if e.w.hostname != "kitchen" || e.m.Status().Hostname != "kitchen" || e.savedNow().Hostname != "kitchen" {
			t.Fatalf("host name %q, status %q, saved %q", e.w.hostname, e.m.Status().Hostname, e.savedNow().Hostname)
		}
	}
	t.Run("no hosts file", func(t *testing.T) {
		e := newEnv(t, withCable())
		check(t, e)
		if _, err := os.Stat(e.hosts); !os.IsNotExist(err) {
			t.Fatalf("a hosts file was made up: %v", err)
		}
	})
	t.Run("not a file", func(t *testing.T) {
		e := newEnv(t, withCable())
		if err := os.Mkdir(e.hosts, 0o755); err != nil {
			t.Fatal(err)
		}
		check(t, e)
	})
	t.Run("directory that is gone", func(t *testing.T) {
		e := newEnv(t, withCable())
		e.m.o.HostsFile = filepath.Join(t.TempDir(), "gone", "hosts")
		check(t, e)
	})
	t.Run("unwritable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes through file permissions")
		}
		e := newEnv(t, withCable())
		e.writeHosts(piHosts)
		dir := filepath.Dir(e.hosts)
		if err := os.Chmod(e.hosts, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		check(t, e)
		e.wantHosts(piHosts)
	})
}

func TestHostsFileUntouchedWhenNoHostnameIsApplied(t *testing.T) {
	// A line that any update to this machine's name or to "kitchen" would change.
	const stale = "127.0.0.1\tlocalhost\n127.0.1.1\told-name\n"
	setup := func(t *testing.T, opts ...envOpt) *env {
		e := newEnv(t, append(opts, withCable())...)
		e.writeHosts(stale)
		return e
	}
	t.Run("nothing chooses a name", func(t *testing.T) {
		e := setup(t)
		e.start()
		e.m.monitorTick(bg)
		e.wantHosts(stale)
	})
	t.Run("the machine already has the name", func(t *testing.T) {
		e := setup(t, withSaved(Saved{Hostname: "kitchen"}), withHostname("kitchen"))
		e.start()
		if e.hostnameChanged() {
			t.Fatal("renamed to the name it has")
		}
		e.wantHosts(stale)
	})
	t.Run("the rename fails", func(t *testing.T) {
		e := setup(t, withSaved(Saved{Hostname: "kitchen"}))
		e.m.o.Runner = failingRunner{Runner: e.w, fail: func(name string, args []string) bool {
			return name == "hostnamectl" || name == "hostname" && len(args) > 0
		}}
		e.start()
		if err := e.m.SetHostname("studio"); err != nil {
			t.Fatal(err)
		}
		if e.w.hostname != "raspberrypi" {
			t.Fatalf("host name %q", e.w.hostname)
		}
		e.wantHosts(stale)
	})
	t.Run("emm.txt owns the name", func(t *testing.T) {
		e := setup(t, withBootFile("hostname = from-file\n"))
		e.start() // renames the machine, and the line with it
		e.writeHosts(stale)
		if err := e.m.SetHostname("other"); err != ErrBootManaged {
			t.Fatalf("err %v", err)
		}
		e.wantHosts(stale)
	})
	t.Run("invalid names", func(t *testing.T) {
		e := setup(t, withSaved(Saved{Hostname: "Not_Valid"}))
		e.start()
		if err := e.m.SetHostname("under_score"); err == nil {
			t.Fatal("accepted")
		}
		if e.hostnameChanged() {
			t.Fatal("renamed")
		}
		e.wantHosts(stale)
	})
}

func TestHostsFileIsReplacedThroughASymlink(t *testing.T) {
	e := newEnv(t, withCable())
	target := filepath.Join(t.TempDir(), "hosts.real")
	if err := os.WriteFile(target, []byte(piHosts), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.hosts); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	e.start()
	if err := e.m.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(e.hosts); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(target)
	if want := strings.TrimSuffix(piHosts, "raspberrypi\n") + "kitchen\n"; string(b) != want {
		t.Fatalf("target:\n%q\nwant:\n%q", b, want)
	}
	// The mode the file had is the mode it keeps.
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v", fi, err)
	}
}

func TestHostsFileOptionAndDefault(t *testing.T) {
	m := NewManager(Options{Runner: newFakeWorld(t.TempDir()), NMDir: t.TempDir(), DNSMasqDir: t.TempDir()})
	if m.o.HostsFile != "/etc/hosts" {
		t.Fatalf("default %q: the daemon constructs Options without it", m.o.HostsFile)
	}
	m = NewManager(Options{Runner: newFakeWorld(t.TempDir()), NMDir: t.TempDir(), DNSMasqDir: t.TempDir(), HostsFile: "/somewhere/else"})
	if m.o.HostsFile != "/somewhere/else" {
		t.Fatalf("option %q", m.o.HostsFile)
	}

	// The fake used for UI development must not rename the laptop's own file.
	t.Setenv("TMPDIR", t.TempDir())
	f := NewFake(Options{})
	if f.o.HostsFile == "" || f.o.HostsFile == "/etc/hosts" {
		t.Fatalf("NewFake uses %q", f.o.HostsFile)
	}
	if err := f.SetHostname("kitchen"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.o.HostsFile)
	if err != nil || !strings.Contains(string(b), "127.0.0.1\tlocalhost\n") || !strings.HasSuffix(string(b), "127.0.1.1\tkitchen\n") {
		t.Fatalf("fake hosts file %q (%v)", b, err)
	}
}

func TestHostsWithName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"replace", "127.0.1.1\told\n", "127.0.1.1\tnew\n"},
		{"keeps the spacing", "127.0.1.1\t\told\n", "127.0.1.1\t\tnew\n"},
		{"spaces", "127.0.1.1   old\n", "127.0.1.1   new\n"},
		{"every name goes", "127.0.1.1\told old.local old.lan\n", "127.0.1.1\tnew\n"},
		{"address alone", "127.0.1.1\n", "127.0.1.1\tnew\n"},
		{"address with trailing blanks", "127.0.1.1  \n", "127.0.1.1  new\n"},
		{"comment kept", "127.0.1.1\told # my box\n", "127.0.1.1\tnew # my box\n"},
		{"indented", "  127.0.1.1\told\n", "  127.0.1.1\tnew\n"},
		{"last line, no newline", "127.0.0.1\tlocalhost\n127.0.1.1\told", "127.0.0.1\tlocalhost\n127.0.1.1\tnew"},
		{"only the first line", "127.0.1.1\told\n127.0.1.1\tother\n", "127.0.1.1\tnew\n127.0.1.1\tother\n"},
		{"already right", "127.0.1.1\tnew\n", "127.0.1.1\tnew\n"},
		{"similar address", "127.0.1.10\told\n", "127.0.1.10\told\n127.0.1.1\tnew\n"},
		{"commented out", "#127.0.1.1\told\n", "#127.0.1.1\told\n127.0.1.1\tnew\n"},
		{"empty", "", "127.0.1.1\tnew\n"},
		{"append after a complete line", "127.0.0.1\tlocalhost\n", "127.0.0.1\tlocalhost\n127.0.1.1\tnew\n"},
		{"append after an incomplete line", "127.0.0.1\tlocalhost", "127.0.0.1\tlocalhost\n127.0.1.1\tnew\n"},
		{"CRLF", "127.0.1.1\told\r\n", "127.0.1.1\tnew\r\n"},
	}
	for _, c := range cases {
		if got := hostsWithName(c.in, "new"); got != c.want {
			t.Errorf("%s: hostsWithName(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if got := hostsWithName(c.want, "new"); got != c.want {
			t.Errorf("%s: not idempotent: %q -> %q", c.name, c.want, got)
		}
	}
}
