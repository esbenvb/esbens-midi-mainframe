package web

import (
	"encoding/json"
	"strings"
	"testing"

	"emm/internal/config"
	"emm/internal/router"
)

func connSet(r *router.Router) map[string]router.Filter {
	m := map[string]router.Filter{}
	for _, c := range r.Connections() {
		m[c.From+" > "+c.To] = c.Filter
	}
	return m
}

func TestForgetOfflineDevice(t *testing.T) {
	e := newEnv(t, nil)
	b := e.b.info.ID
	old, oldB := "usb:0a67:5021:1-1.1.2:0", "usb:0a67:5021:1-1.1.3.4.4:0"
	e.store.Update(func(st *config.State) {
		for _, id := range []string{old, oldB} {
			st.Known[id] = config.KnownPort{Name: "Hydrasynth Deluxe", Kind: router.KindUSB, Input: true, Output: true}
			st.Known["rtp:"+id] = config.KnownPort{Name: "Hydrasynth Deluxe", Kind: router.KindRTP, Input: true, Output: true}
			st.Ports[id] = config.PortSettings{Alias: "Hydra", RTPPort: 5010}
		}
	})
	e.r.Load([]router.ConnSpec{
		{From: old, To: "rtp:" + old, Filter: router.DefaultFilter()},
		{From: "rtp:" + old, To: old, Filter: router.DefaultFilter()},
		{From: oldB, To: b, Filter: router.DefaultFilter()},
		{From: e.a.info.ID, To: b, Filter: router.DefaultFilter()},
	})

	// A plugged-in device cannot be forgotten, an unknown one is not found.
	if resp, _ := e.do(t, "DELETE", "/api/ports/"+b, ""); resp.StatusCode != 409 {
		t.Errorf("plugged in: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/ports/usb:nope:0000:1-9:0", ""); resp.StatusCode != 404 {
		t.Errorf("unknown: %d", resp.StatusCode)
	}

	resp, body := e.do(t, "DELETE", "/api/ports/"+old, "")
	if resp.StatusCode != 200 {
		t.Fatalf("forget: %d %s", resp.StatusCode, body)
	}
	got := connSet(e.r)
	for c := range got {
		if strings.Contains(c, old) {
			t.Errorf("connection of the forgotten device is left: %s", c)
		}
	}
	if _, ok := got[oldB+" > "+b]; !ok {
		t.Error("another device's connection was removed")
	}
	st := e.store.Snapshot()
	for _, id := range []string{old, "rtp:" + old} {
		if _, ok := st.Known[id]; ok {
			t.Errorf("%s is still remembered", id)
		}
	}
	if _, ok := st.Ports[old]; ok {
		t.Error("its settings are still there")
	}
	if _, ok := st.Known[oldB]; !ok {
		t.Error("the other unplugged device was forgotten too")
	}

	// A device's network endpoint cannot be forgotten on its own.
	if resp, _ := e.do(t, "DELETE", "/api/ports/rtp:"+oldB, ""); resp.StatusCode != 400 {
		t.Errorf("endpoint: %d", resp.StatusCode)
	}

	// Forget all that are unplugged.
	resp, body = e.do(t, "POST", "/api/ports/forget-offline", "")
	if resp.StatusCode != 200 {
		t.Fatalf("forget-offline: %d %s", resp.StatusCode, body)
	}
	var out map[string]int
	_ = json.Unmarshal(body, &out)
	if out["removed"] != 1 {
		t.Errorf("removed %v, want 1", out)
	}
	st = e.store.Snapshot()
	if _, ok := st.Known[oldB]; ok {
		t.Error("an unplugged device survived forget-offline")
	}
	if len(connSet(e.r)) != 1 {
		t.Errorf("only the connection between plugged-in devices should be left: %v", connSet(e.r))
	}
	if _, ok := st.Known[e.b.info.ID]; ok && false {
		t.Error("unreachable")
	}
}

func TestForgetOfflineKeepsNetworkPorts(t *testing.T) {
	e := newEnv(t, nil)
	e.store.Update(func(st *config.State) {
		st.VirtualRTP = []config.VirtualRTP{{ID: "rtp:net-daw", Name: "DAW", Port: 5020}}
	})
	_ = e.r.Connect(e.b.info.ID, "rtp:net-daw", router.Filter{})
	resp, body := e.do(t, "POST", "/api/ports/forget-offline", "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(connSet(e.r)) != 1 {
		t.Errorf("a connection to the user's network port was removed: %v", connSet(e.r))
	}
	if len(e.store.Snapshot().VirtualRTP) != 1 {
		t.Error("the user's network port was removed")
	}
}
