// Command emmd is Esben's MIDI Mainframe: a USB MIDI host, patchbay, RTP-MIDI
// server and Bluetooth MIDI peripheral with a web UI. See docs/ARCHITECTURE.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"emm/internal/app"
	"emm/internal/blemidi"
	"emm/internal/config"
	"emm/internal/device"
	"emm/internal/netcfg"
	"emm/internal/router"
	"emm/internal/rtpmidi"
	"emm/internal/sshaccess"
	"emm/internal/web"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

type options struct {
	listen       string
	bootDir      string
	statePath    string
	backend      string
	simHotplug   bool
	allowPower   bool
	avahiDir     string
	netcfgMode   string
	uiDir        string
	logLevel     string
	showVer      bool
	listDev      bool
	noAnnounce   bool
	allowedHosts string
}

func defaultPaths() (boot, state string) {
	if runtime.GOOS == "linux" {
		return "/boot/firmware", "/var/lib/emm/state.json"
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return "", filepath.Join(dir, "emm", "state.json")
}

func parseFlags(args []string) (options, error) {
	var o options
	boot, state := defaultPaths()
	autoBackend := "auto"
	fs := flag.NewFlagSet("emmd", flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", ":80", "HTTP listen address")
	fs.StringVar(&o.bootDir, "boot-dir", boot, "directory holding emm.txt (the FAT32 boot partition); empty disables it")
	fs.StringVar(&o.statePath, "state", state, "state file (connections, names, settings)")
	fs.StringVar(&o.backend, "backend", autoBackend, "MIDI backends, comma separated: auto (rawmidi on Linux, coremidi on macOS), rawmidi, coremidi, sim, none")
	fs.BoolVar(&o.simHotplug, "sim-hotplug", false, "simulated synth unplugs/replugs itself every ~20 s")
	fs.BoolVar(&o.allowPower, "allow-power", false, "enable the reboot/shutdown API")
	fs.StringVar(&o.avahiDir, "avahi-dir", "/etc/avahi/services", "where RTP-MIDI mDNS service files are written (Linux)")
	fs.StringVar(&o.netcfgMode, "netcfg", "auto", "network management: auto (NetworkManager if present), off, fake (simulated, for UI development)")
	fs.StringVar(&o.uiDir, "ui-dir", "", "serve the web UI from this directory instead of the embedded copy (development)")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&o.allowedHosts, "allowed-hosts", "", "extra Host names the web UI accepts, comma separated (exact names; a leading dot allows a whole domain, e.g. .mydomain.example); added to allowed_hosts from emm.txt")
	fs.BoolVar(&o.noAnnounce, "no-announce", false, "do not publish RTP-MIDI endpoints over mDNS")
	fs.BoolVar(&o.showVer, "version", false, "print the version and exit")
	fs.BoolVar(&o.listDev, "list-devices", false, "print the MIDI devices that were found, as JSON, and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	return o, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if o.showVer {
		fmt.Println("emmd", version)
		return
	}
	log := newLogger(o.logLevel)
	slog.SetDefault(log)
	if o.listDev {
		if err := listDevices(); err != nil {
			fmt.Fprintln(os.Stderr, "emmd:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func listDevices() error {
	var out any
	var err error
	switch runtime.GOOS {
	case "linux":
		out, err = device.ListRawMIDI(device.RawMIDIOptions{})
	case "darwin":
		out, err = device.ListCoreMIDI()
	default:
		err = errors.New("no MIDI backend for " + runtime.GOOS)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func pickBackends(spec string, o options, log *slog.Logger) ([]router.Backend, error) {
	var out []router.Backend
	seen := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "auto" {
			switch runtime.GOOS {
			case "linux":
				name = "rawmidi"
			case "darwin":
				name = "coremidi"
			default:
				name = "none"
			}
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case "rawmidi":
			out = append(out, device.NewRawMIDI(device.RawMIDIOptions{Logger: log}))
		case "coremidi":
			out = append(out, device.NewCoreMIDI(device.CoreMIDIOptions{Logger: log}))
		case "sim":
			out = append(out, device.NewSim(device.SimOptions{Hotplug: o.simHotplug, Logger: log}))
		case "none", "":
		default:
			return nil, fmt.Errorf("unknown backend %q (want rawmidi, coremidi, sim or none)", name)
		}
	}
	return out, nil
}

func announcer(o options, log *slog.Logger) rtpmidi.Announcer {
	if o.noAnnounce {
		return rtpmidi.NoopAnnouncer{}
	}
	return pickAnnouncer(o.avahiDir, log)
}

func pickAnnouncer(avahiDir string, log *slog.Logger) rtpmidi.Announcer {
	switch runtime.GOOS {
	case "linux":
		if st, err := os.Stat(avahiDir); err == nil && st.IsDir() {
			return rtpmidi.NewAvahiFiles(avahiDir)
		}
		log.Warn("Avahi service directory not found; network MIDI endpoints will not be announced via mDNS", "dir", avahiDir)
	case "darwin":
		return rtpmidi.NewDNSSD()
	}
	return rtpmidi.NoopAnnouncer{}
}

func run(ctx context.Context, o options, log *slog.Logger) error {
	backends, err := pickBackends(o.backend, o, log)
	if err != nil {
		return err
	}

	// ---- configuration ----------------------------------------------------
	var boot netcfg.BootConfig
	if o.bootDir != "" {
		if boot, err = netcfg.LoadBootConfig(o.bootDir); err != nil {
			log.Warn("could not read emm.txt", "dir", o.bootDir, "err", err)
		}
		for _, w := range boot.Warnings {
			log.Warn("emm.txt", "problem", w)
		}
	}
	store, err := config.Open(o.statePath, log)
	if err != nil {
		return err
	}
	if !store.CheckWritable() {
		log.Warn("state directory is not writable: settings will not survive a restart", "path", o.statePath)
	}
	saved := store.Snapshot()

	rtpOn := func() bool {
		if boot.RTP != nil {
			return *boot.RTP
		}
		on := true
		store.View(func(st *config.State) { on = st.RTPOn() })
		return on
	}
	bleOn := func() bool {
		if boot.BLE != nil {
			return *boot.BLE
		}
		on := true
		store.View(func(st *config.State) { on = st.BLEOn() })
		return on
	}
	bleName := func() string {
		if boot.BLEName != "" {
			return boot.BLEName
		}
		n := ""
		store.View(func(st *config.State) { n = st.BLE.Name })
		if n == "" {
			n = "EMM"
		}
		return n
	}

	// ---- router -----------------------------------------------------------
	rt := router.New(router.Options{
		Logger: log,
		// The router hands these snapshots over in the order the changes were made,
		// so overwriting the stored list wholesale always ends on the newest one.
		OnConnectionsChanged: func(specs []router.ConnSpec) {
			store.Update(func(st *config.State) { st.Connections = specs })
		},
	})
	rt.Load(saved.Connections)

	var srv *web.Server // set below; callbacks may fire before, so they go through notify*
	notifyTopology := func() {
		if srv != nil {
			srv.NotifyTopology()
		}
	}
	notifyNetwork := func() {
		if srv != nil {
			srv.NotifyNetwork()
		}
	}

	// ---- network MIDI (RTP-MIDI) ------------------------------------------
	rtpMgr := rtpmidi.NewManager(rtpmidi.ManagerOptions{
		Host: rt, Announcer: announcer(o, log), Logger: log,
		OnChange: notifyTopology,
	})
	defer rtpMgr.Close()

	rec := &app.Reconciler{
		Router: rt, Store: store, Log: log, RTPEnabled: rtpOn, OnChange: notifyTopology,
		RTP: rtpSyncer{rtpMgr},
	}

	// ---- bluetooth MIDI -----------------------------------------------------
	ble := newBLEController(rt, log, notifyTopology)
	defer ble.stop()

	// ---- network management -------------------------------------------------
	var netMgr *netcfg.Manager
	netOpts := netcfg.Options{
		BootDir: o.bootDir, Logger: log,
		Load: func() netcfg.Saved {
			var s netcfg.Saved
			store.View(func(st *config.State) {
				if len(st.Network) > 0 {
					_ = json.Unmarshal(st.Network, &s)
				}
			})
			return s
		},
		Save:           saveNetwork(store),
		OnStatusChange: notifyNetwork,
	}
	switch strings.ToLower(o.netcfgMode) {
	case "fake":
		netMgr = netcfg.NewFake(netOpts)
	case "off":
	default: // auto
		if runtime.GOOS == "linux" {
			if _, err := exec.LookPath("nmcli"); err == nil {
				netMgr = netcfg.NewManager(netOpts)
			} else {
				log.Warn("nmcli not found: network management is disabled")
			}
		}
	}

	// ---- settings changes from the UI ---------------------------------------
	apply := func() {
		ble.apply(bleOn(), bleName())
		rec.Trigger()
	}

	// ---- web ------------------------------------------------------------------
	deps := web.Deps{
		Router: rt, Store: store, Logger: log,
		Version: version, Backend: describeBackends(backends), AllowPower: o.allowPower,
		UIDir: o.uiDir, BLEDefault: "EMM", AllowedHosts: mergeHosts(splitList(o.allowedHosts), boot.AllowedHosts),
		BootRTP: boot.RTP, BootBLE: boot.BLE, BootBLEName: boot.BLEName, BootWebPassword: boot.WebPassword,
		Hostname:      hostnameFunc(netMgr),
		RTP:           rtpEndpoints(rtpMgr, store),
		BLE:           ble.status,
		Reconcile:     rec.Trigger,
		ApplySettings: apply,
	}
	// SSH access exists on the appliance image only (it ships Dropbear and the unit).
	if sshaccess.Available() {
		ctl := &sshaccess.Controller{Dir: filepath.Join(filepath.Dir(o.statePath), "ssh"), Log: log}
		deps.SSH = sshAdapter{ctl}
		if saved.SSH.Enabled {
			go func() { // the settings were saved with SSH on: bring it back after a reboot
				if err := ctl.Apply(sshaccess.Config{Enabled: true, PasswordHash: saved.SSH.PasswordHash, AuthorizedKeys: saved.SSH.AuthorizedKeys}); err != nil {
					log.Warn("could not switch SSH access back on", "err", err)
				}
			}()
		}
	}
	if netMgr != nil {
		deps.Net = &netAdapter{m: netMgr}
		deps.Wrap = netcfg.CaptiveMiddleware(netMgr)
	}
	srv = web.New(deps)
	srv.Start(ctx)

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w (use --listen :8080 to run without root)", o.listen, err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	// ---- go ---------------------------------------------------------------------
	var wg sync.WaitGroup
	start := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
			log.Debug("stopped", "component", name)
		}()
	}

	for _, b := range backends {
		b := b
		start("backend "+b.Name(), func() {
			if err := b.Run(ctx, rt); err != nil && ctx.Err() == nil {
				log.Error("MIDI backend stopped", "backend", b.Name(), "err", err)
			}
		})
	}
	recDone := make(chan struct{})
	start("reconciler", func() { rec.Run(recDone) })
	if netMgr != nil {
		start("network", func() {
			if err := netMgr.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("network manager stopped", "err", err)
			}
		})
	}
	ble.apply(bleOn(), bleName())

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	log.Info("Esben's MIDI Mainframe is up", "version", version, "listen", ln.Addr().String(),
		"backend", deps.Backend, "state", o.statePath)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		_ = httpSrv.Close() // long-lived event streams
	}
	close(recDone)
	ble.stop()
	wg.Wait()
	if err := store.Flush(); err != nil {
		log.Warn("final state save failed", "err", err)
	}
	return nil
}

// sshAdapter lets the web package drive the SSH controller without importing its types.
type sshAdapter struct{ c *sshaccess.Controller }

func (a sshAdapter) Apply(enabled bool, passwordHash string, keys []string) error {
	return a.c.Apply(sshaccess.Config{Enabled: enabled, PasswordHash: passwordHash, AuthorizedKeys: keys})
}

func (a sshAdapter) Status() (bool, string) { return a.c.Status() }

// saveNetwork returns the hook the network manager calls to persist its
// settings. The credentials must be on disk before the radio switches, so it
// flushes at once. If that fails, the manager treats the change as not made and
// rolls back, so the store must not keep the refused settings either: a later
// (retried) flush would otherwise write exactly what the manager discarded.
func saveNetwork(store *config.Store) func(netcfg.Saved) error {
	return func(s netcfg.Saved) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		var prev json.RawMessage
		store.Update(func(st *config.State) {
			prev = append(json.RawMessage(nil), st.Network...)
			st.Network = b
		})
		err = store.Flush()
		if err != nil {
			store.Update(func(st *config.State) {
				if bytes.Equal(st.Network, b) { // not already replaced by a newer save
					st.Network = prev
				}
			})
		}
		return err
	}
}

