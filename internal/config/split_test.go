package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"emm/internal/router"
)

const legacyState = `{
  "version": 1,
  "connections": [{"from": "a", "to": "b", "filter": {}}],
  "ports": {"a": {"alias": "Keys"}},
  "known": {"a": {"name": "Keys", "kind": "usb", "input": true, "output": false, "last_seen": 5}},
  "ble": {"name": "My EMM"},
  "web": {"salt": "aa", "hash": "bb"},
  "ssh": {"enabled": true, "password_hash": "$6$x$y", "authorized_keys": ["ssh-ed25519 AAAA k"]},
  "network": {"wifi_ssid": "home", "wifi_password": "hunter2"}
}
`

func readJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return m
}

// recordingWriter counts which files are written, in order.
type recordingWriter struct {
	mu    sync.Mutex
	paths []string
}

func (w *recordingWriter) write(path string, data []byte) error {
	w.mu.Lock()
	w.paths = append(w.paths, filepath.Base(path))
	w.mu.Unlock()
	return writeFileAtomic(path, data)
}

func (w *recordingWriter) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.paths
	w.paths = nil
	return p
}

func TestSecretsAndApplicationStateLiveInSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, _ := Open(path, quiet())
	s.Update(func(st *State) {
		st.Connections = []router.ConnSpec{{From: "a", To: "b", Filter: router.DefaultFilter()}}
		st.Ports["a"] = PortSettings{Alias: "Keys"}
		st.BLE.Name = "My EMM"
		st.SetWebPassword("pw")
		st.SSH.Enabled = true
		st.SSH.PasswordHash = "$6$salt$hash"
		st.SSH.AuthorizedKeys = []string{"ssh-ed25519 AAAA me"}
		st.Network = json.RawMessage(`{"wifi_ssid":"home","wifi_password":"hunter2"}`)
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	app, sys := readJSON(t, path), readJSON(t, filepath.Join(dir, "system.json"))
	for _, k := range []string{"web", "ssh", "network"} {
		if _, ok := app[k]; ok {
			t.Errorf("state.json has %q: a secret in the harmless file", k)
		}
		if _, ok := sys[k]; !ok {
			t.Errorf("system.json lacks %q", k)
		}
	}
	for _, k := range []string{"connections", "ports", "known", "ble", "rtp"} {
		if _, ok := app[k]; !ok {
			t.Errorf("state.json lacks %q", k)
		}
		if _, ok := sys[k]; ok {
			t.Errorf("system.json has %q: application state in the secrets file", k)
		}
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"hunter2", "$6$salt$hash", "AAAA", s.Snapshot().Web.Hash} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("state.json contains %q", secret)
		}
	}

	// Encoding a State by any other route cannot leak them either.
	snap := s.Snapshot()
	enc, _ := json.Marshal(&snap)
	for _, secret := range []string{"hunter2", "$6$salt$hash", snap.Web.Hash} {
		if strings.Contains(string(enc), secret) {
			t.Errorf("json.Marshal(State) contains %q", secret)
		}
	}

	// And both come back.
	s2, _ := Open(path, quiet())
	got := s2.Snapshot()
	if len(got.Connections) != 1 || got.Ports["a"].Alias != "Keys" || got.BLE.Name != "My EMM" ||
		!got.CheckWebPassword("pw") || !got.SSH.Enabled || got.SSH.PasswordHash != "$6$salt$hash" ||
		len(got.SSH.AuthorizedKeys) != 1 || string(got.Network) != `{"wifi_ssid":"home","wifi_password":"hunter2"}` {
		t.Fatalf("round trip: %+v / %s", got, got.Network)
	}
}

// Only the file whose own part changed is written.
func TestOnlyTheChangedFileIsWritten(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "state.json"), quiet())
	rw := &recordingWriter{}
	s.write = rw.write
	s.SaveDelay = time.Millisecond

	s.Update(func(st *State) { st.Ports["a"] = PortSettings{Alias: "x"} })
	s.Flush()
	if got := rw.take(); len(got) != 1 || got[0] != "state.json" {
		t.Fatalf("an application change wrote %v", got)
	}
	s.Update(func(st *State) { st.Network = json.RawMessage(`{"wifi_ssid":"h"}`) })
	s.Flush()
	if got := rw.take(); len(got) != 1 || got[0] != "system.json" {
		t.Fatalf("a network change wrote %v", got)
	}
	s.Update(func(st *State) { st.SetWebPassword("pw"); st.BLE.Name = "n" })
	s.Flush()
	if got := rw.take(); len(got) != 2 || got[0] != "system.json" || got[1] != "state.json" {
		t.Fatalf("both changed: wrote %v (the secrets go first)", got)
	}
	s.Update(func(st *State) {}) // nothing changed
	s.Flush()
	if got := rw.take(); len(got) != 0 {
		t.Fatalf("a no-op wrote %v", got)
	}
	// system.json is not created until there is something in it.
	d2 := t.TempDir()
	s2, _ := Open(filepath.Join(d2, "state.json"), quiet())
	s2.Update(func(st *State) { st.BLE.Name = "only app" })
	s2.Flush()
	if _, err := os.Stat(filepath.Join(d2, "system.json")); !os.IsNotExist(err) {
		t.Fatalf("system.json was created for an application change: %v", err)
	}
}

