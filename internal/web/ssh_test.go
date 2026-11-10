package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"emm/internal/config"
)

type fakeSSH struct {
	calls   []sshCall
	err     error
	running bool
}

type sshCall struct {
	enabled bool
	hash    string
	keys    []string
}

func (f *fakeSSH) Apply(enabled bool, hash string, keys []string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, sshCall{enabled, hash, keys})
	f.running = enabled
	return nil
}
func (f *fakeSSH) Status() (bool, string) { return f.running, "SHA256:test" }

// doPW sends a request with the web password, as a browser that has been asked for it would.
func doPW(t *testing.T, e *env, pw, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	return e.do(t, method, path, body, "Authorization", "Basic "+b64("u:"+pw))
}

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl esben@laptop"

func TestSSHNotAvailableOffTheImage(t *testing.T) {
	e := newEnv(t, nil)
	var b sshBody
	e.getJSON(t, "/api/ssh", &b)
	if b.Available {
		t.Errorf("available without a controller: %+v", b)
	}
	if resp, _ := e.do(t, "PUT", "/api/ssh", `{"enabled":true}`); resp.StatusCode != 501 {
		t.Errorf("PUT: %d", resp.StatusCode)
	}
}

func TestSSHNeedsAWebPasswordAndACredential(t *testing.T) {
	f := &fakeSSH{}
	e := newEnv(t, func(d *Deps) { d.SSH = f })

	var b sshBody
	e.getJSON(t, "/api/ssh", &b)
	if !b.Available || b.Enabled || !b.NeedsWebPassword || b.Host != "emm.local" {
		t.Fatalf("initial: %+v", b)
	}

	if resp, body := e.do(t, "PUT", "/api/ssh", `{"enabled":true,"password":"longenough"}`); resp.StatusCode != 409 {
		t.Fatalf("no web password: %d %s", resp.StatusCode, body)
	}
	e.store.Update(func(st *config.State) { st.SetWebPassword("webpw") })

	for _, bad := range []struct{ body, why string }{
		{`{"enabled":true}`, "no way to log in"},
		{`{"enabled":true,"password":"short"}`, "short password"},
		{`{"enabled":true,"authorized_keys":"command=\"x\" ` + sshKey + `"}`, "key with options"},
		{`{"enabled":true,"authorized_keys":"nonsense"}`, "not a key"},
	} {
		if resp, _ := doPW(t, e, "webpw", "PUT", "/api/ssh", bad.body); resp.StatusCode != 400 {
			t.Errorf("%s: %d", bad.why, resp.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused request reached the system: %+v", f.calls)
	}

	resp, body := doPW(t, e, "webpw", "PUT", "/api/ssh", `{"enabled":true,"password":"longenough","authorized_keys":"`+sshKey+`\n"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("enable: %d %s", resp.StatusCode, body)
	}
	if len(f.calls) != 1 || !f.calls[0].enabled || !strings.HasPrefix(f.calls[0].hash, "$6$") ||
		len(f.calls[0].keys) != 1 || f.calls[0].keys[0] != sshKey {
		t.Fatalf("applied: %+v", f.calls)
	}
	if strings.Contains(f.calls[0].hash, "longenough") {
		t.Error("the plain password reached the system layer")
	}
	_ = json.Unmarshal(body, &b)
	if !b.Enabled || !b.Running || !b.PasswordSet || len(b.Keys) != 1 || b.Fingerprint != "SHA256:test" {
		t.Errorf("answer: %+v", b)
	}
	st := e.store.Snapshot()
	if !st.SSH.Enabled || strings.Contains(st.SSH.PasswordHash, "longenough") || st.SSH.PasswordHash == "" {
		t.Errorf("stored: %+v", st.SSH)
	}

	// Removing the web password while SSH is on is refused.
	if resp, _ := doPW(t, e, "webpw", "PUT", "/api/settings", `{"web":{"password":""}}`); resp.StatusCode != 409 {
		t.Errorf("removing the web password: %d", resp.StatusCode)
	}

	// Changing only the keys keeps the password.
	hash := st.SSH.PasswordHash
	if resp, _ := doPW(t, e, "webpw", "PUT", "/api/ssh", `{"authorized_keys":""}`); resp.StatusCode != 200 {
		t.Fatalf("clear keys: %d", resp.StatusCode)
	}
	last := f.calls[len(f.calls)-1]
	if !last.enabled || last.hash != hash || len(last.keys) != 0 {
		t.Errorf("last call: %+v", last)
	}

	// Off.
	if resp, _ := doPW(t, e, "webpw", "PUT", "/api/ssh", `{"enabled":false}`); resp.StatusCode != 200 {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	if f.calls[len(f.calls)-1].enabled || e.store.Snapshot().SSH.Enabled {
		t.Error("not switched off")
	}
	// The web password can go again once SSH is off.
	if resp, _ := doPW(t, e, "webpw", "PUT", "/api/settings", `{"web":{"password":""}}`); resp.StatusCode != 200 {
		t.Errorf("removing the web password with SSH off: %d", resp.StatusCode)
	}
}

func TestSSHFailureLeavesTheSettingsAlone(t *testing.T) {
	f := &fakeSSH{err: errors.New("cannot start SSH: boom")}
	e := newEnv(t, func(d *Deps) { d.SSH = f; d.BootWebPassword = "fromfile" })
	resp, body := doPW(t, e, "fromfile", "PUT", "/api/ssh", `{"enabled":true,"password":"longenough"}`)
	if resp.StatusCode != 502 || !strings.Contains(string(body), "boom") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if e.store.Snapshot().SSH.Enabled {
		t.Error("a failed switch-on was saved")
	}
}
