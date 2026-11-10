package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func stateJSON(name string) []byte {
	return []byte(fmt.Sprintf(`{"version":1,"ble":{"name":%q}}`+"\n", name))
}

// openName opens path and returns the BLE name, which these tests use to tell
// states apart ("" is the empty state).
func openName(t *testing.T, path string) (*Store, string) {
	t.Helper()
	s, err := Open(path, quiet())
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Snapshot().BLE.Name
}

// sealed is what a save writes for the state called name, with sequence number seq.
func sealed(name string, seq uint64) []byte { return seal(stateJSON(name), seq) }

// fileName reads a state file from disk and returns its BLE name; it fails the test
// when the file is not a whole, valid state.
func fileName(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := parseState(b)
	if err != nil {
		t.Fatalf("%s does not parse: %v\n%s", path, err, b)
	}
	return st.BLE.Name
}

// leftovers lists everything in the state directory but state.json.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range dirNames(t, dir) {
		if n != "state.json" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// crashAt makes saves of path stop at step, as a power cut would; the returned
// function removes the hook.
func crashAt(path, step string) (undo func()) {
	h := func(p, s string) bool { return p == path && s == step }
	testHook.Store(&h)
	return func() { testHook.Store(nil) }
}

// The point of the whole exercise: a power cut at any moment of a save leaves a
// state that Open can read, and it is the old one or the new one, never an empty
// one while a good copy exists.
func TestPowerCutAtAnyPointOfASaveKeepsAGoodState(t *testing.T) {
	for _, existing := range []bool{true, false} {
		for _, c := range []struct {
			step         string
			wantExisting string // when a state.json existed before the save
			wantFirst    string // when this was the first save
		}{
			{"mid-write", "old", ""}, // the new file is cut short: ignored
			{"new-written", "new", "new"},
			{"old-removed", "new", "new"},
			{"moved-aside", "new", "new"}, // state.json is missing at this moment
			{"installed", "new", "new"},
		} {
			name := fmt.Sprintf("%s/existing=%v", c.step, existing)
			t.Run(name, func(t *testing.T) {
				if !existing && (c.step == "old-removed" || c.step == "moved-aside") {
					t.Skip("a first save has no state.json to move aside")
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "state.json")
				if existing {
					if err := writeFileAtomic(path, stateJSON("old")); err != nil {
						t.Fatal(err)
					}
				}
				undo := crashAt(path, c.step)
				err := writeFileAtomic(path, stateJSON("new"))
				undo()
				if err != errCrashed {
					t.Fatalf("the save should have been cut at %s, got %v", c.step, err)
				}

				want := c.wantFirst
				if existing {
					want = c.wantExisting
				}
				s, got := openName(t, path)
				if got != want {
					t.Fatalf("recovered %q, want %q (files: %v)", got, want, dirNames(t, dir))
				}
				if s.Unreadable() != nil {
					t.Fatalf("store unreadable: %v", s.Unreadable())
				}
				// Recovery leaves a clean directory with a normal state.json (none when the
				// first save never got far enough, and nothing was recovered).
				if want != "" {
					if got := fileName(t, path); got != want {
						t.Fatalf("state.json after recovery holds %q, want %q", got, want)
					}
				}
				if l := leftovers(t, dir); len(l) != 0 && !(want == "" && existing == false) {
					t.Fatalf("leftover files after recovery: %v", l)
				}
				// And the store keeps working: a later save and reopen give the newest state.
				s.Update(func(st *State) { st.BLE.Name = "later" })
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
				if _, got := openName(t, path); got != "later" {
					t.Fatalf("after a later save: %q", got)
				}
				if l := leftovers(t, dir); len(l) != 0 {
					t.Fatalf("leftover files after a later save: %v", l)
				}
			})
		}
	}
}

