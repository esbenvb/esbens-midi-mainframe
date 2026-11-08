package rtpmidi

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ServiceType is the DNS-SD service type RTP-MIDI endpoints are announced as.
const ServiceType = "_apple-midi._udp"

// Announcer publishes RTP-MIDI endpoints over mDNS / DNS-SD so that peers can
// discover them. id is a stable key (the router port ID); name is what the
// peer shows; port is the control port.
type Announcer interface {
	// Announce publishes (or re-publishes, replacing an earlier announcement
	// with the same id).
	Announce(id, name string, port int) error
	Withdraw(id string)
	// Close withdraws everything this announcer published.
	Close()
}

// NoopAnnouncer announces nothing (peers must be pointed at the address by hand).
type NoopAnnouncer struct{}

func (NoopAnnouncer) Announce(id, name string, port int) error { return nil }
func (NoopAnnouncer) Withdraw(id string)                       {}
func (NoopAnnouncer) Close()                                   {}

// ---- Avahi -----------------------------------------------------------------

// DefaultAvahiDir is where avahi-daemon watches for static service files.
const DefaultAvahiDir = "/etc/avahi/services"

// AvahiFiles announces endpoints by writing `<dir>/emm-rtp-<id>.service`
// files, which avahi-daemon picks up (and drops again when they are removed)
// without a reload. Needs write access to the directory (root on the Pi).
//
// The files are owned by the one emmd that runs: before the first announcement
// every emm-rtp-*.service file already in the directory is removed, so what an
// earlier run left behind (killed, power cut) does not keep announcing
// endpoints that no longer exist.
type AvahiFiles struct {
	Dir string

	mu    sync.Mutex
	files map[string]string // id -> path
	swept bool              // the leftovers of earlier runs are gone
}

// NewAvahiFiles returns an AvahiFiles announcer for dir ("" = DefaultAvahiDir).
func NewAvahiFiles(dir string) *AvahiFiles {
	if dir == "" {
		dir = DefaultAvahiDir
	}
	return &AvahiFiles{Dir: dir, files: map[string]string{}}
}

// avahiMaxName is the DNS-SD instance name limit in bytes.
const avahiMaxName = 63

// cleanServiceName makes name safe as a DNS-SD instance name: no control
// characters, at most 63 bytes (cut on a character boundary), never empty.
func cleanServiceName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > avahiMaxName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = "EMM"
	}
	return name
}

// avahiServiceXML renders the avahi static service definition.
func avahiServiceXML(name string, port int) []byte {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(cleanServiceName(name)))
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" standalone='no'?>\n")
	b.WriteString("<!DOCTYPE service-group SYSTEM \"avahi-service.dtd\">\n")
	b.WriteString("<service-group>\n")
	fmt.Fprintf(&b, "  <name replace-wildcards=\"no\">%s</name>\n", esc.String())
	b.WriteString("  <service>\n")
	fmt.Fprintf(&b, "    <type>%s</type>\n", ServiceType)
	fmt.Fprintf(&b, "    <port>%d</port>\n", port)
	b.WriteString("  </service>\n")
	b.WriteString("</service-group>\n")
	return b.Bytes()
}

// avahiFileName maps an id to a safe file name. Ids that needed changes get a
// short hash so that distinct ids can never collide.
func avahiFileName(id string) string {
	var sb strings.Builder
	changed := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
			changed = true
		}
	}
	s := strings.TrimLeft(sb.String(), ".")
	if s != id {
		changed = true
	}
	if s == "" {
		s = "port"
	}
	if changed {
		h := fnv.New32a()
		h.Write([]byte(id))
		s = fmt.Sprintf("%s-%08x", s, h.Sum32())
	}
	return avahiPrefix + s + ".service"
}

// avahiPrefix starts the name of every service file we write.
const avahiPrefix = "emm-rtp-"

// sweepLocked removes the service files (and half-written .tmp files) of
// earlier runs, once, before this instance writes its own. a.mu must be held.
func (a *AvahiFiles) sweepLocked() {
	if a.swept {
		return
	}
	a.swept = true
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(n, avahiPrefix) ||
			!(strings.HasSuffix(n, ".service") || strings.HasSuffix(n, ".service.tmp")) {
			continue
		}
		_ = os.Remove(filepath.Join(a.Dir, n))
	}
}

func (a *AvahiFiles) Announce(id, name string, port int) error {
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return err
	}
	a.mu.Lock()
	a.sweepLocked()
	a.mu.Unlock()
	path := filepath.Join(a.Dir, avahiFileName(id))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, avahiServiceXML(name, port), 0o644); err != nil {
		return err
	}
	// Avahi only reads *.service, so the .tmp name is never picked up half-written.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	a.mu.Lock()
	a.files[id] = path
	a.mu.Unlock()
	return nil
}

func (a *AvahiFiles) Withdraw(id string) {
	a.mu.Lock()
	path, ok := a.files[id]
	delete(a.files, id)
	a.mu.Unlock()
	if ok {
		_ = os.Remove(path)
	}
}

func (a *AvahiFiles) Close() {
	a.mu.Lock()
	files := a.files
	a.files = map[string]string{}
	a.mu.Unlock()
	for _, p := range files {
		_ = os.Remove(p)
	}
}

// ---- dns-sd (macOS) --------------------------------------------------------

// DNSSD announces endpoints by running the macOS `dns-sd -R` tool, one
// subprocess per endpoint, so that Audio MIDI Setup on the same network can
// discover endpoints of an emmd running on a Mac. A development convenience.
type DNSSD struct {
	// Path of the dns-sd binary; "" looks it up in $PATH.
	Path string

	mu    sync.Mutex
	procs map[string]*exec.Cmd
}

func NewDNSSD() *DNSSD { return &DNSSD{procs: map[string]*exec.Cmd{}} }

// dnssdArgs are the arguments for `dns-sd` to register an endpoint.
func dnssdArgs(name string, port int) []string {
	return []string{"-R", cleanServiceName(name), ServiceType, "local", strconv.Itoa(port)}
}

func (d *DNSSD) Announce(id, name string, port int) error {
	path := d.Path
	if path == "" {
		p, err := exec.LookPath("dns-sd")
		if err != nil {
			return fmt.Errorf("rtpmidi: dns-sd not found: %w", err)
		}
		path = p
	}
	d.Withdraw(id)
	cmd := exec.Command(path, dnssdArgs(name, port)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap after Kill
	d.mu.Lock()
	d.procs[id] = cmd
	d.mu.Unlock()
	return nil
}

func (d *DNSSD) Withdraw(id string) {
	d.mu.Lock()
	cmd := d.procs[id]
	delete(d.procs, id)
	d.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (d *DNSSD) Close() {
	d.mu.Lock()
	procs := d.procs
	d.procs = map[string]*exec.Cmd{}
	d.mu.Unlock()
	for _, c := range procs {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}