// splitList splits a comma-separated flag value, dropping blanks.
// mergeHosts joins the names from --allowed-hosts and from emm.txt into one
// allow-list, dropping repeats (names compare case-insensitively).
func mergeHosts(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, h := range l {
			if k := strings.ToLower(h); !seen[k] {
				seen[k] = true
				out = append(out, h)
			}
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func describeBackends(bs []router.Backend) string {
	if len(bs) == 0 {
		return "none"
	}
	names := make([]string, len(bs))
	for i, b := range bs {
		names[i] = b.Name()
	}
	return strings.Join(names, "+")
}

func hostnameFunc(m *netcfg.Manager) func() string {
	return func() string {
		if m != nil {
			if h := m.Status().Hostname; h != "" {
				return h
			}
		}
		if h, err := os.Hostname(); err == nil && h != "" {
			if i := strings.IndexByte(h, '.'); i >= 0 {
				h = h[:i]
			}
			return strings.ToLower(h)
		}
		return "emm"
	}
}

// rtpSyncer adapts rtpmidi.Manager to app.RTPSyncer.
type rtpSyncer struct{ m *rtpmidi.Manager }

func (s rtpSyncer) Sync(specs []app.RTPSpec) error {
	out := make([]rtpmidi.Spec, len(specs))
	for i, sp := range specs {
		out[i] = rtpmidi.Spec{ID: sp.ID, Name: sp.Name, ControlPort: sp.ControlPort}
	}
	return s.m.Sync(out)
}

// rtpEndpoints builds the GET /api/rtp listing from the running manager.
func rtpEndpoints(m *rtpmidi.Manager, store *config.Store) func() []web.RTPEndpoint {
	return func() []web.RTPEndpoint {
		var out []web.RTPEndpoint
		for _, st := range m.Status() {
			dev := strings.TrimPrefix(st.ID, "rtp:")
			if strings.HasPrefix(dev, "net-") {
				dev = "" // extra virtual network port, not tied to a device
			}
			out = append(out, web.RTPEndpoint{ID: st.ID, Name: st.Name, ControlPort: st.ControlPort, Peers: len(st.Peers), Device: dev})
		}
		return out
	}
}

// netAdapter adapts *netcfg.Manager to web.Network.
type netAdapter struct{ m *netcfg.Manager }

func (a *netAdapter) Status() any           { return a.m.Status() }
func (a *netAdapter) InSetupMode() bool     { return a.m.InSetupMode() }
func (a *netAdapter) Networks() any         { return a.m.CachedScan() }
func (a *netAdapter) BootManaged() []string { return a.m.Status().BootManaged }
func (a *netAdapter) Scan(ctx context.Context, force bool) (any, bool, error) {
	nets, err := a.m.Scan(ctx, force)
	return nets, a.m.InSetupMode() && !force, err
}
func (a *netAdapter) Connect(ssid, password string) error {
	return mapNetErr(a.m.Connect(ssid, password))
}
func (a *netAdapter) Forget() error     { return mapNetErr(a.m.Forget()) }
func (a *netAdapter) SavedSSID() string { return a.m.SavedSSID() }
func (a *netAdapter) Ethernet() any     { return a.m.EthernetSettings() }
func (a *netAdapter) SetEthernet(mode, address, gateway string, dns []string) error {
	return mapNetErr(a.m.SetEthernet(netcfg.EthernetSettings{Mode: mode, Address: address, Gateway: gateway, DNS: dns}))
}
func (a *netAdapter) SetHostname(h string) error { return mapNetErr(a.m.SetHostname(h)) }

// mapNetErr turns "emm.txt fixes this setting" and "no WiFi adapter" into 409s.
func mapNetErr(err error) error {
	if errors.Is(err, netcfg.ErrBootManaged) || errors.Is(err, netcfg.ErrNoWifi) || errors.Is(err, netcfg.ErrBusy) {
		return web.ConflictError{Err: err}
	}
	return err
}

// bleController starts, stops and renames the Bluetooth MIDI server as the
// settings change.
type bleController struct {
	host router.Host
	log  *slog.Logger
	note func()

	mu     sync.Mutex
	srv    *blemidi.Server
	cancel context.CancelFunc
	done   chan struct{}
	name   string
}

func newBLEController(host router.Host, log *slog.Logger, note func()) *bleController {
	return &bleController{host: host, log: log, note: note}
}

func (c *bleController) apply(enabled bool, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !enabled {
		c.stopLocked()
		return
	}
	if c.srv != nil {
		if c.name != name {
			c.name = name
			c.srv.SetName(name)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel, c.name, c.done = cancel, name, make(chan struct{})
	c.srv = blemidi.NewServer(blemidi.Options{Name: name, Logger: c.log, OnChange: c.note}, c.host)
	srv, done := c.srv, c.done
	go func() {
		defer close(done)
		if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("Bluetooth MIDI stopped", "err", err)
		}
	}()
}

func (c *bleController) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *bleController) stopLocked() {
	if c.srv == nil {
		return
	}
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
	c.srv, c.cancel = nil, nil
}

func (c *bleController) status() web.BLEStatus {
	c.mu.Lock()
	srv := c.srv
	c.mu.Unlock()
	if srv == nil {
		return web.BLEStatus{}
	}
	st := srv.Status()
	return web.BLEStatus{Available: st.Available, Advertising: st.Advertising, Connected: st.Connected, Error: st.Error}
}
