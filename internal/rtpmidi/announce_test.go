package rtpmidi

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

type avahiDoc struct {
	XMLName xml.Name `xml:"service-group"`
	Name    struct {
		Wildcards string `xml:"replace-wildcards,attr"`
		Text      string `xml:",chardata"`
	} `xml:"name"`
	Services []struct {
		Type string `xml:"type"`
		Port int    `xml:"port"`
	} `xml:"service"`
}

func parseAvahi(t *testing.T, b []byte) avahiDoc {
	t.Helper()
	var d avahiDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, b)
	}
	return d
}

func TestAvahiServiceXMLExact(t *testing.T) {
	got := string(avahiServiceXML("DX7", 5004))
	want := `<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="no">DX7</name>
  <service>
    <type>_apple-midi._udp</type>
    <port>5004</port>
  </service>
</service-group>
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAvahiServiceXMLEscapingAndCleaning(t *testing.T) {
	d := parseAvahi(t, avahiServiceXML(`A&B <c> "d" 'e' 100%`, 5006))
	if d.Name.Text != `A&B <c> "d" 'e' 100%` || d.Name.Wildcards != "no" {
		t.Fatalf("name round trip: %q", d.Name.Text)
	}
	if len(d.Services) != 1 || d.Services[0].Type != "_apple-midi._udp" || d.Services[0].Port != 5006 {
		t.Fatalf("service: %+v", d.Services)
	}
	// Control characters never reach the file.
	d = parseAvahi(t, avahiServiceXML("a\x00b\x1bc\nd\x7f", 1))
	if d.Name.Text != "abcd" {
		t.Fatalf("control chars: %q", d.Name.Text)
	}
	// Markup in a name cannot break out of the element.
	d = parseAvahi(t, avahiServiceXML("</name><evil/>", 1))
	if d.Name.Text != "</name><evil/>" {
		t.Fatalf("injection: %q", d.Name.Text)
	}
	if got := cleanServiceName(""); got != "EMM" {
		t.Fatalf("empty name -> %q", got)
	}
	if got := cleanServiceName("  \t "); got != "EMM" {
		t.Fatalf("blank name -> %q", got)
	}
	long := strings.Repeat("ø", 50) // 100 bytes
	got := cleanServiceName(long)
	if len(got) > 63 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Fatalf("truncation: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if got := cleanServiceName(strings.Repeat("x", 63)); len(got) != 63 {
		t.Fatal("63-byte name must be kept whole")
	}
}

func TestAvahiFileNames(t *testing.T) {
	a := avahiFileName("rtp:usb:1234:abcd:1.2:0")
	if !strings.HasPrefix(a, "emm-rtp-rtp_usb_1234_abcd_1.2_0-") || !strings.HasSuffix(a, ".service") {
		t.Fatalf("name: %s", a)
	}
	if strings.ContainsAny(a, ":/\\ ") {
		t.Fatalf("unsafe file name %q", a)
	}
	// Ids that sanitise to the same text must not collide.
	if avahiFileName("a:b") == avahiFileName("a_b") || avahiFileName("a:b") == avahiFileName("a/b") {
		t.Fatal("collision")
	}
	// Already-safe ids stay readable.
	if got := avahiFileName("net-1"); got != "emm-rtp-net-1.service" {
		t.Fatalf("safe id: %s", got)
	}
	// Path traversal is impossible.
	for _, id := range []string{"../../etc/passwd", "..", "/abs", "a/../b", "", "."} {
		n := avahiFileName(id)
		if strings.Contains(n, "/") || n != filepath.Base(n) || !strings.HasPrefix(n, "emm-rtp-") {
			t.Fatalf("%q -> %q", id, n)
		}
	}
	if avahiFileName("") == avahiFileName(".") {
		t.Fatal("empty and dot collide")
	}
}

func TestAvahiFilesLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services") // does not exist yet
	a := NewAvahiFiles(dir)
	if err := a.Announce("rtp:one", "One", 5004); err != nil {
		t.Fatal(err)
	}
	if err := a.Announce("rtp:two", "Two", 5006); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".service") {
			t.Fatalf("stray file %s (Avahi would ignore it, but leftovers are litter)", f)
		}
	}
	path := filepath.Join(dir, avahiFileName("rtp:one"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := parseAvahi(t, b); d.Name.Text != "One" || d.Services[0].Port != 5004 {
		t.Fatalf("content: %+v", d)
	}
	// Re-announce replaces.
	if err := a.Announce("rtp:one", "Uno", 5008); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if d := parseAvahi(t, b); d.Name.Text != "Uno" || d.Services[0].Port != 5008 {
		t.Fatalf("replaced content: %+v", d)
	}
	a.Withdraw("rtp:one")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file not removed by Withdraw")
	}
	a.Withdraw("rtp:one") // idempotent
	a.Withdraw("never-announced")
	a.Close()
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Fatalf("Close left %v", files)
	}
}

func TestAvahiFilesUnwritableDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewAvahiFiles(filepath.Join(f, "sub")).Announce("x", "X", 1); err == nil {
		t.Fatal("expected an error when the directory cannot be created")
	}
}

func TestDNSSD(t *testing.T) {
	args := dnssdArgs("My Synth", 5004)
	want := []string{"-R", "My Synth", "_apple-midi._udp", "local", "5004"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("args %v", args)
	}
	// A stand-in for dns-sd that just stays alive.
	script := filepath.Join(t.TempDir(), "fake-dns-sd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewDNSSD()
	d.Path = script
	if err := d.Announce("a", "A", 5004); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	cmd := d.procs["a"]
	d.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("process not started")
	}
	pid := cmd.Process.Pid
	// Re-announce replaces the process.
	if err := d.Announce("a", "A2", 5004); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old process killed", func() bool { return !processAlive(pid) })
	d.Close()
	d.mu.Lock()
	n := len(d.procs)
	d.mu.Unlock()
	if n != 0 {
		t.Fatal("Close left processes")
	}
	bad := NewDNSSD()
	bad.Path = filepath.Join(t.TempDir(), "does-not-exist")
	if err := bad.Announce("x", "X", 1); err == nil {
		t.Fatal("missing binary must be an error")
	}
	NoopAnnouncer{}.Withdraw("x")
	NoopAnnouncer{}.Close()
	if err := (NoopAnnouncer{}).Announce("x", "X", 1); err != nil {
		t.Fatal(err)
	}
}
