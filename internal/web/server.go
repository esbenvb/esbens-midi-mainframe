// Package web serves EMM's HTTP API, the live event stream and the embedded UI.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

//go:embed all:ui
var embeddedUI embed.FS

// RTPEndpoint describes one network MIDI endpoint, for GET /api/rtp.
type RTPEndpoint struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ControlPort int    `json:"control_port"`
	Peers       int    `json:"peers"`
	Device      string `json:"device"` // paired device port ID; "" for extra virtual ports
}

// BLEStatus is the Bluetooth MIDI state shown in settings.
type BLEStatus struct {
	Available   bool   `json:"available"`
	Advertising bool   `json:"advertising"`
	Connected   int    `json:"connected"`
	Error       string `json:"error"`
}

// ConflictError marks an error as "the request is fine but conflicts with how
// the device is configured" (HTTP 409), e.g. a setting fixed by emm.txt.
type ConflictError struct{ Err error }

func (e ConflictError) Error() string { return e.Err.Error() }
func (e ConflictError) Unwrap() error { return e.Err }

// errStatus maps an error from a collaborator to an HTTP status.
func errStatus(err error) int {
	var c ConflictError
	if errors.As(err, &c) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// Network is the slice of network management the API needs. Status and
// Networks are returned as `any` so the JSON shape is owned by the provider
// (see docs/ARCHITECTURE.md).
type Network interface {
	Status() any
	InSetupMode() bool
	Networks() any
	Scan(ctx context.Context, force bool) (networks any, stale bool, err error)
	Connect(ssid, password string) error
	Forget() error
	SavedSSID() string
	Ethernet() any
	SetEthernet(mode, address, gateway string, dns []string) error
	SetHostname(hostname string) error
	// BootManaged lists settings that emm.txt currently overrides.
	BootManaged() []string
}

// Deps is everything the server needs from the rest of the daemon.
type Deps struct {
	Router *router.Router
	Store  *config.Store
	Logger *slog.Logger

	Version    string
	Backend    string
	AllowPower bool

	// Hostname returns the current hostname (first label of emm.local).
	Hostname func() string
	// AllowedHosts lists extra Host header names the server accepts besides
	// the built-in ones (see hostAllowed): exact names, compared
	// case-insensitively, and entries starting with "." which match any name
	// ending in that suffix (".example.org" matches "emm.example.org").
	// It is the escape hatch for a DNS name of your own, e.g. a reverse proxy.
	// emmd fills it from --allowed-hosts and from allowed_hosts in emm.txt.
	AllowedHosts []string
	// UIDir, if set, serves the UI from disk instead of the embedded copy.
	UIDir string

	// Optional collaborators.
	Net        Network
	RTP        func() []RTPEndpoint
	BLE        func() BLEStatus
	BLEDefault string // default Bluetooth name

	// Boot-file overrides (emm.txt). nil pointers mean "not set".
	BootRTP, BootBLE *bool
	BootBLEName      string
	BootWebPassword  string

	// Reconcile is called after a change that can alter which network
	// endpoints exist or which default connections should be made.
	Reconcile func()
	// ApplySettings is called after RTP/BLE settings change.
	ApplySettings func()
	// Power performs "reboot" or "shutdown". Defaults to systemctl.
	Power func(action string) error
	// Wrap, if set, wraps the whole handler (outermost). The daemon uses it
	// for the captive-portal middleware.
	Wrap func(http.Handler) http.Handler
}

// Server is the HTTP server's handler plus its background watchers.
type Server struct {
	d       Deps
	log     *slog.Logger
	hub     *hub
	mux     *http.ServeMux
	started time.Time

	startOnce sync.Once

	// testHookInitialState, when set, runs in handleEvents after the first
	// topology snapshot is taken and before it is sent (tests use it to change
	// a port's counters inside that window).
	testHookInitialState func()
	// testHookSnapshotTaken, when set, runs in buildState right after the
	// router's snapshot (with its counters) was taken: a counter that changes
	// there is not in the snapshot, so the first activity tick must send it.
	testHookSnapshotTaken func()
}

// New builds a Server. Call Start to run its background watchers, then serve
// Handler() with net/http.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Hostname == nil {
		d.Hostname = func() string { return "emm" }
	}
	if d.Power == nil {
		d.Power = defaultPower
	}
	s := &Server{d: d, log: d.Logger.With("component", "web"), hub: newHub(), mux: http.NewServeMux(), started: time.Now()}
	s.routes()
	return s
}

// Start runs the watchers that turn router changes into SSE events.
func (s *Server) Start(ctx context.Context) {
	s.startOnce.Do(func() { go s.watchTopology(ctx) })
}

// NotifyTopology / NotifySettings / NotifyNetwork push an SSE event to every client.
func (s *Server) NotifyTopology() { s.hub.publish("topology", nil) }
func (s *Server) NotifySettings() { s.hub.publish("settings", nil) }
func (s *Server) NotifyNetwork()  { s.hub.publish("network", nil) }