// An installation from before the split has everything in state.json.
func TestOldStateFileIsSplitOnFirstStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)

	s, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot()
	if len(st.Connections) != 1 || st.BLE.Name != "My EMM" || !st.WebPasswordSet() || !st.SSH.Enabled ||
		string(st.Network) != `{"wifi_ssid":"home","wifi_password":"hunter2"}` {
		t.Fatalf("nothing may be lost: %+v", st)
	}
	app, sys := readJSON(t, path), readJSON(t, filepath.Join(dir, "system.json"))
	for _, k := range []string{"web", "ssh", "network"} {
		if _, ok := app[k]; ok {
			t.Errorf("state.json still has %q after the split", k)
		}
		if _, ok := sys[k]; !ok {
			t.Errorf("system.json lacks %q", k)
		}
	}
	if l := leftovers(t, dir); len(l) != 1 || l[0] != "system.json" {
		t.Fatalf("files: %v", l)
	}
	// Starting again changes nothing and loses nothing.
	s2, _ := Open(path, quiet())
	if got := s2.Snapshot(); !got.CheckWebPassword("") && got.Web.Hash != "bb" || string(got.Network) != string(st.Network) {
		t.Fatalf("second start: %+v", got)
	}
}

// If system.json cannot be written, state.json keeps the secrets: they are never
// removed from the one place they are known to be safe until the new place has them.
func TestSplitKeepsTheSecretsInStateJSONUntilSystemJSONIsSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)
	undo := failRenames(func(from, _ string) bool { return strings.HasSuffix(from, "system.json.new") })
	s, _ := Open(path, quiet())
	undo()
	if st := s.Snapshot(); string(st.Network) == "" || !st.WebPasswordSet() {
		t.Fatalf("in memory the secrets must still be there: %+v", st)
	}
	if _, ok := readJSON(t, path)["network"]; !ok {
		t.Fatal("state.json lost the network settings although system.json was never written")
	}
	// Once the card works again, the next save finishes the move.
	s.SaveDelay = time.Millisecond
	s.Update(func(st *State) { st.BLE.Name = "later" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readJSON(t, filepath.Join(dir, "system.json"))["network"]; !ok {
		t.Fatal("system.json still lacks the network settings")
	}
	// Even now state.json (rewritten with the new BLE name) must not have the secrets:
	// the application file is only ever written from the application part.
	if _, ok := readJSON(t, path)["network"]; ok {
		t.Fatal("state.json still has the network settings")
	}
}

// system.json wins over secrets left behind in an old state.json.
func TestSystemFileWinsOverLeftoverSecretsInStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)
	os.WriteFile(filepath.Join(dir, "system.json"),
		[]byte(`{"version":1,"web":{"salt":"s","hash":"newhash"},"ssh":{},"network":{"wifi_ssid":"new"}}`), 0o600)
	s, _ := Open(path, quiet())
	st := s.Snapshot()
	if st.Web.Hash != "newhash" || string(st.Network) != `{"wifi_ssid":"new"}` || st.SSH.Enabled {
		t.Fatalf("got %+v %s", st.Web, st.Network)
	}
	if _, ok := readJSON(t, path)["network"]; ok {
		t.Fatal("the leftover secrets were not removed from state.json")
	}
}

// A damaged system.json is moved aside like a damaged state.json, and the secrets
// still in an old state.json are used.
func TestCorruptSystemFileFallsBackToOldStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)
	os.WriteFile(filepath.Join(dir, "system.json"), []byte("{not json"), 0o600)
	s, _ := Open(path, quiet())
	if st := s.Snapshot(); string(st.Network) != `{"wifi_ssid":"home","wifi_password":"hunter2"}` {
		t.Fatalf("got %s", st.Network)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "system.json.corrupt-*")); len(m) != 1 {
		t.Fatalf("the damaged file should be kept: %v", dirNames(t, dir))
	}
}

// A system.json that cannot be read does not stop the application state from being saved.
func TestUnreadableSystemFileOnlyStopsItself(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	sysPath := filepath.Join(dir, "system.json")
	os.WriteFile(sysPath, seal([]byte(`{"version":1,"web":{"hash":"keep"}}`), 1), 0o600)
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == sysPath {
			return nil, &os.PathError{Op: "read", Path: p, Err: syscall.EIO}
		}
		return orig(p)
	}
	s, _ := Open(path, quiet())
	readFile = orig
	if s.Unreadable() == nil || s.Writable() {
		t.Fatal("the store must report that a file is unreadable")
	}
	s.SaveDelay = time.Millisecond
	s.Update(func(st *State) {
		st.BLE.Name = "saved anyway"
		st.Network = json.RawMessage(`{"wifi_ssid":"x"}`)
	})
	err := s.Flush()
	if err == nil {
		t.Fatal("the change to the unreadable file must be reported as not saved")
	}
	if _, got := openName(t, path); got != "saved anyway" {
		t.Fatalf("the application state was not saved: %q", got)
	}
	b, _ := os.ReadFile(sysPath)
	if !strings.Contains(string(b), "keep") || strings.Contains(string(b), "wifi_ssid") {
		t.Fatalf("system.json was touched: %s", b)
	}
}

