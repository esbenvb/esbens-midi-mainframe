package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"emm/internal/router"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestOpenMissingFile(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sub", "state.json"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot()
	if st.Version != CurrentVersion || st.Ports == nil || st.Known == nil || !st.RTPOn() || !st.BLEOn() {
		t.Fatalf("bad defaults: %+v", st)
	}
}

func TestRoundTripAndAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "emm", "state.json")
	s, _ := Open(path, quiet())
	s.SaveDelay = time.Millisecond
	f := false
	s.Update(func(st *State) {
		st.Connections = []router.ConnSpec{{From: "a", To: "b", Filter: router.Filter{Block: []string{"cc"}}}}
		st.Ports["a"] = PortSettings{Alias: "Keys", RTP: &f, RTPPort: 5004}
		st.Known["a"] = KnownPort{Name: "Keys", Kind: router.KindUSB, Input: true}
		st.BLE.Name = "My EMM"
		st.Network = json.RawMessage(`{"wifi_ssid":"x"}`)
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	s2, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	st := s2.Snapshot()
	if len(st.Connections) != 1 || st.Connections[0].Filter.Block[0] != "cc" {
		t.Fatalf("connections: %+v", st.Connections)
	}
	if p := st.Ports["a"]; p.Alias != "Keys" || p.RTPEnabled() || p.RTPPort != 5004 {
		t.Fatalf("port settings: %+v", p)
	}
	if st.BLE.Name != "My EMM" || string(st.Network) != `{"wifi_ssid":"x"}` {
		t.Fatalf("ble/network: %+v %s", st.BLE, st.Network)
	}
}

func TestDebouncedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, quiet())
	s.SaveDelay = 20 * time.Millisecond
	saved := make(chan error, 4)
	s.OnSaved = func(err error) { saved <- err }
	for i := 0; i < 10; i++ {
		s.Update(func(st *State) { st.BLE.Name = "n" })
	}
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no save")
	}
	select {
	case <-saved:
		t.Fatal("writes were not coalesced")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCorruptFileIsMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Connections) != 0 {
		t.Fatal("expected empty state")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt file not preserved: %v", matches)
	}
}

func TestUnwritableDirReportsAndRetries(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _ := Open(filepath.Join(ro, "state.json"), quiet())
	s.SaveDelay = time.Millisecond
	if s.CheckWritable() {
		t.Fatal("read-only dir reported writable")
	}
	s.Update(func(st *State) { st.BLE.Name = "x" })
	if err := s.Flush(); err == nil {
		t.Fatal("expected write error")
	}
	if s.Writable() {
		t.Fatal("Writable should be false after a failed save")
	}
	os.Chmod(ro, 0o755)
	if err := s.Flush(); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if !s.Writable() {
		t.Fatal("Writable should recover")
	}
}

func TestWebPassword(t *testing.T) {
	var st State
	if st.WebPasswordSet() || st.CheckWebPassword("") || st.CheckWebPassword("x") {
		t.Fatal("empty state must reject everything")
	}
	st.SetWebPassword("hunter2")
	if !st.WebPasswordSet() || !st.CheckWebPassword("hunter2") || st.CheckWebPassword("hunter3") || st.CheckWebPassword("") {
		t.Fatal("password check")
	}
	if strings.Contains(st.Web.Hash, "hunter2") {
		t.Fatal("password stored in clear")
	}
	st.SetWebPassword("")
	if st.WebPasswordSet() {
		t.Fatal("clear failed")
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"), quiet())
	s.Update(func(st *State) { st.Ports["a"] = PortSettings{Alias: "one"} })
	snap := s.Snapshot()
	snap.Ports["a"] = PortSettings{Alias: "changed"}
	if s.Snapshot().Ports["a"].Alias != "one" {
		t.Fatal("snapshot aliases live state")
	}
}

func TestNoOpUpdateDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, quiet())
	s.SaveDelay = time.Millisecond
	s.Update(func(st *State) {}) // nothing changed
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a no-op update wrote the file")
	}
	s.Update(func(st *State) { st.BLE.Name = "x" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	info1, _ := os.Stat(path)
	s.Update(func(st *State) { st.BLE.Name = "x" }) // same value again
	time.Sleep(30 * time.Millisecond)
	info2, _ := os.Stat(path)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("rewriting identical state touched the file")
	}
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A save that failed is retried by the store itself, without any further
// change and without an explicit Flush, and no-op Updates must not get in the
// way (the reconciler issues those all the time).
func TestFailedSaveIsRetriedByTimer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)
	path := filepath.Join(ro, "state.json")
	s, _ := Open(path, quiet())
	s.SaveDelay = time.Millisecond
	s.retryBase, s.retryMax = 20*time.Millisecond, 80*time.Millisecond
	failed := make(chan struct{}, 1)
	s.OnSaved = func(err error) {
		if err != nil {
			select {
			case failed <- struct{}{}:
			default:
			}
		}
	}
	s.Update(func(st *State) { st.BLE.Name = "renamed" })
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("the debounced save never ran")
	}
	if s.Writable() {
		t.Fatal("Writable should be false after a failed save")
	}

	// The card recovers. Nothing changes any more; only identical Updates arrive.
	if err := os.Chmod(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Update(func(st *State) { st.BLE.Name = "renamed" })
	}
	waitFor(t, 3*time.Second, "the failed save to be retried", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	waitFor(t, time.Second, "Writable to recover", s.Writable)
	s2, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Snapshot().BLE.Name; got != "renamed" {
		t.Fatalf("retried save wrote %q", got)
	}
}

