package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"emm/internal/router"
)

func (e *env) postRaw(t *testing.T, path, ctype string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func waitData(t *testing.T, p *tport, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d := p.data(); len(d) >= n {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("device got %d bytes, wanted %d", len(p.data()), n)
	return nil
}

func TestConsoleSend(t *testing.T) {
	e := newEnv(t, nil)
	net := &tport{info: router.PortInfo{ID: "rtp:net-x", Name: "Net", Kind: router.KindRTP, Output: true}}
	e.r.AddPort(net)
	b := "/api/ports/" + e.b.info.ID + "/send"

	resp, body := e.do(t, "POST", b, `{"data":[144,60,100]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("send: %d %s", resp.StatusCode, body)
	}
	if got := waitData(t, e.b, 3); !bytes.Equal(got, []byte{0x90, 60, 100}) {
		t.Fatalf("device got % X", got)
	}
	// Several messages, a SysEx among them.
	resp, _ = e.do(t, "POST", b, `{"data":[176,7,100,240,125,1,247,250]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("multi: %d", resp.StatusCode)
	}
	waitData(t, e.b, 3+8)

	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"network port":    {"/api/ports/rtp:net-x/send", `{"data":[250]}`, 403},
		"input-only port": {"/api/ports/" + e.a.info.ID + "/send", `{"data":[250]}`, 400},
		"unknown port":    {"/api/ports/usb:nope/send", `{"data":[250]}`, 404},
		"empty":           {b, `{"data":[]}`, 400},
		"incomplete":      {b, `{"data":[144,60]}`, 400},
		"byte over 255":   {b, `{"data":[256]}`, 400},
		"negative":        {b, `{"data":[-1]}`, 400},
		"open sysex":      {b, `{"data":[240,1,2]}`, 400},
		"not JSON":        {b, `nope`, 400},
		"high data byte":  {b, `{"data":[144,200,100]}`, 400},
		"far too much":    {b, `{"data":[` + strings.Repeat("248,", maxConsoleBytes) + `248]}`, 400},
	} {
		resp, body := e.do(t, "POST", tc.path, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: want %d, got %d %s", name, tc.want, resp.StatusCode, body)
		}
	}
	if n := len(net.data()); n != 0 {
		t.Errorf("the network port was sent %d bytes", n)
	}
}

func TestSysexUpload(t *testing.T) {
	e := newEnv(t, nil)
	net := &tport{info: router.PortInfo{ID: "ble:x", Name: "BLE", Kind: router.KindBLE, Output: true}}
	e.r.AddPort(net)
	path := "/api/ports/" + e.b.info.ID + "/sysex"
	m1 := []byte{0xF0, 0x7D, 1, 2, 3, 0xF7}
	m2 := append([]byte{0xF0, 0x7D}, bytes.Repeat([]byte{9}, 700)...)
	m2 = append(m2, 0xF7)
	file := append(append([]byte{}, m1...), m2...)

	start := time.Now()
	resp, body := e.postRaw(t, path+"?gap_ms=40", "application/octet-stream", file)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	var out struct{ Messages, Bytes int }
	if err := json.Unmarshal(body, &out); err != nil || out.Messages != 2 || out.Bytes != len(file) {
		t.Fatalf("answer %s (%v)", body, err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("the gap between messages was not kept: %v", time.Since(start))
	}
	if got := e.b.data(); !bytes.Equal(got, file) {
		t.Fatalf("device got %d bytes, want %d", len(got), len(file))
	}

	for name, tc := range map[string]struct {
		path, ctype string
		body        []byte
		want        int
	}{
		"text/plain":      {path, "text/plain", file, 415},
		"no type":         {path, "", file, 415},
		"not sysex":       {path, "application/octet-stream", []byte{0x90, 60, 100}, 400},
		"cut off":         {path, "application/octet-stream", file[:10], 400},
		"empty":           {path, "application/octet-stream", nil, 400},
		"bad gap":         {path + "?gap_ms=-1", "application/octet-stream", m1, 400},
		"gap too long":    {path + "?gap_ms=999999", "application/octet-stream", m1, 400},
		"network port":    {"/api/ports/ble:x/sysex", "application/octet-stream", m1, 403},
		"input-only port": {"/api/ports/" + e.a.info.ID + "/sysex", "application/octet-stream", m1, 400},
		"unknown port":    {"/api/ports/usb:nope/sysex", "application/octet-stream", m1, 404},
		"too large":       {path, "application/octet-stream", append([]byte{0xF0}, make([]byte, maxSysexUpload)...), 413},
	} {
		resp, body := e.postRaw(t, tc.path, tc.ctype, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: want %d, got %d %s", name, tc.want, resp.StatusCode, body)
		}
	}
	if len(net.data()) != 0 {
		t.Error("the Bluetooth port was sent data")
	}
	// Raw bytes are accepted for the SysEx upload and nowhere else.
	resp, _ = e.postRaw(t, "/api/ports/"+e.b.info.ID+"/send", "application/octet-stream", []byte(`{"data":[250]}`))
	if resp.StatusCode != 415 {
		t.Errorf("octet-stream to /send: %d", resp.StatusCode)
	}
}

// gatedPort is a wired device that takes data only when the test lets it.
type gatedPort struct {
	tport
	gate chan struct{}
}

func (g *gatedPort) Write(b []byte) error { <-g.gate; return g.tport.Write(b) }

func TestSysexUploadOneAtATime(t *testing.T) {
	e := newEnv(t, nil)
	g := &gatedPort{tport: *usb("usb:gate:0001:1.4:0", "Gated", true, true), gate: make(chan struct{})}
	e.r.AddPort(g)
	path := "/api/ports/" + g.info.ID + "/sysex"
	msg := []byte{0xF0, 0x7D, 1, 0xF7}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, body := e.postRaw(t, path, "application/octet-stream", msg)
		if resp.StatusCode != 200 {
			t.Errorf("first upload: %d %s", resp.StatusCode, body)
		}
	}()
	time.Sleep(150 * time.Millisecond) // it is now waiting for the device
	resp, body := e.postRaw(t, path, "application/octet-stream", msg)
	if resp.StatusCode != 409 {
		t.Errorf("second upload while one runs: %d %s", resp.StatusCode, body)
	}
	g.gate <- struct{}{}
	wg.Wait()
	// Free again afterwards.
	go func() { g.gate <- struct{}{} }()
	if resp, body := e.postRaw(t, path, "application/octet-stream", msg); resp.StatusCode != 200 {
		t.Errorf("upload after the first finished: %d %s", resp.StatusCode, body)
	}
}

type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
}

// next returns the next event's name and data.
func (r *sseReader) next() (string, string) {
	var ev, data string
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && ev != "":
			return ev, data
		}
	}
	r.t.Fatal("event stream ended")
	return "", ""
}

