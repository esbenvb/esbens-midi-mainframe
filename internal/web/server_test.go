package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emm/internal/config"
	"emm/internal/router"
)

type tport struct {
	info router.PortInfo
	mu   sync.Mutex
	got  []byte
}

func (p *tport) Info() router.PortInfo { return p.info }
func (p *tport) Write(b []byte) error {
	p.mu.Lock()
	p.got = append(p.got, b...)
	p.mu.Unlock()
	return nil
}
func (p *tport) Close() error { return nil }
func (p *tport) data() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.got...)
}

func usb(id, name string, in, out bool) *tport {
	return &tport{info: router.PortInfo{ID: id, Name: name, Kind: router.KindUSB, Group: name, Input: in, Output: out, Detail: "test"}}
}

type fakeNet struct {
	setup     bool
	connected []string
	forgot    bool
	hostname  string
}

func (f *fakeNet) Status() any {
	return map[string]any{"mode": "client", "setup_mode": f.setup, "hostname": "emm"}
}
func (f *fakeNet) InSetupMode() bool { return f.setup }
func (f *fakeNet) Networks() any     { return []map[string]any{{"ssid": "Home", "signal": 80}} }
func (f *fakeNet) Scan(ctx context.Context, force bool) (any, bool, error) {
	return f.Networks(), f.setup && !force, nil
}
func (f *fakeNet) Connect(ssid, pw string) error {
	if ssid == "Managed" {
		return ConflictError{Err: errors.New("controlled by emm.txt")}
	}
	f.connected = append(f.connected, ssid+"/"+pw)
	return nil
}
func (f *fakeNet) Forget() error                                { f.forgot = true; return nil }
func (f *fakeNet) SavedSSID() string                            { return "Home" }
func (f *fakeNet) Ethernet() any                                { return map[string]any{"mode": "auto"} }
func (f *fakeNet) SetEthernet(m, a, g string, d []string) error { return nil }
func (f *fakeNet) SetHostname(h string) error                   { f.hostname = h; return nil }
func (f *fakeNet) BootManaged() []string                        { return []string{"hostname"} }

type env struct {
	srv        *Server
	ts         *httptest.Server
	r          *router.Router
	store      *config.Store
	net        *fakeNet
	a, b       *tport
	reconciled atomic.Int32
	applied    atomic.Int32
	power      chan string
}

func newEnv(t *testing.T, mut func(*Deps)) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{power: make(chan string, 4), net: &fakeNet{}}
	e.r = router.New(router.Options{Logger: log})
	st, err := config.Open(filepath.Join(t.TempDir(), "state.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	st.SaveDelay = time.Millisecond
	t.Cleanup(func() { _ = st.Flush() }) // runs before the temp dir is removed
	e.store = st
	e.a, e.b = usb("usb:aaaa:0001:1.1:0", "Keys", true, false), usb("usb:bbbb:0002:1.2:0", "Synth", true, true)
	e.r.AddPort(e.a)
	e.r.AddPort(e.b)
	d := Deps{
		Router: e.r, Store: st, Logger: log, Version: "test", Backend: "fake", AllowPower: true,
		Hostname: func() string { return "emm" }, Net: e.net,
		RTP:           func() []RTPEndpoint { return []RTPEndpoint{{ID: "rtp:x", Name: "X", ControlPort: 5004}} },
		BLE:           func() BLEStatus { return BLEStatus{Available: true} },
		Reconcile:     func() { e.reconciled.Add(1) },
		ApplySettings: func() { e.applied.Add(1) },
		Power:         func(a string) error { e.power <- a; return nil },
	}
	if mut != nil {
		mut(&d)
	}
	e.srv = New(d)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.srv.Start(ctx)
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) do(t *testing.T, method, path, body string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	return e.doWith(t, http.DefaultClient, method, path, body, hdr...)
}

// noRedirect is a client that hands back the first response instead of
// following a redirect, for tests that must see what the server answered to
// the request itself.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) doWith(t *testing.T, c *http.Client, method, path, body string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	resp, b := e.do(t, "GET", path, "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("GET %s: bad JSON: %v\n%s", path, err, b)
	}
}

func TestStateListsPortsAndGhosts(t *testing.T) {
	e := newEnv(t, nil)
	e.r.Load([]router.ConnSpec{{From: e.a.info.ID, To: e.b.info.ID, Filter: router.DefaultFilter()},
		{From: "usb:cccc:0003:1.3:0", To: e.b.info.ID, Filter: router.Filter{}}})
	e.store.Update(func(st *config.State) {
		st.Known["usb:cccc:0003:1.3:0"] = config.KnownPort{Name: "Old Synth", Kind: router.KindUSB, Input: true, Group: "Old"}
		st.Ports[e.a.info.ID] = config.PortSettings{Alias: "My Keys"}
	})
	var s stateBody
	e.getJSON(t, "/api/state", &s)
	if s.Hostname != "emm" || s.Version != "test" || s.Now == 0 {
		t.Fatalf("header: %+v", s)
	}
	byID := map[string]portJSON{}
	for _, p := range s.Ports {
		byID[p.ID] = p
	}
	if len(s.Ports) != 3 {
		t.Fatalf("want 2 live + 1 ghost, got %d", len(s.Ports))
	}
	if p := byID[e.a.info.ID]; !p.Online || p.Alias != "My Keys" || !p.RTP {
		t.Fatalf("keys: %+v", p)
	}
	if g := byID["usb:cccc:0003:1.3:0"]; g.Online || g.Name != "Old Synth" || !g.Input {
		t.Fatalf("ghost: %+v", g)
	}
	if len(s.Connections) != 2 || !s.Connections[0].Online && !s.Connections[1].Online {
		t.Fatalf("connections: %+v", s.Connections)
	}
}

func TestConnectionsLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID

	resp, body := e.do(t, "PUT", "/api/connections", `{"from":"`+a+`","to":"`+b+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, body)
	}
	var c connJSON
	_ = json.Unmarshal(body, &c)
	if !c.Online || len(c.Filter.Block) != 1 || c.Filter.Block[0] != "active_sense" {
		t.Fatalf("default filter expected: %+v", c)
	}

	// Updating the filter explicitly, then re-PUTting without a filter keeps it.
	e.do(t, "PUT", "/api/connections", `{"from":"`+a+`","to":"`+b+`","filter":{"block":["cc"],"transpose":2}}`)
	_, body = e.do(t, "PUT", "/api/connections", `{"from":"`+a+`","to":"`+b+`"}`)
	_ = json.Unmarshal(body, &c)
	if c.Filter.Transpose != 2 || c.Filter.Block[0] != "cc" {
		t.Fatalf("filter not preserved: %+v", c.Filter)
	}

	// Traffic actually flows.
	if err := e.r.Inject(a, nil); err == nil || err != router.ErrNotOutput {
		t.Fatalf("inject into an input-only port: %v", err)
	}

	for _, bad := range []string{
		`{"from":"","to":"x"}`,
		`{"from":"` + a + `","to":"` + a + `"}`,
		`{"from":"` + b + `","to":"` + a + `"}`, // a cannot receive
		`{"from":"` + a + `","to":"` + b + `","filter":{"block":["nope"]}}`,
		`not json`,
	} {
		resp, _ := e.do(t, "PUT", "/api/connections", bad)
		if resp.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d", bad, resp.StatusCode)
		}
	}

	resp, _ = e.do(t, "DELETE", "/api/connections?from="+a+"&to="+b, "")
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "DELETE", "/api/connections?from="+a+"&to="+b, "")
	if resp.StatusCode != 404 {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "DELETE", "/api/connections", "")
	if resp.StatusCode != 400 {
		t.Fatalf("delete without params: %d", resp.StatusCode)
	}
}

func TestPersistsConnectionsThroughStore(t *testing.T) {
	e := newEnv(t, nil)
	// The daemon wires OnConnectionsChanged to the store; emulate with the router API.
	e.r.Load(nil)
	_ = e.r.Connect(e.a.info.ID, e.b.info.ID, router.Filter{})
	if len(e.r.Connections()) != 1 {
		t.Fatal("setup")
	}
}