// The retry delay backs off while the failure lasts, up to the cap.
func TestRetryBacksOffAndIsCapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)
	s, _ := Open(filepath.Join(ro, "state.json"), quiet())
	s.retryBase, s.retryMax = time.Hour, 4*time.Hour // timers never fire during the test
	s.Update(func(st *State) { st.BLE.Name = "x" })
	var got []time.Duration
	for i := 0; i < 5; i++ {
		if err := s.Flush(); err == nil {
			t.Fatal("expected write error")
		}
		s.mu.Lock()
		got = append(got, s.retry)
		armed := s.timer != nil
		s.mu.Unlock()
		if !armed {
			t.Fatal("no retry timer after a failed save")
		}
	}
	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retry delays %v, want %v", got, want)
		}
	}
	// Recovery resets the backoff and leaves no timer behind.
	os.Chmod(ro, 0o755)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retry != 0 || s.timer != nil || s.dirty {
		t.Fatalf("after recovery: retry=%v timer=%v dirty=%v", s.retry, s.timer != nil, s.dirty)
	}
}

// slowWriter replaces the store's file writer: the first call blocks until
// release is closed, later calls go straight through. Every call writes for
// real, so the file shows which snapshot landed last.
type slowWriter struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
	done    int
}

func newSlowWriter() *slowWriter {
	return &slowWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *slowWriter) write(path string, data []byte) error {
	w.mu.Lock()
	w.calls++
	first := w.calls == 1
	w.mu.Unlock()
	if first {
		close(w.started)
		<-w.release
	}
	err := writeFileAtomic(path, data)
	w.mu.Lock()
	w.done++
	w.mu.Unlock()
	return err
}

func (w *slowWriter) finished() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

// A second Flush must wait for the write that is in flight instead of
// reporting success while the data is still only in a temp file.
func TestFlushWaitsForWriteInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, quiet())
	w := newSlowWriter()
	s.write = w.write
	s.Update(func(st *State) { st.BLE.Name = "latest" })

	aDone := make(chan error, 1)
	go func() { aDone <- s.Flush() }()
	<-w.started

	bDone := make(chan error, 1)
	var finishedAtB int
	go func() {
		err := s.Flush()
		finishedAtB = w.finished()
		bDone <- err
	}()
	select {
	case <-bDone:
		t.Fatal("second Flush returned while the first write was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(w.release)
	for i, ch := range []chan error{aDone, bDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("flush %d: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("flush %d never returned", i)
		}
	}
	if finishedAtB != 1 {
		t.Fatalf("second Flush returned before the write finished (finished writes: %d)", finishedAtB)
	}
	if s2, _ := Open(path, quiet()); s2.Snapshot().BLE.Name != "latest" {
		t.Fatal("state not on disk")
	}
}

// Two writes must not rename over each other out of order: the newest
// snapshot has to be the one left on disk.
func TestConcurrentFlushesLeaveNewestOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, quiet())
	w := newSlowWriter()
	s.write = w.write
	s.Update(func(st *State) { st.BLE.Name = "old" })

	aDone := make(chan error, 1)
	go func() { aDone <- s.Flush() }() // stalls in its write with the old snapshot
	<-w.started

	s.Update(func(st *State) { st.BLE.Name = "new" })
	bDone := make(chan error, 1)
	go func() { bDone <- s.Flush() }()
	time.Sleep(100 * time.Millisecond) // B is either queued behind A or already wrote
	close(w.release)
	for _, ch := range []chan error{aDone, bDone} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if s2, _ := Open(path, quiet()); s2.Snapshot().BLE.Name != "new" {
		t.Fatalf("an older snapshot was renamed over the newer one: %q", s2.Snapshot().BLE.Name)
	}
}

// system.json holds the WiFi passphrase in clear and the web password hash, and
// state.json is private too.
func TestStateFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// A file left by an older version with the wide mode is narrowed on the next save.
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o644)
	s, _ := Open(path, quiet())
	s.Update(func(st *State) {
		st.BLE.Name = "x"
		st.Network = json.RawMessage(`{"wifi_password":"secret"}`)
		st.SetWebPassword("pw")
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"state.json", "system.json"} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode %o, want 600", f, got)
		}
	}
}

