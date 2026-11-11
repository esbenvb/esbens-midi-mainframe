package web

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"emm/internal/config"
	"emm/internal/router"
)

// secretsFor puts recognisable secrets in the store and returns the header that
// logs in with the web password.
func secretsFor(e *env) (auth []string, secrets []string) {
	e.store.Update(func(st *config.State) {
		st.SetWebPassword("webpw")
		st.SSH.Enabled = true
		st.SSH.PasswordHash = "$6$saltsalt$hashhashhash"
		st.SSH.AuthorizedKeys = []string{"ssh-ed25519 AAAAKEYKEYKEY me@laptop"}
		st.Network = json.RawMessage(`{"wifi_ssid":"HomeNet","wifi_password":"hunter2hunter2"}`)
	})
	snap := e.store.Snapshot()
	return []string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":webpw"))},
		[]string{"hunter2hunter2", "HomeNet", "$6$saltsalt$hashhashhash", "AAAAKEYKEYKEY", snap.Web.Hash, snap.Web.Salt}
}

func TestBackupHasTheApplicationStateAndNoSecrets(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	e.r.Connect(a, b, router.Filter{Block: []string{"clock"}, Transpose: 2})
	e.store.Update(func(st *config.State) {
		st.Ports[b] = config.PortSettings{Alias: "Lead synth", Hidden: true}
		st.Presets = []config.Preset{{ID: "p-live", Name: "Live", Saved: 5, Connections: []router.ConnSpec{{From: a, To: b, Filter: router.DefaultFilter()}}}}
		st.BLE.Name = "Stage"
	})
	auth, secrets := secretsFor(e)

	resp, body := e.do(t, "GET", "/api/backup", "", auth...)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="emm-backup-emm-`) || !strings.HasSuffix(cd, `.json"`) {
		t.Errorf("Content-Disposition: %q", cd)
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", resp.Header)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(string(body), s) {
			t.Errorf("the backup contains %q", s)
		}
	}
	var bf struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
		Host    string `json:"host"`
		State   map[string]json.RawMessage
	}
	if err := json.Unmarshal(body, &bf); err != nil {
		t.Fatal(err)
	}
	if bf.Format != "emm-backup" || bf.Version != 1 || bf.Host != "emm" {
		t.Fatalf("envelope: %+v", bf)
	}
	for _, k := range []string{"connections", "ports", "known", "presets", "ble"} {
		if _, ok := bf.State[k]; !ok {
			t.Errorf("state lacks %q", k)
		}
	}
	for _, k := range []string{"web", "ssh", "network"} {
		if _, ok := bf.State[k]; ok {
			t.Errorf("state has %q", k)
		}
	}
	// A backup needs the password like everything else.
	if resp, _ := e.do(t, "GET", "/api/backup", ""); resp.StatusCode != 401 {
		t.Errorf("without the password: %d", resp.StatusCode)
	}
}

func TestRestoreBringsBackTheRoutingAndLeavesTheSecretsAlone(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	f := router.Filter{Block: []string{"clock"}, Transpose: 2}
	e.r.Connect(a, b, f)
	e.store.Update(func(st *config.State) {
		st.Ports[b] = config.PortSettings{Alias: "Lead synth", Hidden: true}
		st.Known[a] = config.KnownPort{Name: "Keys", Kind: router.KindUSB, Input: true}
		st.Known[b] = config.KnownPort{Name: "Synth", Kind: router.KindUSB, Input: true, Output: true}
		st.Presets = []config.Preset{{ID: "p-live", Name: "Live", Connections: []router.ConnSpec{{From: a, To: b, Filter: router.DefaultFilter()}}}}
	})
	auth, _ := secretsFor(e)
	_, backup := e.do(t, "GET", "/api/backup", "", auth...)

	// Mess everything up, including the secrets.
	e.r.Disconnect(a, b)
	e.r.Connect(b, "usb:zzzz:0009:1.9:0", router.Filter{})
	e.store.Update(func(st *config.State) {
		st.Ports = map[string]config.PortSettings{}
		st.Presets = nil
		st.Known["usb:zzzz:0009:1.9:0"] = config.KnownPort{Name: "Other", Kind: router.KindUSB, Output: true}
		st.Network = json.RawMessage(`{"wifi_ssid":"Changed"}`)
	})
	secretsBefore := e.store.Snapshot()

	// A crafted backup that also carries secrets must not set them.
	var doc map[string]json.RawMessage
	json.Unmarshal(backup, &doc)
	var state map[string]json.RawMessage
	json.Unmarshal(doc["state"], &state)
	state["network"] = json.RawMessage(`{"wifi_ssid":"Evil","wifi_password":"pwned"}`)
	state["web"] = json.RawMessage(`{"salt":"x","hash":"evil"}`)
	state["ssh"] = json.RawMessage(`{"enabled":true,"password_hash":"$6$evil$evil"}`)
	doc["state"], _ = json.Marshal(state)
	crafted, _ := json.Marshal(doc)

	resp, body := e.do(t, "POST", "/api/restore", string(crafted), auth...)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var res struct {
		Connections int           `json:"connections"`
		Skipped     []skippedConn `json:"skipped"`
		Presets     int           `json:"presets"`
		Warnings    []string      `json:"warnings"`
	}
	json.Unmarshal(body, &res)
	if res.Connections != 1 || len(res.Skipped) != 0 || res.Presets != 1 || len(res.Warnings) != 0 {
		t.Fatalf("result: %+v", res)
	}

	got := connSet(e.r)
	if len(got) != 1 {
		t.Fatalf("connections: %v", got)
	}
	if g, ok := got[a+" > "+b]; !ok || g.Transpose != 2 || len(g.Block) != 1 {
		t.Fatalf("the connection and its filter did not come back: %v", got)
	}
	after := e.store.Snapshot()
	if after.Ports[b].Alias != "Lead synth" || !after.Ports[b].Hidden || len(after.Presets) != 1 || after.Presets[0].Name != "Live" {
		t.Fatalf("application state not restored: %+v %+v", after.Ports, after.Presets)
	}
	if _, ok := after.Known["usb:zzzz:0009:1.9:0"]; ok {
		t.Error("a device that is not in the backup is still remembered")
	}
	if string(after.Network) != string(secretsBefore.Network) || after.Web != secretsBefore.Web || after.SSH.PasswordHash != secretsBefore.SSH.PasswordHash {
		t.Fatalf("a restore changed the device's own settings: %s %+v %+v", after.Network, after.Web, after.SSH)
	}
	// And the restored state is what gets saved.
	if err := e.store.Flush(); err != nil {
		t.Fatal(err)
	}
	if e.reconciled.Load() == 0 || e.applied.Load() == 0 {
		t.Errorf("reconcile %d, apply settings %d: a restore must trigger both", e.reconciled.Load(), e.applied.Load())
	}
}

func TestRestoreRefusesFilesThatAreNotBackups(t *testing.T) {
	e := newEnv(t, nil)
	e.r.Connect(e.a.info.ID, e.b.info.ID, router.DefaultFilter())
	before := connSet(e.r)

	huge := `{"format":"emm-backup","version":1,"state":{"padding":"` + strings.Repeat("x", maxBackupBytes) + `"}}`
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"not json":          {`not json`, 400},
		"empty object":      {`{}`, 400},
		"wrong format":      {`{"format":"something","version":1,"state":{}}`, 400},
		"newer format":      {`{"format":"emm-backup","version":2,"state":{}}`, 400},
		"no state":          {`{"format":"emm-backup","version":1}`, 400},
		"state is a string": {`{"format":"emm-backup","version":1,"state":"x"}`, 400},
		"state is null":     {`{"format":"emm-backup","version":1,"state":null}`, 400},
		"newer settings":    {`{"format":"emm-backup","version":1,"state":{"version":99}}`, 400},
		"wrong types":       {`{"format":"emm-backup","version":1,"state":{"ports":[1,2]}}`, 400},
		"too large":         {huge, 413},
	} {
		resp, body := e.do(t, "POST", "/api/restore", c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", name, resp.StatusCode, body, c.want)
		}
	}
	// Nothing changed.
	after := connSet(e.r)
	if len(after) != len(before) {
		t.Fatalf("a refused restore changed the connections: %v -> %v", before, after)
	}
	if e.reconciled.Load() != 0 {
		t.Error("a refused restore reconciled")
	}
}

func TestRestoreSanitisesWhatItReads(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	state := map[string]any{
		"version": 1,
		"known": map[string]any{
			a:                    map[string]any{"name": "Keys", "kind": "usb", "input": true},
			b:                    map[string]any{"name": "Synth", "kind": "usb", "input": true, "output": true},
			"bad id with spaces": map[string]any{"name": "x"},
		},
		"ports": map[string]any{
			b:                  map[string]any{"alias": strings.Repeat("a", 500), "rtp_port": 99999},
			"../../etc/passwd": map[string]any{"alias": "x"},
		},
		"connections": []any{
			map[string]any{"from": a, "to": b, "filter": map[string]any{}},
			map[string]any{"from": a, "to": b, "filter": map[string]any{}},                      // duplicate
			map[string]any{"from": a, "to": a, "filter": map[string]any{}},                      // loop
			map[string]any{"from": "no good", "to": b, "filter": map[string]any{}},              // bad id
			map[string]any{"from": b, "to": a, "filter": map[string]any{"transpose": 9999}},     // bad filter
			map[string]any{"from": "usb:never:0000:1.1:0", "to": b, "filter": map[string]any{}}, // unknown device
		},
		"presets": []any{
			map[string]any{"id": "p-a", "name": "One", "connections": []any{map[string]any{"from": a, "to": b, "filter": map[string]any{}}}},
			map[string]any{"id": "p-b", "name": "one"}, // same name, other case
			map[string]any{"id": "p-c", "name": ""},    // no name
			map[string]any{"id": "bad id", "name": "Two"},
		},
		"virtual_rtp": []any{
			map[string]any{"id": "rtp:net-ok", "name": "Laptop"},
			map[string]any{"id": "usb:not-rtp", "name": "x"},
		},
		"ble": map[string]any{"name": strings.Repeat("n", 60)},
	}
	raw, _ := json.Marshal(map[string]any{"format": "emm-backup", "version": 1, "state": state})
	resp, body := e.do(t, "POST", "/api/restore", string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var res struct {
		Connections int           `json:"connections"`
		Skipped     []skippedConn `json:"skipped"`
		Presets     int           `json:"presets"`
		Warnings    []string      `json:"warnings"`
	}
	json.Unmarshal(body, &res)
	if res.Connections != 1 || len(res.Skipped) != 1 || res.Presets != 2 || len(res.Warnings) == 0 {
		t.Fatalf("result: %+v", res)
	}
	st := e.store.Snapshot()
	if _, ok := st.Known["bad id with spaces"]; ok {
		t.Error("an invalid id was kept")
	}
	if _, ok := st.Ports["../../etc/passwd"]; ok {
		t.Error("an invalid id was kept in the port settings")
	}
	if p := st.Ports[b]; len([]rune(p.Alias)) != 60 || p.RTPPort != 0 {
		t.Errorf("alias or port not cleaned: %d runes, port %d", len([]rune(p.Alias)), p.RTPPort)
	}
	if len(st.Presets) != 2 || st.Presets[0].Name != "One" || st.Presets[1].Name != "Two" || st.Presets[1].ID == "bad id" {
		t.Errorf("presets: %+v", st.Presets)
	}
	if len(st.VirtualRTP) != 1 || st.VirtualRTP[0].ID != "rtp:net-ok" {
		t.Errorf("virtual: %+v", st.VirtualRTP)
	}
	if len([]rune(st.BLE.Name)) != 20 {
		t.Errorf("BLE name: %q", st.BLE.Name)
	}
}

// The ids in a backup from another unit are unknown here, and their connections are skipped.
func TestRestoreFromAnotherUnitSkipsUnknownDevices(t *testing.T) {
	e := newEnv(t, nil)
	other := "usb:1111:2222:1-1.4:0"
	state := map[string]any{
		"known":       map[string]any{other: map[string]any{"name": "Their synth", "kind": "usb", "input": true, "output": true}},
		"connections": []any{map[string]any{"from": other, "to": e.b.info.ID, "filter": map[string]any{}}},
	}
	raw, _ := json.Marshal(map[string]any{"format": "emm-backup", "version": 1, "state": state})
	resp, body := e.do(t, "POST", "/api/restore", string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var res struct {
		Connections int `json:"connections"`
	}
	json.Unmarshal(body, &res)
	// The device is in the backup's own list of remembered devices, so its
	// connection is kept (dormant until it is plugged in).
	if res.Connections != 1 {
		t.Fatalf("result: %s", body)
	}
}

func TestRestoreRejectsAwkwardIDsKindsAndPorts(t *testing.T) {
	e := newEnv(t, nil)
	a := e.a.info.ID
	state := map[string]any{
		"known": map[string]any{
			a:                    map[string]any{"name": "Keys", "kind": "usb", "input": true},
			"..":                 map[string]any{"name": "dots", "kind": "usb"},
			"usb:odd:0001:1.1:0": map[string]any{"name": "odd kind", "kind": strings.Repeat("k", 1000)},
		},
		"ports": map[string]any{
			a:                   map[string]any{"rtp_port": 53},
			".":                 map[string]any{"alias": "dot"},
			"usb:hi:0001:1.1:0": map[string]any{"rtp_port": 65535},
		},
		"presets": []any{map[string]any{"id": "..", "name": "Dots"}},
		"virtual_rtp": []any{
			map[string]any{"id": "rtp:net-laptop", "name": "Laptop", "port": 5010},
			map[string]any{"id": "rtp:" + a, "name": "Takes over a device endpoint"},
			map[string]any{"id": "rtp:net-UPPER", "name": "Not what the UI makes"},
			map[string]any{"id": "rtp:net-x", "name": "Low port", "port": 80},
		},
	}
	raw, _ := json.Marshal(map[string]any{"format": "emm-backup", "version": 1, "state": state})
	resp, body := e.do(t, "POST", "/api/restore", string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	st := e.store.Snapshot()
	if _, ok := st.Known[".."]; ok {
		t.Error("an id of '..' was kept")
	}
	if _, ok := st.Known["usb:odd:0001:1.1:0"]; ok {
		t.Error("an entry with an unknown kind was kept")
	}
	if _, ok := st.Ports["."]; ok {
		t.Error("an id of '.' was kept")
	}
	if st.Ports[a].RTPPort != 0 || st.Ports["usb:hi:0001:1.1:0"].RTPPort != 0 {
		t.Errorf("ports outside the allocator's range were kept: %+v", st.Ports)
	}
	if len(st.Presets) != 1 || st.Presets[0].ID == ".." {
		t.Errorf("presets: %+v", st.Presets)
	}
	if len(st.VirtualRTP) != 2 || st.VirtualRTP[0].Port != 5010 || st.VirtualRTP[1].Port != 0 {
		t.Errorf("virtual ports: %+v", st.VirtualRTP)
	}
	var res struct {
		Saved bool `json:"saved"`
	}
	json.Unmarshal(body, &res)
	if !res.Saved {
		t.Errorf("a restore on a working card must report saved: %s", body)
	}
}

// A file cannot make the daemon carry a huge routing: the lists share one budget.
func TestRestoreHasAnOverallConnectionBudget(t *testing.T) {
	e := newEnv(t, nil)
	var conns []any
	for i := 0; i < 900; i++ {
		conns = append(conns, map[string]any{"from": "usb:src:" + strings.Repeat("0", 3) + ":1." + strings.Repeat("1", 1+i%7) + ":" + string(rune('a'+i%26)), "to": "usb:dst:0000:1." + itoa(i) + ":0", "filter": map[string]any{}})
	}
	var presets []any
	for p := 0; p < 10; p++ {
		presets = append(presets, map[string]any{"name": "P" + itoa(p), "connections": conns})
	}
	raw, _ := json.Marshal(map[string]any{"format": "emm-backup", "version": 1, "state": map[string]any{"presets": presets, "connections": conns}})
	resp, body := e.do(t, "POST", "/api/restore", string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %.200s", resp.StatusCode, body)
	}
	total := 0
	for _, p := range e.store.Snapshot().Presets {
		total += len(p.Connections)
	}
	if total > maxBackupBudget {
		t.Fatalf("%d preset connections kept, budget %d", total, maxBackupBudget)
	}
	if !strings.Contains(string(body), "over the limit") {
		t.Errorf("the cut must be reported: %.300s", body)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// Restores and recalls at the same time leave one coherent routing (and no data race).
func TestConcurrentRestoresAreSerialised(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	e.store.Update(func(st *config.State) {
		st.Known[a] = config.KnownPort{Name: "Keys", Kind: router.KindUSB, Input: true}
		st.Known[b] = config.KnownPort{Name: "Synth", Kind: router.KindUSB, Input: true, Output: true}
	})
	mk := func(filter map[string]any) string {
		raw, _ := json.Marshal(map[string]any{"format": "emm-backup", "version": 1, "state": map[string]any{
			"known":       map[string]any{a: map[string]any{"name": "Keys", "kind": "usb", "input": true}, b: map[string]any{"name": "Synth", "kind": "usb", "input": true, "output": true}},
			"connections": []any{map[string]any{"from": a, "to": b, "filter": filter}},
		}})
		return string(raw)
	}
	bodies := []string{mk(map[string]any{"transpose": 1}), mk(map[string]any{"transpose": 2}), mk(map[string]any{"transpose": 3})}
	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.do(t, "POST", "/api/restore", bodies[i%3])
		}(i)
	}
	wg.Wait()
	got := connSet(e.r)
	if len(got) != 1 {
		t.Fatalf("connections: %v", got)
	}
}