// R8: PUT /api/connections refuses ports the device has never seen (404), so a
// typo is not persisted as a junk connection; dormant connections to remembered
// ports stay possible.
func TestPutConnectionUnknownPort(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	const gone, typo = "usb:cccc:0003:1.3:0", "usb:aaaa:0001:1.1:O" // O, not zero
	e.store.Update(func(st *config.State) {
		st.Known[gone] = config.KnownPort{Name: "Old Synth", Kind: router.KindUSB, Input: true, Output: true}
		st.Ports["rtp:usb:dddd:0004:1.4:0"] = config.PortSettings{Alias: "net"}
	})

	for _, body := range []string{
		`{"from":"` + typo + `","to":"` + b + `"}`,
		`{"from":"` + a + `","to":"` + typo + `"}`,
		`{"from":"nope","to":"also-nope"}`,
		`{"from":"` + typo + `","to":"` + b + `","filter":{"block":["cc"]}}`,
	} {
		resp, rb := e.do(t, "PUT", "/api/connections", body)
		if resp.StatusCode != 404 || !strings.Contains(string(rb), "unknown port") {
			t.Errorf("%s: want 404 unknown port, got %d %s", body, resp.StatusCode, rb)
		}
	}
	if n := len(e.r.Connections()); n != 0 {
		t.Fatalf("a refused PUT left %d connections behind: %v", n, e.r.Connections())
	}
	// Malformed IDs and bodies keep their 400.
	if resp, _ := e.do(t, "PUT", "/api/connections", `{"from":"bad id","to":"`+b+`"}`); resp.StatusCode != 400 {
		t.Errorf("invalid id: %d", resp.StatusCode)
	}

	// A remembered (Known) port that is unplugged: allowed, the connection is dormant.
	resp, rb := e.do(t, "PUT", "/api/connections", `{"from":"`+gone+`","to":"`+b+`"}`)
	var c connJSON
	if resp.StatusCode != 200 || json.Unmarshal(rb, &c) != nil || c.Online || c.From != gone {
		t.Fatalf("dormant connection to a known port: %d %s", resp.StatusCode, rb)
	}
	// So is one to a port that only has settings in the store.
	resp, rb = e.do(t, "PUT", "/api/connections", `{"from":"`+a+`","to":"rtp:usb:dddd:0004:1.4:0"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("connection to a port with stored settings: %d %s", resp.StatusCode, rb)
	}
	if n := len(e.r.Connections()); n != 2 {
		t.Fatalf("want the two dormant connections, got %v", e.r.Connections())
	}
}

func TestPatchPort(t *testing.T) {
	e := newEnv(t, nil)
	a := e.a.info.ID
	_ = e.r.Connect(a, e.b.info.ID, router.Filter{})
	pair := "rtp:" + e.b.info.ID
	e.r.AddPort(&tport{info: router.PortInfo{ID: pair, Name: "net", Kind: router.KindRTP, Input: true, Output: true}})
	_ = e.r.Connect(e.b.info.ID, pair, router.Filter{})
	_ = e.r.Connect(pair, e.b.info.ID, router.Filter{})

	resp, body := e.do(t, "PATCH", "/api/ports/"+a, `{"alias":"  Master Keys ","hidden":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	var p portJSON
	_ = json.Unmarshal(body, &p)
	if p.Alias != "Master Keys" || !p.Hidden {
		t.Fatalf("patched: %+v", p)
	}
	if e.reconciled.Load() == 0 {
		t.Fatal("reconcile not called")
	}

	// Turning network exposure off removes the paired connections.
	resp, _ = e.do(t, "PATCH", "/api/ports/"+e.b.info.ID, `{"rtp":false}`)
	if resp.StatusCode != 200 {
		t.Fatalf("rtp off: %d", resp.StatusCode)
	}
	for _, c := range e.r.Connections() {
		if c.From == pair || c.To == pair {
			t.Fatalf("paired connection survived: %+v", c)
		}
	}
	if len(e.r.Connections()) != 1 {
		t.Fatalf("unrelated connection was removed: %+v", e.r.Connections())
	}

	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/ports/nope", `{"alias":"x"}`, 404},
		{"/api/ports/" + a, `{"alias":"` + strings.Repeat("x", 80) + `"}`, 400},
		{"/api/ports/" + pair, `{"rtp":true}`, 400},
	} {
		resp, _ := e.do(t, "PATCH", tc.path, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: want %d got %d", tc.path, tc.body, tc.want, resp.StatusCode)
		}
	}
}

func TestPanicAndTestNote(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := e.do(t, "POST", "/api/ports/"+e.b.info.ID+"/test-note", "")
	if resp.StatusCode != 200 {
		t.Fatalf("test-note: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(e.b.data()) < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.b.data(); len(got) < 6 || got[0] != 0x90 || got[3] != 0x80 {
		t.Fatalf("test note bytes: %x", got)
	}
	resp, _ = e.do(t, "POST", "/api/ports/"+e.b.info.ID+"/panic", "")
	if resp.StatusCode != 200 {
		t.Fatalf("panic: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/api/ports/missing/panic", "")
	if resp.StatusCode != 404 {
		t.Fatalf("panic on unknown port: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/api/ports/"+e.a.info.ID+"/panic", "")
	if resp.StatusCode != 400 {
		t.Fatalf("panic on input-only port: %d", resp.StatusCode)
	}
}

func TestHostCheck(t *testing.T) {
	e := newEnv(t, nil)
	for host, want := range map[string]int{
		"emm.local":         200,
		"emm":               200,
		"emm.lan":           200,
		"EMM.home:8080":     200,
		"something.local":   200,
		"127.0.0.1:8099":    200,
		"192.168.1.20":      200,
		"[::1]:80":          200,
		"evil.example.com":  421,
		"attacker.com:8080": 421,
		// R1: a public name whose first label is the hostname must not pass.
		"emm.evil.com":              421,
		"emm.attacker.example:80":   421,
		"emm.192.168.1.50.sslip.io": 421,
		"emm.home.arpa":             200,
		"emm.office.lan":            200,
	} {
		resp, b := e.do(t, "GET", "/healthz", "", "Host", host)
		if resp.StatusCode != want {
			t.Errorf("Host %q: want %d got %d", host, want, resp.StatusCode)
		}
		if want == 421 && (!strings.Contains(string(b), "--allowed-hosts") || !strings.Contains(string(b), "allowed_hosts in emm.txt")) {
			t.Errorf("Host %q: the 421 message should say how to allow a name (the flag and the emm.txt key): %s", host, b)
		}
	}
}

func TestHostCheckAllowedHosts(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.AllowedHosts = []string{"pi.example.org", ".studio.example"} })
	for host, want := range map[string]int{
		"pi.example.org":        200,
		"PI.Example.Org:8080":   200,
		"rack.studio.example":   200,
		"emm.local":             200,
		"example.org":           421,
		"x.pi.example.org":      421,
		"studio.example":        421,
		"emm.studio.evil.com":   421,
		"evilstudio.example":    421,
		"emm.evil.com":          421,
		"rack.studio.example.x": 421,
	} {
		if resp, _ := e.do(t, "GET", "/healthz", "", "Host", host); resp.StatusCode != want {
			t.Errorf("Host %q: want %d got %d", host, want, resp.StatusCode)
		}
	}
}

func TestHostAllowedTable(t *testing.T) {
	extra := []string{"pi.example.org", ".studio.example", "  MixedCase.Example.NET. ", "", ".", "*.wild.example"}
	for _, tc := range []struct {
		host string
		want bool
	}{
		// IP literals and localhost, with and without port
		{"127.0.0.1", true}, {"127.0.0.1:8099", true}, {"192.168.1.20", true}, {"10.0.0.1:80", true},
		{"[::1]", true}, {"[::1]:80", true}, {"::1", true}, {"[fe80::1]:8080", true}, {"[2001:db8::1]", true},
		{"localhost", true}, {"LOCALHOST:80", true}, {"localhost.", true}, {"evil.localhost", false},
		// the bare hostname
		{"emm", true}, {"EMM", true}, {"emm:80", true}, {"emm.", true}, {"other", false}, {"emmx", false},
		// mDNS: any *.local
		{"emm.local", true}, {"emm.local.", true}, {"EMM.LOCAL:8080", true}, {"something.local", true},
		{"a.b.local", true}, {".local", false}, {"local", false}, {"emm.local.evil.com", false},
		// names a router hands out: <hostname>.<private TLD>, <hostname>.home.arpa
		{"emm.lan", true}, {"emm.lan.", true}, {"EMM.Lan:8080", true}, {"emm.home", true}, {"EMM.home:8080", true},
		{"emm.home.arpa", true}, {"emm.home.arpa.", true}, {"emm.office.lan", true}, {"emm.internal", true},
		{"emm.localdomain", true}, {"emm.intranet", true}, {"emm.private", true},
		{"other.lan", false}, {"home.arpa", false}, {"emm.arpa", false}, {"lan", false}, {"other.home.arpa", false},
		// DNS rebinding and syntax tricks
		{"emm.evil.com", false}, {"emm.evil.com:80", false}, {"emm.evil.com.", false}, {"EMM.Evil.COM", false},
		{"emm.attacker.example", false}, {"emm.192.168.1.50.sslip.io", false}, {"emm.lan.evil.com", false},
		{"emm.home.arpa.evil.com", false}, {"emm.com", false}, {"emm.lan@evil.com", false},
		{"emm.lan:80@evil.com", false}, {"evil.com:emm.lan", false}, {"emm..lan", false}, {"emm.lan..", false},
		{"emm.evil.com#.lan", false}, {"emm.lan/evil.com", false}, {"emm .lan", false}, {"evil.com", false},
		{"[emm.lan]", false},
		// Deps.AllowedHosts: exact names, ".suffix" entries, case and dots ignored
		{"pi.example.org", true}, {"PI.Example.Org:8080", true}, {"pi.example.org.", true}, {"x.pi.example.org", false},
		{"example.org", false}, {"foo.studio.example", true}, {"a.b.studio.example", true}, {"studio.example", false},
		{"evilstudio.example", false}, {"mixedcase.example.net", true}, {"foo.wild.example", false},
		{"anything.com", false}, // the "." entry must not allow everything
	} {
		if got := hostAllowed(tc.host, "emm", extra); got != tc.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
	// Without extras nothing from that list is special.
	if hostAllowed("pi.example.org", "emm", nil) {
		t.Error("pi.example.org must need an AllowedHosts entry")
	}
	// The hostname is compared case-insensitively and follows renames.
	if !hostAllowed("stage.lan", "Stage", nil) || hostAllowed("emm.lan", "stage", nil) {
		t.Error("hostname must be taken from the argument")
	}
	// An empty Host (HTTP/1.0 without the header) stays allowed.
	if !hostAllowed("", "emm", nil) {
		t.Error("empty host")
	}
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t, nil)
	host := strings.TrimPrefix(e.ts.URL, "http://")
	resp, _ := e.do(t, "POST", "/api/ports/"+e.b.info.ID+"/panic", "", "Origin", "http://evil.example")
	if resp.StatusCode != 403 {
		t.Errorf("cross-origin POST: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/api/ports/"+e.b.info.ID+"/panic", "", "Origin", "http://"+host)
	if resp.StatusCode != 200 {
		t.Errorf("same-origin POST: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("PUT", e.ts.URL+"/api/settings", strings.NewReader(`{"rtp":{"enabled":false}}`))
	req.Header.Set("Content-Type", "text/plain")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 415 {
		t.Errorf("text/plain body: %d", r2.StatusCode)
	}
	if h := resp.Header.Get("Content-Security-Policy"); !strings.Contains(h, "default-src 'self'") {
		t.Errorf("CSP header: %q", h)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, nil)
	// No password: open.
	if resp, _ := e.do(t, "GET", "/api/state", ""); resp.StatusCode != 200 {
		t.Fatal("open by default")
	}
	// Set one through the API.
	if resp, b := e.do(t, "PUT", "/api/settings", `{"web":{"password":"hunter2"}}`); resp.StatusCode != 200 || !strings.Contains(string(b), `"password_set":true`) {
		t.Fatalf("set password: %d %s", resp.StatusCode, b)
	}
	resp, _ := e.do(t, "GET", "/api/state", "")
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("expected 401 challenge, got %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/state", "", "Authorization", "Basic "+b64("emm:wrong")); resp.StatusCode != 401 {
		t.Fatal("wrong password accepted")
	}
	if resp, _ := e.do(t, "GET", "/api/state", "", "Authorization", "Basic "+b64("anyone:hunter2")); resp.StatusCode != 200 {
		t.Fatal("right password rejected")
	}
	if resp, _ := e.do(t, "GET", "/healthz", ""); resp.StatusCode != 200 {
		t.Fatal("healthz must stay open")
	}
	// Setup mode is open so the WiFi flow works on the setup network.
	e.net.setup = true
	if resp, _ := e.do(t, "GET", "/api/network", ""); resp.StatusCode != 200 {
		t.Fatal("setup mode must bypass the password")
	}
}

func TestBootPasswordWins(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.BootWebPassword = "fromfile" })
	if resp, _ := e.do(t, "GET", "/api/state", ""); resp.StatusCode != 401 {
		t.Fatal("boot password must be enforced")
	}
	if resp, _ := e.do(t, "GET", "/api/state", "", "Authorization", "Basic "+b64("u:fromfile")); resp.StatusCode != 200 {
		t.Fatal("boot password rejected")
	}
}

// R2: in setup mode only the onboarding calls skip the password.
func TestSetupModeAuthIsNarrow(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>emm</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "setup.js"), []byte("// js"), 0o644)
	for name, mut := range map[string]func(*env){
		"boot password":   nil,
		"stored password": func(e *env) { e.store.Update(func(st *config.State) { st.SetWebPassword("hunter2") }) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) {
				d.UIDir = dir
				if mut == nil {
					d.BootWebPassword = "hunter2"
				}
			})
			if mut != nil {
				mut(e)
			}
			e.net.setup = true
			a, b := e.a.info.ID, e.b.info.ID
			wifi := `{"ssid":"Home","password":"supersecret"}`

			for _, tc := range []struct {
				method, path, body string
				want               int // status without credentials
			}{
				// what the setup page needs
				{"GET", "/", "", 200}, {"GET", "/setup", "", 200}, {"GET", "/index.html", "", 200},
				{"GET", "/setup.js", "", 200}, {"HEAD", "/setup.js", "", 200}, {"GET", "/healthz", "", 200},
				{"GET", "/api/network", "", 200}, {"POST", "/api/network/scan", "", 200},
				{"POST", "/api/network/wifi", wifi, 202},
				// everything else needs the password, even in setup mode
				{"GET", "/api/state", "", 401}, {"GET", "/api/settings", "", 401},
				{"PUT", "/api/settings", `{"web":{"password":""}}`, 401},
				{"PUT", "/api/settings", `{"web":{"password":"mine"}}`, 401},
				{"POST", "/api/system/reboot", "", 401}, {"POST", "/api/system/shutdown", "", 401},
				{"GET", "/api/system", "", 401},
				{"PUT", "/api/connections", `{"from":"` + a + `","to":"` + b + `"}`, 401},
				{"DELETE", "/api/connections?from=" + a + "&to=" + b, "", 401},
				{"PATCH", "/api/ports/" + a, `{"alias":"x"}`, 401},
				{"POST", "/api/ports/" + b + "/panic", "", 401}, {"POST", "/api/ports/" + b + "/test-note", "", 401},
				{"GET", "/api/rtp", "", 401}, {"POST", "/api/rtp/ports", `{"name":"x"}`, 401},
				{"DELETE", "/api/rtp/ports/rtp:net-x", "", 401},
				{"DELETE", "/api/network/wifi", "", 401}, {"PUT", "/api/network/ethernet", `{"mode":"auto"}`, 401},
				{"PUT", "/api/network/hostname", `{"hostname":"stage"}`, 401},
				{"GET", "/api/network/wifi", "", 401}, {"POST", "/api/network", "", 401},
				{"GET", "/api/nope", "", 401}, {"GET", "/api", "", 401},
			} {
				resp, rb := e.do(t, tc.method, tc.path, tc.body)
				if resp.StatusCode != tc.want {
					t.Errorf("%s %s without password: want %d got %d %s", tc.method, tc.path, tc.want, resp.StatusCode, rb)
				}
			}
			// Unclean paths must not be mistaken for static files. The mux answers
			// them with a 301 to the clean path, and the client would then be judged
			// on that second request, which hides a setupRoute that does not clean
			// the path. So look at the first response: it has to be the 401 itself.
			for _, p := range []string{"//api/settings", "/index.html/../api/settings", "/./api/state"} {
				resp, rb := e.doWith(t, noRedirect, "GET", p, "")
				if resp.StatusCode != 401 {
					t.Errorf("GET %s without password: want 401 as the first response, got %d %s", p, resp.StatusCode, rb)
				}
			}
			// The event stream: if it were open it would never end, so look at the status line only.
			if resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(e.ts.URL + "/api/events"); err != nil {
				t.Errorf("GET /api/events: %v", err)
			} else {
				resp.Body.Close()
				if resp.StatusCode != 401 {
					t.Errorf("GET /api/events without password: want 401 got %d", resp.StatusCode)
				}
			}
			if len(e.net.hostname) != 0 || e.applied.Load() != 0 || len(e.r.Connections()) != 0 {
				t.Errorf("a refused call had an effect: hostname=%q applied=%d conns=%v", e.net.hostname, e.applied.Load(), e.r.Connections())
			}
			select {
			case a := <-e.power:
				t.Errorf("power action %q ran without the password", a)
			case <-time.After(1200 * time.Millisecond): // the handler would act after 700 ms
			}

			// With the password the rest works, and the password was not touched.
			auth := []string{"Authorization", "Basic " + b64("u:hunter2")}
			if resp, _ := e.do(t, "GET", "/api/state", "", auth...); resp.StatusCode != 200 {
				t.Errorf("state with password: %d", resp.StatusCode)
			}
			var s settingsBody
			resp, rb := e.do(t, "GET", "/api/settings", "", auth...)
			if resp.StatusCode != 200 || json.Unmarshal(rb, &s) != nil || !s.Web.PasswordSet {
				t.Errorf("password must still be set: %d %s", resp.StatusCode, rb)
			}
			if resp, _ := e.do(t, "PUT", "/api/connections", `{"from":"`+a+`","to":"`+b+`"}`, auth...); resp.StatusCode != 200 {
				t.Errorf("PUT connection with password: %d", resp.StatusCode)
			}
			// Wrong credentials do not help on a closed route.
			if resp, _ := e.do(t, "GET", "/api/state", "", "Authorization", "Basic "+b64("u:nope")); resp.StatusCode != 401 {
				t.Errorf("wrong password: %d", resp.StatusCode)
			}
			// Outside setup mode the onboarding calls need the password too.
			e.net.setup = false
			if resp, _ := e.do(t, "GET", "/api/network", ""); resp.StatusCode != 401 {
				t.Errorf("GET /api/network outside setup mode: %d", resp.StatusCode)
			}
		})
	}
}

