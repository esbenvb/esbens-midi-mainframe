package web

import (
	"encoding/json"
	"strings"
	"testing"

	"emm/internal/config"
	"emm/internal/router"
)

func TestPresetsSaveRecallRenameDelete(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	f := router.Filter{Block: []string{"clock"}, Transpose: 3}
	if err := e.r.Connect(a, b, f); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 41) + `"}`, `not json`} {
		if resp, _ := e.do(t, "POST", "/api/presets", bad); resp.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d", bad, resp.StatusCode)
		}
	}

	resp, body := e.do(t, "POST", "/api/presets", `{"name":"  Live   set "}`)
	if resp.StatusCode != 200 {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	var saved presetJSON
	_ = json.Unmarshal(body, &saved)
	if saved.Name != "Live set" || saved.Connections != 1 || saved.ID == "" || saved.Saved == 0 {
		t.Fatalf("saved: %+v", saved)
	}

	// Same name (any case) is refused unless replace is set.
	if resp, _ := e.do(t, "POST", "/api/presets", `{"name":"live SET"}`); resp.StatusCode != 409 {
		t.Errorf("duplicate name: %d", resp.StatusCode)
	}

	// Change the routing, then bring the preset back.
	_ = e.r.Disconnect(a, b)
	if err := e.r.Connect(b, "usb:zzzz:0009:1.9:0", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	e.store.Update(func(st *config.State) {
		st.Known["usb:zzzz:0009:1.9:0"] = config.KnownPort{Name: "Other", Kind: router.KindUSB, Output: true}
	})

	resp, body = e.do(t, "POST", "/api/presets/"+saved.ID+"/recall", "")
	if resp.StatusCode != 200 {
		t.Fatalf("recall: %d %s", resp.StatusCode, body)
	}
	var rec struct {
		Name    string        `json:"name"`
		Applied int           `json:"applied"`
		Skipped []skippedConn `json:"skipped"`
	}
	_ = json.Unmarshal(body, &rec)
	if rec.Name != "Live set" || rec.Applied != 1 || len(rec.Skipped) != 0 {
		t.Fatalf("recall result: %+v", rec)
	}
	got := connSet(e.r)
	if len(got) != 1 {
		t.Fatalf("connections after recall: %v", got)
	}
	if g, ok := got[a+" > "+b]; !ok || g.Transpose != 3 || len(g.Block) != 1 || g.Block[0] != "clock" {
		t.Fatalf("filter not restored: %v", got)
	}
	if e.reconciled.Load() == 0 {
		t.Error("recall did not trigger a reconcile")
	}

	// Overwrite with replace.
	_ = e.r.Disconnect(a, b)
	if resp, _ := e.do(t, "POST", "/api/presets", `{"name":"Live set","replace":true}`); resp.StatusCode != 200 {
		t.Fatalf("replace: %d", resp.StatusCode)
	}
	var list []presetJSON
	e.getJSON(t, "/api/presets", &list)
	if len(list) != 1 || list[0].Connections != 0 {
		t.Fatalf("list after replace: %+v", list)
	}

	// Rename, then name clash.
	if resp, _ := e.do(t, "POST", "/api/presets", `{"name":"Other"}`); resp.StatusCode != 200 {
		t.Fatal("second preset")
	}
	if resp, _ := e.do(t, "PATCH", "/api/presets/"+saved.ID, `{"name":"other"}`); resp.StatusCode != 409 {
		t.Errorf("rename onto an existing name: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "PATCH", "/api/presets/"+saved.ID, `{"name":"Studio"}`); resp.StatusCode != 200 {
		t.Errorf("rename: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "PATCH", "/api/presets/nope", `{"name":"x"}`); resp.StatusCode != 404 {
		t.Errorf("rename unknown: %d", resp.StatusCode)
	}

	// Delete.
	if resp, _ := e.do(t, "DELETE", "/api/presets/"+saved.ID, ""); resp.StatusCode != 200 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/presets/"+saved.ID, ""); resp.StatusCode != 404 {
		t.Errorf("delete again: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/presets/"+saved.ID+"/recall", ""); resp.StatusCode != 404 {
		t.Errorf("recall deleted: %d", resp.StatusCode)
	}
}

func TestPresetRecallSkipsUnknownDevicesAndKeepsDormantOnes(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.a.info.ID, e.b.info.ID
	e.store.Update(func(st *config.State) {
		st.Known["usb:old:0001:1.5:0"] = config.KnownPort{Name: "Unplugged", Kind: router.KindUSB, Input: true, Output: true}
		st.Presets = []config.Preset{{ID: "p-x", Name: "x", Connections: []router.ConnSpec{
			{From: a, To: b, Filter: router.DefaultFilter()},
			{From: "usb:old:0001:1.5:0", To: b, Filter: router.DefaultFilter()},  // known, unplugged: dormant
			{From: "usb:gone:0001:1.6:0", To: b, Filter: router.DefaultFilter()}, // never seen / forgotten
			{From: b, To: a, Filter: router.DefaultFilter()},                     // a cannot receive any more
		}}}
	})
	resp, body := e.do(t, "POST", "/api/presets/p-x/recall", "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var rec struct {
		Applied int           `json:"applied"`
		Skipped []skippedConn `json:"skipped"`
	}
	_ = json.Unmarshal(body, &rec)
	if rec.Applied != 2 || len(rec.Skipped) != 2 {
		t.Fatalf("applied %d, skipped %+v", rec.Applied, rec.Skipped)
	}
	got := connSet(e.r)
	if _, ok := got["usb:old:0001:1.5:0 > "+b]; !ok {
		t.Errorf("the dormant connection was dropped: %v", got)
	}
	if _, ok := got["usb:gone:0001:1.6:0 > "+b]; ok {
		t.Errorf("a connection to an unknown device was created: %v", got)
	}
}

func TestPresetLimit(t *testing.T) {
	e := newEnv(t, nil)
	e.store.Update(func(st *config.State) {
		for i := 0; i < maxPresets; i++ {
			st.Presets = append(st.Presets, config.Preset{ID: "p-" + strings.Repeat("a", i+1), Name: strings.Repeat("a", i+1)})
		}
	})
	if resp, _ := e.do(t, "POST", "/api/presets", `{"name":"one too many"}`); resp.StatusCode != 409 {
		t.Errorf("over the limit: %d", resp.StatusCode)
	}
	// Replacing an existing one is still fine.
	if resp, _ := e.do(t, "POST", "/api/presets", `{"name":"a","replace":true}`); resp.StatusCode != 200 {
		t.Errorf("replace at the limit: %d", resp.StatusCode)
	}
}