// A read error that is neither "missing" nor corrupt JSON must not keep the
// daemon from starting, and the file must never be replaced: it may hold the web
// password hash and the saved WiFi that one failed read could not deliver. The
// store runs on an empty in-memory state, reports itself as not writable, and
// Flush refuses with an error instead of overwriting.
func TestUnreadableStateFileIsNeverOverwritten(t *testing.T) {
	const good = `{"version":1,"ble":{"name":"keep"},"web":{"salt":"aa","hash":"bb"},"network":{"ssid":"home"}}`
	cases := []struct {
		name  string
		setup func(t *testing.T, path string) (restore func())
	}{
		{"I/O error on read", func(t *testing.T, path string) func() {
			if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
				t.Fatal(err)
			}
			orig := readFile
			readFile = func(p string) ([]byte, error) {
				if p == path {
					return nil, &os.PathError{Op: "read", Path: p, Err: syscall.EIO}
				}
				return orig(p)
			}
			return func() { readFile = orig }
		}},
		{"permission denied", func(t *testing.T, path string) func() {
			if os.Geteuid() == 0 {
				t.Skip("root reads everything")
			}
			if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			return func() { _ = os.Chmod(path, 0o600) }
		}},
		{"directory in the way", func(t *testing.T, path string) func() {
			if err := os.Mkdir(path, 0o755); err != nil { // read gives EISDIR
				t.Fatal(err)
			}
			return func() {}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			restore := tc.setup(t, path)
			defer restore()

			s, err := Open(path, quiet())
			if err != nil {
				t.Fatalf("Open must not fail on a read error: %v", err)
			}
			s.SaveDelay = time.Millisecond
			saved := 0
			s.OnSaved = func(error) { saved++ }

			if s.Unreadable() == nil {
				t.Fatal("the store does not know that its file was unreadable")
			}
			if s.Writable() {
				t.Fatal("Writable must be false so that the UI warns that settings will not survive")
			}
			st := s.Snapshot()
			if len(st.Connections) != 0 || st.Ports == nil || st.Known == nil || st.Version != CurrentVersion ||
				st.BLE.Name != "" || st.WebPasswordSet() {
				t.Fatalf("expected a usable empty state: %+v", st)
			}
			// CheckWritable only probes the directory; it must not turn the flag back on.
			s.CheckWritable()
			if s.Writable() {
				t.Fatal("CheckWritable made an unreadable store writable")
			}

			// What the daemon does a moment after startup: record the ports it sees.
			s.Update(func(st *State) {
				st.Known["usb:1:2:3:0"] = KnownPort{Name: "Synth"}
				st.SetWebPassword("new")
			})
			s.mu.Lock()
			timer := s.timer
			s.mu.Unlock()
			if timer != nil {
				t.Fatal("a save was scheduled for a store that must not write")
			}
			if mem := s.Snapshot(); !mem.WebPasswordSet() {
				t.Fatal("in-memory changes must still work")
			}
			err = s.Flush()
			if err == nil {
				t.Fatal("Flush must refuse while the state file is unreadable")
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("the error should name the file: %v", err)
			}
			if err := s.Flush(); err == nil {
				t.Fatal("the second Flush must refuse as well")
			}
			time.Sleep(60 * time.Millisecond) // no timer, no retry, no late write
			if saved != 0 {
				t.Fatal("OnSaved was called although nothing was written")
			}
			if s.Writable() {
				t.Fatal("Writable became true")
			}
			if names := dirNames(t, dir); len(names) != 1 || names[0] != "state.json" {
				t.Fatalf("stray files next to the state file: %v", names)
			}

			restore() // the card works again (a restart)
			if fi, err := os.Stat(path); err != nil {
				t.Fatalf("the unreadable entry is gone: %v", err)
			} else if !fi.IsDir() {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != good {
					t.Fatalf("the state file was changed: %q (%v)", b, err)
				}
				s2, err := Open(path, quiet())
				if err != nil {
					t.Fatal(err)
				}
				if st := s2.Snapshot(); st.BLE.Name != "keep" || !st.WebPasswordSet() || string(st.Network) != `{"ssid":"home"}` {
					t.Fatalf("nothing was lost, but reading the file again gave %+v", st)
				}
				if s2.Unreadable() != nil || !s2.Writable() {
					t.Fatal("a readable file must give a normal, writable store")
				}
			}
		})
	}
}

// Corrupt JSON, unlike an unreadable file, is moved aside, and saving works
// from then on.
func TestCorruptFileIsMovedAsideAndStoreStaysWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if s.Unreadable() != nil || !s.Writable() {
		t.Fatalf("a corrupt file is moved aside: the store must be writable (unreadable=%v)", s.Unreadable())
	}
	s.Update(func(st *State) { st.BLE.Name = "fresh" })
	if err := s.Flush(); err != nil {
		t.Fatalf("saving after a corrupt start must work: %v", err)
	}
	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 1 {
		t.Fatalf("corrupt file not preserved: %v", matches)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"fresh"`) {
		t.Fatalf("new state not written: %s", b)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}