// A normal save leaves state.json and nothing else, and it is private.
func TestSaveLeavesOnlyStateJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 0; i < 3; i++ {
		if err := writeFileAtomic(path, stateJSON(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		if l := leftovers(t, dir); len(l) != 0 {
			t.Fatalf("save %d left %v", i, l)
		}
	}
	if _, got := openName(t, path); got != "2" {
		t.Fatalf("got %q", got)
	}
}

// What a cut leaves on FAT is not only a short file: the size can be right with
// zeros or nothing in it.
func TestUnparsableLeftoversAreIgnoredWhenStateJSONIsGood(t *testing.T) {
	for name, junk := range map[string]string{
		"empty":       "",
		"zero-filled": strings.Repeat("\x00", 64),
		"cut short":   `{"version":1,"ble":{"name":"ne`,
		"null":        "null",
		"array":       "[]",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, stateJSON("good"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".new", []byte(junk), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, got := openName(t, path); got != "good" {
				t.Fatalf("got %q", got)
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Fatalf("junk not cleaned up: %v", l)
			}
		})
	}
}

// state.json itself can be what the cut damaged (a torn directory entry, a
// zero-filled file): the other copies save the day, and the damaged one is kept.
func TestDamagedStateJSONFallsBackToTheOtherCopy(t *testing.T) {
	for _, c := range []struct {
		name, other, want string
	}{
		{"new", ".new", "new"},
		{"old", ".old", "old"},
	} {
		for _, bad := range []string{"", "{not json", "null", strings.Repeat("\x00", 40)} {
			t.Run(fmt.Sprintf("%s/%q", c.name, bad), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "state.json")
				if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+c.other, stateJSON(c.want), 0o600); err != nil {
					t.Fatal(err)
				}
				_, got := openName(t, path)
				if got != c.want {
					t.Fatalf("got %q, want %q", got, c.want)
				}
				if got := fileName(t, path); got != c.want {
					t.Fatalf("state.json after recovery holds %q, want %q", got, c.want)
				}
				l := leftovers(t, dir)
				if len(l) != 1 || !strings.HasPrefix(l[0], "state.json.corrupt-") {
					t.Fatalf("the damaged file should be the only thing kept: %v", l)
				}
			})
		}
	}
}

// Both copies whole (the cut came after the new one was synced): the new one
// is what the save was installing and wins; the older state.json is not kept as
// corrupt, because it is not.
func TestNewerCopyWinsOverGoodStateJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, sealed("old", 4), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", sealed("new", 5), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".old", sealed("older", 3), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, got := openName(t, path); got != "new" {
		t.Fatalf("got %q", got)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("leftovers: %v", l)
	}
}

// Nothing parses anywhere: an empty state, as before, and saving works.
func TestNothingRecoverableStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for _, n := range []string{"", ".new", ".old"} {
		if err := os.WriteFile(path+n, []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, got := openName(t, path)
	if got != "" || s.Unreadable() != nil || !s.Writable() {
		t.Fatalf("got %q, unreadable=%v", got, s.Unreadable())
	}
	s.Update(func(st *State) { st.BLE.Name = "fresh" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, got := openName(t, path); got != "fresh" {
		t.Fatalf("got %q", got)
	}
	if l := leftovers(t, dir); len(l) != 1 || !strings.HasPrefix(l[0], "state.json.corrupt-") {
		t.Fatalf("only the corrupt file should be kept: %v", l)
	}
}

// A save that fails before it moves anything leaves state.json as it was.
func TestFailedSaveLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeFileAtomic(path, stateJSON("old")); err != nil {
		t.Fatal(err)
	}
	// A directory called state.json.new cannot be written as a file.
	if err := os.Mkdir(path+".new", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, stateJSON("new")); err == nil {
		t.Fatal("expected an error")
	}
	if _, got := openName(t, path); got != "old" {
		t.Fatalf("got %q", got)
	}
}

// ---- sealing, ordering and failure paths (found by reviewing the protocol) ----

func TestSealAndUnseal(t *testing.T) {
	for _, in := range []string{"{}\n", `{"a":1}`, "{\n  \"version\": 1,\n  \"ports\": {\n    \"x\": {}\n  }\n}\n", `{"s":"\n  \"_save\": {"}`} {
		out := seal([]byte(in), 42)
		if err := validState(out); err != nil {
			t.Fatalf("sealed %q is not valid JSON: %v\n%s", in, err, out)
		}
		seq, sealedOK, valid := unseal(out)
		if seq != 42 || !sealedOK || !valid {
			t.Errorf("%q: seq=%d sealed=%v valid=%v\n%s", in, seq, sealedOK, valid, out)
		}
	}
	// Anything that changed after sealing, however little, breaks the seal.
	out := seal(stateJSON("aaaa"), 7)
	for _, tamper := range []func([]byte) []byte{
		func(b []byte) []byte { return []byte(strings.Replace(string(b), "aaaa", "bbbb", 1)) },
		func(b []byte) []byte { return []byte(strings.Replace(string(b), `"seq": 7`, `"seq": 8`, 1)) },
		func(b []byte) []byte { return b[:len(b)-3] },
	} {
		if _, s, v := unseal(tamper(append([]byte(nil), out...))); s && v {
			t.Errorf("a changed file still verifies")
		}
	}
	if _, s, _ := unseal(stateJSON("plain")); s {
		t.Error("a file without a seal reports one")
	}
}

// FAT can leave a file of the right size whose clusters still hold an older save.
// Such a .new parses, but it is older than state.json and must lose.
func TestStaleCompleteNewDoesNotBeatNewerStateJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, sealed("bbbb", 5), 0o600)
	os.WriteFile(path+".new", sealed("aaaa", 4), 0o600) // same size, older save
	if _, got := openName(t, path); got != "bbbb" {
		t.Fatalf("got %q: an older, complete .new replaced the newest state", got)
	}
	if got := fileName(t, path); got != "bbbb" || len(leftovers(t, dir)) != 0 {
		t.Fatalf("state.json %q, leftovers %v", got, leftovers(t, dir))
	}
}