func TestSettings(t *testing.T) {
	yes := true
	e := newEnv(t, func(d *Deps) { d.BootBLE = &yes; d.BootBLEName = "FromFile" })
	var s settingsBody
	e.getJSON(t, "/api/settings", &s)
	if !s.RTP.Enabled || !s.BLE.Enabled || s.BLE.Name != "FromFile" || !s.BLE.Available || s.Web.PasswordSet {
		t.Fatalf("defaults: %+v", s)
	}
	resp, b := e.do(t, "PUT", "/api/settings", `{"rtp":{"enabled":false}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, b)
	}
	_ = json.Unmarshal(b, &s)
	if s.RTP.Enabled {
		t.Fatal("rtp should be off")
	}
	if s.BLE.Name != "FromFile" {
		t.Fatal("emm.txt must keep winning for the BLE name")
	}
	if e.applied.Load() != 1 {
		t.Fatal("ApplySettings not called")
	}
	if st := e.store.Snapshot(); st.RTPOn() {
		t.Fatalf("not persisted: %+v", st)
	}
	// The BLE name is fixed by emm.txt in this environment: changing it is refused.
	if resp, b := e.do(t, "PUT", "/api/settings", `{"ble":{"name":"Stage Right"}}`); resp.StatusCode != 409 {
		t.Fatalf("emm.txt-managed name must answer 409, got %d %s", resp.StatusCode, b)
	}
	for _, bad := range []string{`{"ble":{"name":""}}`, `{"ble":{"name":"` + strings.Repeat("n", 21) + `"}}`} {
		if resp, _ := e.do(t, "PUT", "/api/settings", bad); resp.StatusCode != 400 {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
}

func TestRTPVirtualPorts(t *testing.T) {
	e := newEnv(t, nil)
	resp, b := e.do(t, "POST", "/api/rtp/ports", `{"name":"DAW Bus A"}`)
	if resp.StatusCode != 201 || !strings.Contains(string(b), `"rtp:net-daw-bus-a"`) {
		t.Fatalf("add: %d %s", resp.StatusCode, b)
	}
	resp, b = e.do(t, "POST", "/api/rtp/ports", `{"name":"DAW Bus A"}`)
	if !strings.Contains(string(b), `"rtp:net-daw-bus-a-2"`) {
		t.Fatalf("duplicate name must get a new id: %s", b)
	}
	if resp, _ := e.do(t, "POST", "/api/rtp/ports", `{"name":" "}`); resp.StatusCode != 400 {
		t.Fatal("empty name accepted")
	}
	var list []RTPEndpoint
	e.getJSON(t, "/api/rtp", &list)
	if len(list) != 1 || list[0].ControlPort != 5004 {
		t.Fatalf("rtp list: %+v", list)
	}
	if resp, _ := e.do(t, "DELETE", "/api/rtp/ports/rtp:net-daw-bus-a", ""); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/rtp/ports/rtp:net-daw-bus-a", ""); resp.StatusCode != 404 {
		t.Fatal("double delete")
	}
	if got := len(e.store.Snapshot().VirtualRTP); got != 1 {
		t.Fatalf("store has %d virtual ports", got)
	}
}

// web-daemon R3: the id that POST /api/rtp/ports hands out is usable at once.
// The endpoint only goes live when the reconciler has run (a moment later, and
// in this test never), and until then the id is in no other list.
func TestNewNetworkPortIsKnownBeforeTheReconcilerRuns(t *testing.T) {
	e := newEnv(t, nil)
	resp, b := e.do(t, "POST", "/api/rtp/ports", `{"name":"DAW"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("add: %d %s", resp.StatusCode, b)
	}
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &added); err != nil || added.ID != "rtp:net-daw" {
		t.Fatalf("add: %s (%v)", b, err)
	}
	if e.r.HasPort(added.ID) {
		t.Fatal("test set-up: the port must not be live yet")
	}

	// The documented sequence: create a network port, then connect it.
	b2 := e.b.info.ID
	if resp, b := e.do(t, "PUT", "/api/connections", `{"from":"`+added.ID+`","to":"`+b2+`"}`); resp.StatusCode != 200 {
		t.Fatalf("connect from the new port: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do(t, "PUT", "/api/connections", `{"from":"`+b2+`","to":"`+added.ID+`"}`); resp.StatusCode != 200 {
		t.Fatalf("connect to the new port: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do(t, "PATCH", "/api/ports/"+added.ID, `{"alias":"Studio DAW"}`); resp.StatusCode != 200 {
		t.Fatalf("patch the new port: %d %s", resp.StatusCode, b)
	}
	if got := e.store.Snapshot().Ports[added.ID].Alias; got != "Studio DAW" {
		t.Fatalf("alias not stored: %q", got)
	}

	// Once such a port is deleted again its id is unknown, as any typo is.
	if resp, b := e.do(t, "POST", "/api/rtp/ports", `{"name":"Gone"}`); resp.StatusCode != 201 {
		t.Fatalf("add: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "DELETE", "/api/rtp/ports/rtp:net-gone", ""); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	for _, id := range []string{"rtp:net-gone", "rtp:net-nope"} { // deleted, and never created
		if resp, _ := e.do(t, "PUT", "/api/connections", `{"from":"`+id+`","to":"`+b2+`"}`); resp.StatusCode != 404 {
			t.Fatalf("connect from %s: %d", id, resp.StatusCode)
		}
		if resp, _ := e.do(t, "PATCH", "/api/ports/"+id, `{"alias":"x"}`); resp.StatusCode != 404 {
			t.Fatalf("patch %s: %d", id, resp.StatusCode)
		}
	}
}

func TestNetworkEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	var n map[string]any
	e.getJSON(t, "/api/network", &n)
	if n["saved_ssid"] != "Home" || n["status"] == nil {
		t.Fatalf("network body: %v", n)
	}
	resp, b := e.do(t, "POST", "/api/network/wifi", `{"ssid":"Home","password":"supersecret"}`)
	if resp.StatusCode != 202 || len(e.net.connected) != 1 || e.net.connected[0] != "Home/supersecret" {
		t.Fatalf("connect: %d %s %v", resp.StatusCode, b, e.net.connected)
	}
	for _, bad := range []string{`{"ssid":"","password":"x"}`, `{"ssid":"Home","password":"short"}`} {
		if resp, _ := e.do(t, "POST", "/api/network/wifi", bad); resp.StatusCode != 400 {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "POST", "/api/network/wifi", `{"ssid":"Open"}`); resp.StatusCode != 202 {
		t.Error("open network rejected")
	}
	if resp, _ := e.do(t, "POST", "/api/network/wifi", `{"ssid":"Managed","password":"supersecret"}`); resp.StatusCode != 409 {
		t.Errorf("boot-managed setting should answer 409, got %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/network/wifi", ""); resp.StatusCode != 200 || !e.net.forgot {
		t.Error("forget")
	}
	if resp, _ := e.do(t, "PUT", "/api/network/hostname", `{"hostname":"stage"}`); resp.StatusCode != 200 || e.net.hostname != "stage" {
		t.Error("hostname")
	}
	if resp, _ := e.do(t, "PUT", "/api/network/ethernet", `{"mode":"banana"}`); resp.StatusCode != 400 {
		t.Error("bad ethernet mode accepted")
	}
	e.net.setup = true
	resp, b = e.do(t, "POST", "/api/network/scan", "")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"stale":true`) {
		t.Fatalf("scan: %d %s", resp.StatusCode, b)
	}
}

// R6: a raw 64-digit hex PSK is a valid WPA key (netcfg and the UI accept it).
func TestWifiPasswordForms(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdefABCDEF", 3)[:64]
	for _, tc := range []struct {
		name, pw string
		want     int
	}{
		{"open", "", 202},
		{"7 characters", strings.Repeat("a", 7), 400},
		{"8 characters", strings.Repeat("a", 8), 202},
		{"63 characters", strings.Repeat("a", 63), 202},
		{"64 hex digits", hex64, 202},
		{"64 hex digits upper case", strings.ToUpper(hex64), 202},
		{"64 characters, not hex", strings.Repeat("a", 63) + "g", 400},
		{"64 characters with a space", strings.Repeat("a", 63) + " ", 400},
		{"65 hex digits", hex64 + "0", 400},
		{"63 hex digits", hex64[:63], 202}, // an ordinary passphrase that happens to be hex
	} {
		e := newEnv(t, nil)
		body, _ := json.Marshal(map[string]string{"ssid": "Home", "password": tc.pw})
		resp, b := e.do(t, "POST", "/api/network/wifi", string(body))
		if resp.StatusCode != tc.want {
			t.Errorf("%s: want %d got %d %s", tc.name, tc.want, resp.StatusCode, b)
		}
		if tc.want == 202 && (len(e.net.connected) != 1 || e.net.connected[0] != "Home/"+tc.pw) {
			t.Errorf("%s: Connect not called with the password: %v", tc.name, e.net.connected)
		}
		if tc.want == 400 && len(e.net.connected) != 0 {
			t.Errorf("%s: Connect called for a refused password", tc.name)
		}
	}
}

// R7: usb_devices counts physical devices, not distinct product names.
func TestSystemCountsDevicesNotGroups(t *testing.T) {
	e := newEnv(t, nil) // two devices already: Keys and Synth
	add := func(id string, kind router.PortKind, in, out bool) {
		e.r.AddPort(&tport{info: router.PortInfo{ID: id, Name: id, Kind: kind, Group: "Elektron Digitakt", Input: in, Output: out}})
	}
	count := func() int {
		var sys map[string]any
		e.getJSON(t, "/api/system", &sys)
		return int(sys["usb_devices"].(float64))
	}
	if n := count(); n != 2 {
		t.Fatalf("baseline: %d", n)
	}
	// One Digitakt with two cables, and a second one on another hub port: all four
	// ports share the Group "Elektron Digitakt".
	add("usb:1935:0001:1-1.2:0", router.KindUSB, true, true)
	add("usb:1935:0001:1-1.2:1", router.KindUSB, true, true)
	add("usb:1935:0001:1-1.2:1-0", router.KindUSB, true, true) // further rawmidi device of the same card
	if n := count(); n != 3 {
		t.Fatalf("one device with several cables counts once: got %d", n)
	}
	add("usb:1935:0001:1-1.3:0", router.KindUSB, true, true)
	if n := count(); n != 4 {
		t.Fatalf("two identical devices count twice: got %d", n)
	}
	// IDs of another shape stand for themselves: simulated ports, CoreMIDI's usb:cm-<uid>.
	add("sim:synth", router.KindSim, true, true)
	add("usb:cm-00001234", router.KindUSB, true, true)
	add("usb:cm-00001235", router.KindUSB, true, true)
	if n := count(); n != 7 {
		t.Fatalf("sim and CoreMIDI-style ports: got %d", n)
	}
	// Network and Bluetooth ports are not devices.
	add("rtp:usb:1935:0001:1-1.3:0", router.KindRTP, true, true)
	add("ble:0", router.KindBLE, true, true)
	if n := count(); n != 7 {
		t.Fatalf("network ports must not count: got %d", n)
	}
}

func TestDeviceKey(t *testing.T) {
	for id, want := range map[string]string{
		"usb:1234:abcd:1.2:0":      "usb:1234:abcd:1.2",
		"usb:1234:abcd:SN001:3":    "usb:1234:abcd:SN001",
		"usb:1234:abcd:1-1.2:1-0":  "usb:1234:abcd:1-1.2",
		"usb:cm-00001234":          "usb:cm-00001234",
		"sim:keyboard":             "sim:keyboard",
		"rtp:usb:1234:abcd:1.2:0":  "rtp:usb:1234:abcd:1.2:0",
		"weird":                    "weird",
		"usb:1234:abcd:1.2":        "usb:1234:abcd:1.2",
		"usb:1234:abcd:1.2:0~3":    "usb:1234:abcd:1.2",
		"usb:1234:abcd:ser_ial:00": "usb:1234:abcd:ser_ial",
	} {
		if got := deviceKey(id); got != want {
			t.Errorf("deviceKey(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestNetworkUnmanaged(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Net = nil })
	var n map[string]any
	e.getJSON(t, "/api/network", &n)
	if n["status"].(map[string]any)["mode"] != "unmanaged" {
		t.Fatalf("status: %v", n["status"])
	}
	if resp, _ := e.do(t, "POST", "/api/network/scan", ""); resp.StatusCode != 501 {
		t.Fatal("scan on unmanaged system")
	}
}

func TestSystemAndPower(t *testing.T) {
	e := newEnv(t, nil)
	var sys map[string]any
	e.getJSON(t, "/api/system", &sys)
	if sys["version"] != "test" || sys["allow_power"] != true || sys["backend"] != "fake" || sys["usb_devices"].(float64) != 2 {
		t.Fatalf("system: %v", sys)
	}
	if resp, _ := e.do(t, "POST", "/api/system/reboot", ""); resp.StatusCode != 202 {
		t.Fatalf("reboot: %d", resp.StatusCode)
	}
	select {
	case a := <-e.power:
		if a != "reboot" {
			t.Fatal(a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("power action not performed")
	}

	e2 := newEnv(t, func(d *Deps) { d.AllowPower = false })
	if resp, _ := e2.do(t, "POST", "/api/system/shutdown", ""); resp.StatusCode != 403 {
		t.Fatalf("shutdown must be refused: %d", resp.StatusCode)
	}
}

func TestStaticUI(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>emm</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("// js"), 0o644)
	os.WriteFile(filepath.Join(dir, ".secret"), []byte("nope"), 0o644)
	e := newEnv(t, func(d *Deps) { d.UIDir = dir })
	for path, want := range map[string]int{"/": 200, "/setup": 200, "/index.html": 200, "/app.js": 200,
		"/missing.css": 404, "/.secret": 404, "/api/nope": 404} {
		if resp, _ := e.do(t, "GET", path, ""); resp.StatusCode != want {
			t.Errorf("GET %s: want %d got %d", path, want, resp.StatusCode)
		}
	}
	if _, b := e.do(t, "GET", "/setup", ""); string(b) != "<h1>emm</h1>" {
		t.Errorf("/setup body: %q", b)
	}
	if resp, _ := e.do(t, "POST", "/index.html", ""); resp.StatusCode != 405 {
		t.Errorf("POST static: %d", resp.StatusCode)
	}
}

func TestEmbeddedUIServes(t *testing.T) {
	e := newEnv(t, nil) // no UIDir: embedded copy
	resp, _ := e.do(t, "GET", "/", "")
	// While the UI is being built the embedded dir may hold no index.html; once
	// it does, it must be served.
	if _, err := embeddedUI.Open("ui/index.html"); err == nil && resp.StatusCode != 200 {
		t.Fatalf("embedded index.html not served: %d", resp.StatusCode)
	}
}

// readEvent reads one SSE event (name + data) from r.
func readEvent(t *testing.T, r *bufio.Reader, timeout time.Duration) (string, string) {
	t.Helper()
	type res struct{ name, data string }
	ch := make(chan res, 1)
	go func() {
		var name, data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- res{"error", err.Error()}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				name = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case line == "" && name != "":
				ch <- res{name, data}
				return
			}
		}
	}()
	select {
	case v := <-ch:
		return v.name, v.data
	case <-time.After(timeout):
		t.Fatal("timed out waiting for SSE event")
		return "", ""
	}
}

func TestSSE(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.ts.URL + "/api/events?monitor=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)

	name, data := readEvent(t, rd, 3*time.Second)
	if name != "topology" || !strings.Contains(data, e.a.info.ID) {
		t.Fatalf("first event: %s %s", name, data)
	}

	// A topology change is pushed.
	e.r.AddPort(usb("usb:dddd:0004:1.4:0", "New Box", true, true))
	var sawTopology bool
	deadline := time.Now().Add(3 * time.Second)
	for !sawTopology && time.Now().Before(deadline) {
		n, d := readEvent(t, rd, 3*time.Second)
		if n == "topology" && strings.Contains(d, "New Box") {
			sawTopology = true
		}
	}
	if !sawTopology {
		t.Fatal("no topology event for the new port")
	}

	// Traffic produces activity and monitor events.
	_ = e.r.Connect(e.a.info.ID, e.b.info.ID, router.Filter{})
	h := e.r.AddPort(usb("usb:eeee:0005:1.5:0", "Src", true, false))
	_ = e.r.Connect("usb:eeee:0005:1.5:0", e.b.info.ID, router.Filter{})
	h.Receive([]byte{0x90, 60, 100})
	var sawAct, sawMon bool
	deadline = time.Now().Add(3 * time.Second)
	for !(sawAct && sawMon) && time.Now().Before(deadline) {
		n, d := readEvent(t, rd, 3*time.Second)
		switch n {
		case "activity":
			if strings.Contains(d, `"rx_msgs":1`) {
				sawAct = true
			}
		case "monitor":
			if strings.Contains(d, "Note On ch1 C4 vel 100") {
				sawMon = true
			}
		}
	}
	if !sawAct || !sawMon {
		t.Fatalf("activity=%v monitor=%v", sawAct, sawMon)
	}

	// Settings/network notifications arrive as events too.
	e.srv.NotifySettings()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := readEvent(t, rd, 3*time.Second)
		if n == "settings" {
			return
		}
	}
	t.Fatal("no settings event")
}

// R5: a counter that changes after the first topology snapshot was taken must
// still reach the client as an activity event. (The baseline used to be taken
// after the snapshot, so such a change counted as already delivered.)
//
// Two windows are covered, one hook each: the change lands right after the
// router's snapshot inside buildState (a baseline taken after buildState has
// returned, say, "initial := buildState(); last := Stats()", would miss it), and
// it lands after buildState has returned but before the topology event is sent
// (a baseline taken even later would miss that).
func TestSSEInitialBaselineCoversSnapshotWindow(t *testing.T) {
	windows := []struct {
		name string
		hook func(e *env, fn func())
	}{
		{"right after the router snapshot", func(e *env, fn func()) { e.srv.testHookSnapshotTaken = fn }},
		{"after the state is built", func(e *env, fn func()) { e.srv.testHookInitialState = fn }},
	}
	for _, w := range windows {
		t.Run(w.name, func(t *testing.T) {
			e := newEnv(t, nil)
			src := e.r.AddPort(usb("usb:eeee:0005:1.5:0", "Src", true, false))
			var fired atomic.Int32
			w.hook(e, func() {
				if fired.Add(1) == 1 {
					src.Receive([]byte{0x90, 60, 100}) // after the snapshot, before it is sent
				}
			})
			resp, err := http.Get(e.ts.URL + "/api/events")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			rd := bufio.NewReader(resp.Body)

			name, data := readEvent(t, rd, 3*time.Second)
			if name != "topology" {
				t.Fatalf("first event: %s %s", name, data)
			}
			var topo stateBody
			if err := json.Unmarshal([]byte(data), &topo); err != nil {
				t.Fatal(err)
			}
			for _, p := range topo.Ports {
				if p.ID == "usb:eeee:0005:1.5:0" && p.Stats.RxMsgs != 0 {
					t.Fatalf("test setup: the snapshot already contains the message (rx_msgs=%d)", p.Stats.RxMsgs)
				}
			}
			if fired.Load() != 1 {
				t.Fatal("test setup: hook did not run")
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				n, d := readEvent(t, rd, 3*time.Second)
				if n != "activity" {
					continue
				}
				var act map[string]activityPayload
				if err := json.Unmarshal([]byte(d), &act); err != nil {
					t.Fatal(err)
				}
				if a, ok := act["usb:eeee:0005:1.5:0"]; ok && a.RxMsgs == 1 {
					return
				}
			}
			t.Fatal("the change made after the snapshot never reached the client")
		})
	}
}

func b64(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	b := []byte(s)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		for j := 0; j < 3; j++ {
			n <<= 8
			if i+j < len(b) {
				n |= uint32(b[i+j])
			}
		}
		out = append(out, tbl[n>>18&63], tbl[n>>12&63])
		if rem > 1 {
			out = append(out, tbl[n>>6&63])
		} else {
			out = append(out, '=')
		}
		if rem > 2 {
			out = append(out, tbl[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

func TestSettingsManagedByBootFile(t *testing.T) {
	off := false
	e := newEnv(t, func(d *Deps) { d.BootRTP = &off; d.BootBLEName = "FromFile"; d.BootWebPassword = "pw" })
	auth := "Basic " + b64("u:pw")
	resp, b := e.do(t, "GET", "/api/settings", "", "Authorization", auth)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d %s", resp.StatusCode, b)
	}
	var s settingsBody
	_ = json.Unmarshal(b, &s)
	if !s.Managed.RTP || !s.Managed.BLEName || !s.Managed.WebPassword || s.Managed.BLE {
		t.Fatalf("managed flags: %+v", s.Managed)
	}
	if !strings.Contains(string(b), `"managed":{"rtp":true,"ble":false,"ble_name":true,"web_password":true}`) {
		t.Fatalf("wire shape: %s", b)
	}
	for _, body := range []string{`{"rtp":{"enabled":true}}`, `{"ble":{"name":"Other"}}`, `{"web":{"password":"x"}}`} {
		resp, b := e.do(t, "PUT", "/api/settings", body, "Authorization", auth)
		if resp.StatusCode != 409 || !strings.Contains(string(b), "emm.txt") {
			t.Errorf("%s: want 409 naming emm.txt, got %d %s", body, resp.StatusCode, b)
		}
	}
	if e.applied.Load() != 0 {
		t.Error("a refused PUT must not apply anything")
	}
	// A setting emm.txt does not fix stays editable.
	resp, b = e.do(t, "PUT", "/api/settings", `{"ble":{"enabled":false}}`, "Authorization", auth)
	if resp.StatusCode != 200 {
		t.Errorf("unmanaged setting refused: %d %s", resp.StatusCode, b)
	}
}

func TestSetupPathTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>emm</h1>"), 0o644)
	e := newEnv(t, func(d *Deps) { d.UIDir = dir })
	for _, p := range []string{"/setup", "/setup/", "/index.html"} {
		if resp, _ := e.do(t, "GET", p, ""); resp.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
}
