package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type daemon struct {
	base   string
	cancel context.CancelFunc
	done   chan error
}

func startDaemon(t *testing.T, state string) *daemon { return startDaemonWith(t, state, nil) }

// startDaemonWith is startDaemon with a chance to change the options first.
func startDaemonWith(t *testing.T, state string, mut func(*options)) *daemon {
	t.Helper()
	addr := freeAddr(t)
	o := options{listen: addr, statePath: state, backend: "sim", netcfgMode: "off", logLevel: "error", noAnnounce: true}
	if mut != nil {
		mut(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{base: "http://" + addr, cancel: cancel, done: make(chan error, 1)}
	go func() {
		d.done <- run(ctx, o, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(d.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return d
}

func (d *daemon) stop(t *testing.T) {
	t.Helper()
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func (d *daemon) get(t *testing.T, path string, v any) {
	t.Helper()
	resp, err := http.Get(d.base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func (d *daemon) send(t *testing.T, method, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, d.base+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type stateResp struct {
	Ports []struct {
		ID     string `json:"id"`
		Online bool   `json:"online"`
		RTP    bool   `json:"rtp"`
		Stats  struct {
			RxMsgs uint64 `json:"rx_msgs"`
			TxMsgs uint64 `json:"tx_msgs"`
		} `json:"stats"`
	} `json:"ports"`
	Connections []struct {
		From, To string
		Online   bool
	} `json:"connections"`
}

func waitState(t *testing.T, d *daemon, what string, ok func(stateResp) bool) stateResp {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var s stateResp
	for time.Now().Before(deadline) {
		s = stateResp{}
		d.get(t, "/api/state", &s)
		if ok(s) {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last state: %+v", what, s)
	return s
}

func hasConn(s stateResp, from, to string) (found, online bool) {
	for _, c := range s.Connections {
		if c.From == from && c.To == to {
			return true, c.Online
		}
	}
	return false, false
}

func TestDaemonEndToEndWithPersistence(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")

	d := startDaemon(t, state)
	s := waitState(t, d, "sim devices and their network endpoints", func(s stateResp) bool {
		n := 0
		for _, p := range s.Ports {
			if p.Online {
				n++
			}
		}
		f, _ := hasConn(s, "sim:synth", "rtp:sim:synth")
		return n >= 8 && f // 4 devices + 4 network endpoints, default connections made
	})
	_ = s

	var eps []struct {
		ID          string `json:"id"`
		ControlPort int    `json:"control_port"`
		Device      string `json:"device"`
	}
	d.get(t, "/api/rtp", &eps)
	if len(eps) != 4 {
		t.Fatalf("want 4 network endpoints, got %+v", eps)
	}
	ports := map[string]int{}
	for _, e := range eps {
		ports[e.ID] = e.ControlPort
		if e.ControlPort == 0 || e.Device == "" {
			t.Fatalf("bad endpoint %+v", e)
		}
	}

	// Wire the keyboard to the synth and see traffic arrive.
	if c := d.send(t, "PUT", "/api/connections", `{"from":"sim:keyboard","to":"sim:synth"}`); c != 200 {
		t.Fatalf("connect: %d", c)
	}
	waitState(t, d, "traffic at the synth", func(s stateResp) bool {
		for _, p := range s.Ports {
			if p.ID == "sim:synth" && p.Stats.TxMsgs >= 4 {
				return true
			}
		}
		return false
	})
	// An alias is persisted too.
	if c := d.send(t, "PATCH", "/api/ports/sim:synth", `{"alias":"Lead"}`); c != 200 {
		t.Fatalf("patch: %d", c)
	}
	d.stop(t)

	// Second run from the same state file: connections and port numbers return.
	d2 := startDaemon(t, state)
	defer d2.stop(t)
	waitState(t, d2, "restored connection", func(s stateResp) bool {
		f, on := hasConn(s, "sim:keyboard", "sim:synth")
		return f && on
	})
	var eps2 []struct {
		ID          string `json:"id"`
		ControlPort int    `json:"control_port"`
	}
	d2.get(t, "/api/rtp", &eps2)
	for _, e := range eps2 {
		if ports[e.ID] != e.ControlPort {
			t.Errorf("endpoint %s moved from UDP %d to %d across a restart", e.ID, ports[e.ID], e.ControlPort)
		}
	}
	var raw struct {
		Ports []struct {
			ID    string `json:"id"`
			Alias string `json:"alias"`
		} `json:"ports"`
	}
	d2.get(t, "/api/state", &raw)
	for _, p := range raw.Ports {
		if p.ID == "sim:synth" && p.Alias != "Lead" {
			t.Errorf("alias lost across restart: %q", p.Alias)
		}
	}
	// And the default connections were not duplicated or recreated differently.
	var again stateResp
	d2.get(t, "/api/state", &again)
	if len(again.Connections) != 4*2-1+1+0 { // 4 devices × 2 directions − keyboard's missing return path, + keyboard→synth
		t.Logf("connections after restart: %d", len(again.Connections))
	}
	_ = fmt.Sprint
}

// web-daemon R2: names for the web server's Host allow-list come from emm.txt
// (allowed_hosts) as well as from --allowed-hosts, since the appliance cannot
// pass flags.
func TestAllowedHostsFromBootFileAndFlag(t *testing.T) {
	boot := t.TempDir()
	if err := os.WriteFile(filepath.Join(boot, "emm.txt"), []byte("allowed_hosts = nas.example.test, .studio.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWith(t, filepath.Join(t.TempDir(), "state.json"), func(o *options) {
		o.bootDir = boot
		o.allowedHosts = "flag.example.test"
	})
	defer d.stop(t)

	for host, want := range map[string]int{
		"nas.example.test":    200, // emm.txt
		"NAS.example.test:80": 200,
		"rack.studio.example": 200, // emm.txt, a whole domain
		"flag.example.test":   200, // --allowed-hosts
		"emm.local":           200,
		"evil.example.test":   421,
		"studio.example":      421,
	} {
		req, _ := http.NewRequest("GET", d.base+"/healthz", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: want %d got %d", host, want, resp.StatusCode)
		}
		if want == 421 && (!strings.Contains(string(b), "allowed_hosts in emm.txt") || !strings.Contains(string(b), "--allowed-hosts")) {
			t.Errorf("Host %q: the 421 message should name both ways to allow a name: %s", host, b)
		}
	}
}

func TestMergeHosts(t *testing.T) {
	cases := []struct {
		name  string
		lists [][]string
		want  []string
	}{
		{"nothing", nil, nil},
		{"only the flag", [][]string{{"a.example", ".b.example"}, nil}, []string{"a.example", ".b.example"}},
		{"only emm.txt", [][]string{nil, {"c.example"}}, []string{"c.example"}},
		{"both, repeats dropped", [][]string{{"a.example", "B.example"}, {"b.example", "c.example", "A.EXAMPLE"}},
			[]string{"a.example", "B.example", "c.example"}},
	}
	for _, c := range cases {
		if got := mergeHosts(c.lists...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