// A mix of two saves parses but its checksum does not match.
func TestMixedContentIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, sealed("good", 3), 0o600)
	mixed := []byte(strings.Replace(string(sealed("new!", 9)), "new!", "NEW?", 1)) // seq 9 but altered
	os.WriteFile(path+".new", mixed, 0o600)
	if _, got := openName(t, path); got != "good" {
		t.Fatalf("got %q", got)
	}
}

// A damaged state.json (the seal does not match) loses to a sealed older copy.
func TestDamagedSealedStateJSONFallsBackToSealedOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte(strings.Replace(string(sealed("main", 6)), "main", "MAIN", 1)), 0o600)
	os.WriteFile(path+".old", sealed("prev", 5), 0o600)
	if _, got := openName(t, path); got != "prev" {
		t.Fatalf("got %q", got)
	}
	if l := leftovers(t, dir); len(l) != 1 || !strings.HasPrefix(l[0], "state.json.corrupt-") {
		t.Fatalf("the damaged file should be kept: %v", l)
	}
}

// A file from before sealing, or edited by hand, is used when it is all there is.
func TestUnsealedAndHandEditedFilesStillLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, stateJSON("legacy"), 0o600)
	s, got := openName(t, path)
	if got != "legacy" || len(leftovers(t, dir)) != 0 {
		t.Fatalf("legacy: %q %v", got, leftovers(t, dir))
	}
	s.Update(func(st *State) { st.BLE.Name = "sealed now" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Hand edit: the content changes, the seal no longer matches.
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "sealed now", "by hand!!", 1)), 0o600)
	if _, got := openName(t, path); got != "by hand!!" {
		t.Fatalf("hand edit lost: %q", got)
	}
}

// The sequence number grows with every save, so "newest" is never a guess.
func TestSequenceNumberGrows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 1; i <= 4; i++ {
		if err := writeFileAtomic(path, stateJSON("x")); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if seq, ok, valid := unseal(b); seq != uint64(i) || !ok || !valid {
			t.Fatalf("save %d: seq=%d sealed=%v valid=%v", i, seq, ok, valid)
		}
	}
}