func TestSysexListen(t *testing.T) {
	e := newEnv(t, nil)
	h := e.r.AddPort(usb("usb:dump:0001:1.5:0", "Dumper", true, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/ports/usb:dump:0001:1.5:0/sysex/listen", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("listen: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := &sseReader{t: t, sc: bufio.NewScanner(resp.Body)}
	if ev, _ := rd.next(); ev != "ready" {
		t.Fatalf("first event %q", ev)
	}
	msg := append([]byte{0xF0, 0x43, 0x10}, bytes.Repeat([]byte{7}, 600)...)
	msg = append(msg, 0xF7)
	h.Receive([]byte{0x90, 60, 100}) // not SysEx: not reported
	h.Receive(msg)
	for {
		ev, data := rd.next()
		if ev == "progress" {
			continue
		}
		if ev != "sysex" {
			t.Fatalf("event %q %s", ev, data)
		}
		var d struct {
			Data     string
			Bytes    int
			Complete bool
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(d.Data)
		if !bytes.Equal(raw, msg) || !d.Complete || d.Bytes != len(msg) {
			t.Fatalf("got %d bytes complete=%v, want %d", len(raw), d.Complete, len(msg))
		}
		break
	}
	// Unplugging ends the stream with a "closed" event.
	e.r.RemovePort("usb:dump:0001:1.5:0")
	for {
		ev, _ := rd.next()
		if ev == "closed" {
			break
		}
	}
}

func TestSysexListenRefusals(t *testing.T) {
	e := newEnv(t, nil)
	e.r.AddPort(&tport{info: router.PortInfo{ID: "rtp:net-y", Name: "Net", Kind: router.KindRTP, Input: true, Output: true}})
	for path, want := range map[string]int{
		"/api/ports/rtp:net-y/sysex/listen":           403,
		"/api/ports/" + e.b.info.ID + "/sysex/listen": 200, // a wired port with an input
		"/api/ports/usb:nope/sysex/listen":            404,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Errorf("%s: want %d got %d", path, want, resp.StatusCode)
		}
		resp.Body.Close()
		cancel()
	}
	// An output-only wired port has nothing to listen to.
	e.r.AddPort(usb("usb:outonly:0001:1.6:0", "Out", false, true))
	resp, _ := e.do(t, "GET", "/api/ports/usb:outonly:0001:1.6:0/sysex/listen", "")
	if resp.StatusCode != 400 {
		t.Errorf("output-only port: %d", resp.StatusCode)
	}
}