// With an unreadable state.json from before the split, the secrets are in it
// (and cannot be read), so system.json must not be created either.
func TestUnreadableOldStateFileKeepsSystemFileUnwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == path {
			return nil, &os.PathError{Op: "read", Path: p, Err: syscall.EIO}
		}
		return orig(p)
	}
	s, _ := Open(path, quiet())
	readFile = orig
	s.Update(func(st *State) { st.SetWebPassword("fresh") })
	if err := s.Flush(); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, "system.json")); !os.IsNotExist(err) {
		t.Fatal("system.json was created and would hide the secrets in the unreadable state.json")
	}
	if b, _ := os.ReadFile(path); string(b) != legacyState {
		t.Fatal("state.json was touched")
	}
}

// If system.json cannot be written at all, state.json keeps the secrets, however
// often it is saved meanwhile: they must never exist only in memory.
func TestSecretsNeverLeaveStateJSONWhileSystemJSONCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(legacyState), 0o600)
	// A directory in the way: every attempt to write system.json fails.
	if err := os.Mkdir(filepath.Join(dir, "system.json.new"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(path, quiet())
	s.SaveDelay = time.Millisecond
	s.Update(func(st *State) { st.BLE.Name = "app change" })
	if err := s.Flush(); err == nil {
		t.Fatal("system.json cannot be written: Flush must say so")
	}
	if m := readJSON(t, path); m["network"] == nil || m["web"] == nil || m["ssh"] == nil {
		t.Fatalf("state.json lost the secrets although system.json was never written: %v", keys(m))
	}
	// A restart now still has everything.
	reopened, _ := Open(path, quiet())
	if snap := reopened.Snapshot(); string(snap.Network) == "" || !snap.WebPasswordSet() || snap.BLE.Name != "app change" {
		t.Fatalf("after a restart: %+v", snap)
	}
	// The card recovers: the next save finishes the move, in one Flush.
	os.Remove(filepath.Join(dir, "system.json.new"))
	s.Update(func(st *State) { st.BLE.Name = "later" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if m := readJSON(t, filepath.Join(dir, "system.json")); m["network"] == nil {
		t.Fatal("system.json lacks the network settings")
	}
	if m := readJSON(t, path); m["network"] != nil || m["web"] != nil || m["ssh"] != nil {
		t.Fatalf("state.json still has secrets: %v", keys(m))
	}
}

func keys(m map[string]json.RawMessage) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}

// A block of the old file that cannot be read is dropped, the rest is kept, and the
// old file is kept as a whole before it is rewritten.
func TestOldStateFileWithAnUnreadableSecretBlockIsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	bad := strings.Replace(legacyState, `"enabled": true`, `"enabled": "yes"`, 1)
	os.WriteFile(path, []byte(bad), 0o600)
	s, _ := Open(path, quiet())
	st := s.Snapshot()
	if string(st.Network) != `{"wifi_ssid":"home","wifi_password":"hunter2"}` || !st.WebPasswordSet() || st.SSH.PasswordHash != "" {
		t.Fatalf("got %s %+v %+v", st.Network, st.Web, st.SSH)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(m) != 1 {
		t.Fatalf("a copy of the old file must be kept: %v", dirNames(t, dir))
	}
	if b, _ := os.ReadFile(m[0]); !strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), `"yes"`) {
		t.Fatalf("the kept copy is not the old file: %s", b)
	}
}

// A system.json written by hand, as when preparing a card: no seal, a byte order
// mark from a Windows editor, and only the keys it needs.
func TestHandWrittenSystemFileLoads(t *testing.T) {
	for name, content := range map[string]string{
		"minimal":  `{"network": {"wifi_ssid": "Studio", "wifi_password": "secret"}}`,
		"with BOM": "\xef\xbb\xbf" + `{"network": {"wifi_ssid": "Studio", "wifi_password": "secret"}}`,
		"CRLF":     "{\r\n  \"network\": {\"wifi_ssid\": \"Studio\", \"wifi_password\": \"secret\"}\r\n}\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "system.json"), []byte(content), 0o600)
			s, _ := Open(filepath.Join(dir, "state.json"), quiet())
			var n struct {
				SSID string `json:"wifi_ssid"`
			}
			json.Unmarshal(s.Snapshot().Network, &n)
			if n.SSID != "Studio" {
				t.Fatalf("network: %s", s.Snapshot().Network)
			}
			if m, _ := filepath.Glob(filepath.Join(dir, "*.corrupt-*")); len(m) != 0 {
				t.Fatalf("the file was moved aside: %v", m)
			}
		})
	}
}