// If state.json cannot be read, a complete copy beside it is still used (in
// memory), and nothing on the card is touched or written.
func TestUnreadableStateJSONRunsOnACopyAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, sealed("main", 2), 0o600)
	os.WriteFile(path+".new", sealed("newer", 3), 0o600)
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == path {
			return nil, &os.PathError{Op: "read", Path: p, Err: syscall.EIO}
		}
		return orig(p)
	}
	s, got := openName(t, path)
	readFile = orig
	if got != "newer" || s.Unreadable() == nil {
		t.Fatalf("got %q, unreadable=%v", got, s.Unreadable())
	}
	s.SaveDelay = time.Millisecond
	s.Update(func(st *State) { st.BLE.Name = "changed" })
	if err := s.Flush(); err == nil {
		t.Fatal("Flush must refuse")
	}
	if got := fileName(t, path); got != "main" {
		t.Fatalf("state.json was touched: %q", got)
	}
	if got := fileName(t, path+".new"); got != "newer" {
		t.Fatalf(".new was touched: %q", got)
	}
}

func failRenames(match func(from, to string) bool) (undo func()) {
	orig := renameFn
	renameFn = func(from, to string) error {
		if match(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EIO}
		}
		return orig(from, to)
	}
	return func() { renameFn = orig }
}

// Recovery that fails halfway puts state.json back, and a later save finishes the job.
func TestRecoveryThatFailsHalfwayLeavesNothingMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, sealed("main", 2), 0o600)
	os.WriteFile(path+".new", sealed("newer", 3), 0o600)
	undo := failRenames(func(from, _ string) bool { return from == path+".new" })
	s, got := openName(t, path)
	undo()
	if got != "newer" {
		t.Fatalf("got %q", got)
	}
	if got := fileName(t, path); got != "main" {
		t.Fatalf("state.json was not put back: %q", got)
	}
	if got := fileName(t, path+".new"); got != "newer" {
		t.Fatalf("the complete .new was lost: %q", got)
	}
	// The next save settles first, so the complete .new is installed, not truncated.
	s.Update(func(st *State) { st.BLE.Name = "latest" })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, got := openName(t, path); got != "latest" {
		t.Fatalf("after the save: %q", got)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("leftovers: %v", l)
	}
}

// A complete .new beside a missing state.json must survive the next save being cut.
func TestSaveSettlesBeforeItTouchesNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path+".new", sealed("only copy", 3), 0o600)
	undo := crashAt(path, "mid-write")
	err := writeFileAtomic(path, stateJSON("next"))
	undo()
	if err != errCrashed {
		t.Fatal(err)
	}
	if _, got := openName(t, path); got != "only copy" {
		t.Fatalf("got %q: the only complete copy was overwritten", got)
	}
}

// A first save whose final rename fails leaves the complete .new for Open.
func TestFailedFirstInstallKeepsNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	undo := failRenames(func(from, _ string) bool { return from == path+".new" })
	err := writeFileAtomic(path, stateJSON("first"))
	undo()
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, got := openName(t, path); got != "first" {
		t.Fatalf("got %q", got)
	}
}

// A failed install over an existing state.json puts the old one back.
func TestFailedInstallPutsTheOldFileBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeFileAtomic(path, stateJSON("old")); err != nil {
		t.Fatal(err)
	}
	undo := failRenames(func(from, _ string) bool { return from == path+".new" })
	err := writeFileAtomic(path, stateJSON("new"))
	undo()
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := fileName(t, path); got != "old" {
		t.Fatalf("state.json holds %q", got)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("leftovers: %v", l)
	}
}

// A longer, stale .new must not leave a tail behind the new content.
func TestStaleLongerNewLeavesNoTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path+".new", []byte(strings.Repeat("x", 5000)), 0o600)
	if err := writeFileAtomic(path, stateJSON("short")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if _, _, ok := unseal(b); !ok || len(b) > 400 {
		t.Fatalf("state.json is %d bytes, seal valid=%v", len(b), ok)
	}
}

// A stale .old (a previous cut) is gone after the next save.
func TestStaleOldIsRemovedBySave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFileAtomic(path, stateJSON("one"))
	os.WriteFile(path+".old", sealed("stale", 1), 0o600)
	if err := writeFileAtomic(path, stateJSON("two")); err != nil {
		t.Fatal(err)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("leftovers: %v", l)
	}
}