func (s *Server) watchTopology(ctx context.Context) {
	ch := s.d.Router.Changes()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		// Re-arm before debouncing and publishing, so nothing that changes in
		// between is lost; the published state is computed fresh anyway.
		ch = s.d.Router.Changes()
		// Coalesce bursts (a hub with ten synths plugging in at once).
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Millisecond):
		}
		s.hub.publish("topology", nil)
	}
}

// Handler returns the complete HTTP handler.
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.security(h)
	h = s.auth(h)
	h = s.hostCheck(h)
	h = s.recoverAndLog(h)
	if s.d.Wrap != nil {
		h = s.d.Wrap(h)
	}
	return h
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("PUT /api/connections", s.handlePutConnection)
	m.HandleFunc("DELETE /api/connections", s.handleDeleteConnection)
	m.HandleFunc("PATCH /api/ports/{id}", s.handlePatchPort)
	m.HandleFunc("DELETE /api/ports/{id}", s.handleDeletePort)
	m.HandleFunc("POST /api/ports/forget-offline", s.handleForgetOffline)
	m.HandleFunc("POST /api/ports/{id}/panic", s.handlePanic)
	m.HandleFunc("POST /api/ports/{id}/test-note", s.handleTestNote)

	m.HandleFunc("GET /api/rtp", s.handleRTPList)
	m.HandleFunc("POST /api/rtp/ports", s.handleRTPAdd)
	m.HandleFunc("DELETE /api/rtp/ports/{id}", s.handleRTPDelete)

	m.HandleFunc("GET /api/settings", s.handleSettingsGet)
	m.HandleFunc("PUT /api/settings", s.handleSettingsPut)

	m.HandleFunc("GET /api/network", s.handleNetworkGet)
	m.HandleFunc("POST /api/network/scan", s.handleNetworkScan)
	m.HandleFunc("POST /api/network/wifi", s.handleNetworkWifi)
	m.HandleFunc("DELETE /api/network/wifi", s.handleNetworkForget)
	m.HandleFunc("PUT /api/network/ethernet", s.handleNetworkEthernet)
	m.HandleFunc("PUT /api/network/hostname", s.handleNetworkHostname)

	m.HandleFunc("GET /api/system", s.handleSystem)
	m.HandleFunc("POST /api/system/reboot", func(w http.ResponseWriter, r *http.Request) { s.handlePower(w, r, "reboot") })
	m.HandleFunc("POST /api/system/shutdown", func(w http.ResponseWriter, r *http.Request) { s.handlePower(w, r, "shutdown") })

	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no such API endpoint")
	})
	m.HandleFunc("/", s.handleStatic)
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "request body is empty")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	return true
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,200}$`)

func validID(id string) bool { return idRE.MatchString(id) }

// ---- middleware --------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) recoverAndLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				if rec.status == 200 {
					writeErr(rec, http.StatusInternalServerError, "internal error")
				}
			}
			if rec.status >= 500 {
				s.log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "status", rec.status)
			} else if r.URL.Path != "/api/events" {
				s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// hostCheck defends against DNS-rebinding: a hostile web page that resolves
// its own domain to the Pi's address must not be able to drive the API. See
// hostAllowed for the accepted Host values.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, s.d.Hostname(), s.d.AllowedHosts) {
			writeErr(w, http.StatusMisdirectedRequest, "unknown host name; use http://"+s.d.Hostname()+
				".local/ or the device's IP address, or allow this name: add it to allowed_hosts in emm.txt"+
				" (the config file on the SD card), or start emmd with --allowed-hosts=NAME"+
				" (a name starting with a dot, such as .example.org, allows every name below it)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// privateTLDs are suffixes that are never delegated in the public DNS (RFC 6761,
// RFC 8375 and the usual router conventions), so an outsider cannot register a
// name below them and point it at the Pi.
var privateTLDs = map[string]bool{
	"lan": true, "home": true, "internal": true, "localdomain": true, "intranet": true, "private": true,
}

// hostAllowed reports whether the Host header value may reach the API. The
// accepted values are an explicit allow-list, never a pattern an outside
// domain can satisfy:
//
//   - IP literals (v4, and v6 in brackets), and "localhost";
//   - the bare hostname ("emm") and any name ending in ".local" (mDNS);
//   - "<hostname>.<private TLD>" and "<hostname>.home.arpa", also with more
//     labels in between ("emm.office.lan"), for names a router hands out;
//   - the names listed in extra (Deps.AllowedHosts).
//
// A trailing dot, a port and upper case are ignored. Anything else, including
// "emm.attacker.example" (right first label, public suffix), is refused.
func hostAllowed(hostport, hostname string, extra []string) bool {
	host := hostport
	if h, port, err := net.SplitHostPort(hostport); err == nil {
		if strings.Trim(port, "0123456789") != "" {
			return false
		}
		host = h
	}
	host = strings.ToLower(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		_, err := netip.ParseAddr(host[1 : len(host)-1]) // brackets are for IPv6 literals only
		return err == nil
	}
	if host == "" {
		return true // HTTP/1.0 without a Host header; browsers always send one
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "localhost" {
		return true
	}
	if !validDNSName(host) {
		return false
	}
	labels := strings.Split(host, ".")
	hn := strings.ToLower(hostname)
	last := labels[len(labels)-1]
	switch {
	case len(labels) == 1 && host == hn:
		return true
	case len(labels) > 1 && last == "local":
		return true
	case len(labels) > 1 && labels[0] == hn && (privateTLDs[last] ||
		len(labels) > 2 && last == "arpa" && labels[len(labels)-2] == "home"):
		return true
	}
	for _, e := range extra {
		e = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e)), ".")
		switch {
		case e == "":
		case e[0] == '.':
			if len(e) > 1 && len(host) > len(e) && strings.HasSuffix(host, e) {
				return true
			}
		case host == e:
			return true
		}
	}
	return false
}

// validDNSName is a strict syntax check for a lower-case ASCII host name: 1-63
// characters from [a-z0-9_-] per label, no empty labels. Browsers send IDNs in
// their punycode form.
func validDNSName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	start := 0
	for i := 0; i <= len(h); i++ {
		if i < len(h) && h[i] != '.' {
			c := h[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
			continue
		}
		if i == start || i-start > 63 {
			return false
		}
		start = i + 1
	}
	return true
}

// security adds response headers and CSRF protection for state-changing calls.
func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			// Cross-site form posts can't set a JSON content type or omit Origin
			// consistently; require same-origin when the browser says where it is from.
			if o := r.Header.Get("Origin"); o != "" {
				if !sameOrigin(o, r.Host) {
					writeErr(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
			if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(origin), "http://"), "https://")
	return o == strings.ToLower(host)
}

// auth enforces the optional UI password with HTTP Basic (any user name).
// While the device is in WiFi setup mode the calls the onboarding page needs
// stay open (see setupRoute), because the only network is the setup access
// point and the user has no other way in. Everything else, settings and
// password changes, power, connections and so on, needs the password even then:
// the setup network is often open, so anyone in range could otherwise use it.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authRequired() || r.URL.Path == "/healthz" || s.setupOpen(r) {
			next.ServeHTTP(w, r)
			return
		}
		_, pw, ok := r.BasicAuth()
		if !ok || !s.checkPassword(pw) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Esben's MIDI Mainframe", charset="UTF-8"`)
			writeErr(w, http.StatusUnauthorized, "password required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupOpen reports whether r may skip the password because the device is in
// setup mode and r is one of the onboarding calls.
func (s *Server) setupOpen(r *http.Request) bool {
	return s.d.Net != nil && s.d.Net.InSetupMode() && setupRoute(r.Method, r.URL.Path)
}

// setupRoute lists what the setup page uses without credentials: the static UI
// files (any GET/HEAD outside /api), GET /api/network, POST /api/network/scan
// and POST /api/network/wifi.
func setupRoute(method, p string) bool {
	p = path.Clean("/" + p) // the mux redirects unclean paths; decide on the clean one anyway
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		switch method + " " + p {
		case "GET /api/network", "POST /api/network/scan", "POST /api/network/wifi":
			return true
		}
		return false
	}
	return method == http.MethodGet || method == http.MethodHead
}

func (s *Server) authRequired() bool {
	if s.d.BootWebPassword != "" {
		return true
	}
	var set bool
	s.d.Store.View(func(st *config.State) { set = st.WebPasswordSet() })
	return set
}

func (s *Server) checkPassword(pw string) bool {
	if s.d.BootWebPassword != "" {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(s.d.BootWebPassword)) == 1
	}
	var ok bool
	s.d.Store.View(func(st *config.State) { ok = st.CheckWebPassword(pw) })
	return ok
}

// ---- static UI -----------------------------------------------------------------

func (s *Server) uiFS() fs.FS {
	if s.d.UIDir != "" {
		return os.DirFS(s.d.UIDir)
	}
	sub, err := fs.Sub(embeddedUI, "ui")
	if err != nil {
		return embeddedUI
	}
	return sub
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fsys := s.uiFS()
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch strings.TrimSuffix(name, "/") {
	case "", "setup", "index.html":
		name = "index.html"
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, ".") || strings.Contains(name, "/.") {
		http.NotFound(w, r)
		return
	}
	f, err := fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	// Small files on a LAN: always revalidate so a UI update applies at once.
	w.Header().Set("Cache-Control", "no-cache")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, st.ModTime(), rs)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(data)))
}
