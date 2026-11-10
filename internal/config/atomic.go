package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Crash-safe saving of one file on a filesystem without a journal (FAT32).
//
// A save never replaces anything. It
//
//  1. settles whatever an earlier interrupted save left (see settle),
//  2. writes the whole new content, sealed (below), to path.new and syncs it,
//  3. moves path aside to path.old,
//  4. renames path.new to path,
//  5. removes path.old.
//
// Every step creates a file, renames to a free name, or removes one; none of
// them replaces an existing file, which on vfat is several directory writes that
// a power cut can tear into two entries with one name, or none.
//
// A file is "sealed" by a last key, "_save", holding a sequence number and the
// SHA-256 of everything before it and of the number itself. That makes two things checkable that a plain
// JSON parse does not: whether the file is whole (FAT can leave a file of the
// right size whose clusters still hold an older save, or a mix), and which copy
// is the newest. Open and every later save use scanCopies and settle: of
// path.new, path and path.old the best copy wins, a sealed one over one that only
// parses (a file from before sealing, or one edited by hand), then the highest
// sequence number. So at any instant of any save, and at a second power cut during
// the recovery, the state that comes back is the one from before the save or the
// one it was writing, never an empty one while a good copy exists.

// Suffixes of the files a save works with.
const (
	newSuffix = ".new"
	oldSuffix = ".old"
)

// testHook lets a test stop a save at a named point, as a power cut would: it
// returns true to "crash" there, and writeFileAtomic then returns errCrashed
// at once without cleaning anything up. It is told which file is being saved so
// that a test only crashes its own saves, not those of a store another test left
// running. renameFn is os.Rename; a test swaps it to make renames fail. Both are
// untouched in production.
var (
	testHook   atomic.Pointer[func(path, step string) bool]
	errCrashed = errors.New("simulated power cut")
	renameFn   = os.Rename
)

// isObject is the check a save makes of what is on the card: a JSON object. (Open
// applies the stricter validState / validSystem of each file.)
func isObject(data []byte) error {
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' || !json.Valid(data) {
		return errors.New("not a JSON object")
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { // best effort: FAT may refuse
		_ = d.Sync()
		d.Close()
	}
}

// ---- sealing ----

const sealMarker = "\n  \"_save\": "

// seal appends the "_save" key to a JSON object. Anything that is not an object
// is returned as it is (it can then only ever be a "suspect" copy).
func seal(data []byte, seq uint64) []byte {
	s := bytes.TrimRight(data, " \t\r\n")
	if len(s) == 0 || s[len(s)-1] != '}' {
		return data
	}
	p := bytes.TrimRight(s[:len(s)-1], " \t\r\n")
	sum := sealSum(p, seq)
	var b bytes.Buffer
	b.Write(p)
	if !bytes.HasSuffix(p, []byte("{")) {
		b.WriteByte(',')
	}
	fmt.Fprintf(&b, "%s{\"seq\": %d, \"sha256\": \"%s\"}\n}\n", sealMarker, seq, hex.EncodeToString(sum[:]))
	return b.Bytes()
}

// sealSum is the checksum in the seal: of the content and of the sequence number.
func sealSum(content []byte, seq uint64) [32]byte {
	h := sha256.New()
	h.Write(content)
	fmt.Fprintf(h, "\n%d", seq)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// unseal reports the sequence number in a file's "_save" key, whether there is
// one, and whether its checksum matches the content before it.
func unseal(b []byte) (seq uint64, sealed, valid bool) {
	i := bytes.LastIndex(b, []byte(sealMarker))
	if i < 0 {
		return 0, false, false
	}
	tail := b[i+len(sealMarker):]
	j := bytes.IndexByte(tail, '}')
	if j < 0 {
		return 0, true, false
	}
	var t struct {
		Seq uint64 `json:"seq"`
		Sum string `json:"sha256"`
	}
	if err := json.Unmarshal(tail[:j+1], &t); err != nil {
		return 0, true, false
	}
	if string(bytes.TrimSpace(tail[j+1:])) != "}" {
		return t.Seq, true, false
	}
	p := bytes.TrimSuffix(b[:i], []byte(","))
	sum := sealSum(p, t.Seq)
	return t.Seq, true, hex.EncodeToString(sum[:]) == t.Sum
}

// ---- reading what is on the card ----

type rank int

const (
	rankInvalid rank = iota // absent, unreadable or not a state
	rankSuspect             // parses, but is not sealed or the seal does not match
	rankSealed              // parses and the checksum matches
)

// fileCopy is one of the files a save works with, as found on the card.
type fileCopy struct {
	path string
	data []byte
	err  error // nil, os.ErrNotExist (absent) or a read error
	seq  uint64
	rank rank
}

func (c *fileCopy) exists() bool { return c.err == nil }

func readCopy(path string, validate func([]byte) error, read func(string) ([]byte, error)) fileCopy {
	b, err := read(path)
	c := fileCopy{path: path, data: b, err: err}
	if err != nil || validate(b) != nil {
		return c
	}
	seq, sealed, ok := unseal(b)
	c.seq = seq
	if sealed && ok {
		c.rank = rankSealed
	} else {
		c.rank = rankSuspect
	}
	return c
}

type copies struct{ nw, main, old fileCopy }

// scanCopies reads the three files with read (os.ReadFile, or the test-replaceable
// readFile when opening a store).
func scanCopies(path string, validate func([]byte) error, read func(string) ([]byte, error)) copies {
	return copies{
		nw:   readCopy(path+newSuffix, validate, read),
		main: readCopy(path, validate, read),
		old:  readCopy(path+oldSuffix, validate, read),
	}
}

func suffixOf(which string) string {
	switch which {
	case "new":
		return newSuffix
	case "old":
		return oldSuffix
	}
	return ""
}

// best picks the copy to run on: of the sealed ones the highest sequence number
// (on a tie .new, then state.json, then .old); if none is sealed, the first that
// parses of state.json, .old, .new. which is "new", "main" or "old".
func (c *copies) best() (*fileCopy, string) {
	return c.bestOf(&c.nw, &c.main, &c.old)
}

// bestOf is best over some of the copies (given in the order of preference on a tie).
func (c *copies) bestOf(list ...*fileCopy) (*fileCopy, string) {
	name := func(f *fileCopy) string {
		switch f {
		case &c.nw:
			return "new"
		case &c.old:
			return "old"
		}
		return "main"
	}
	var best *fileCopy
	for _, f := range list {
		if f.rank == rankSealed && (best == nil || f.seq > best.seq) {
			best = f
		}
	}
	if best != nil {
		return best, name(best)
	}
	for _, f := range []*fileCopy{&c.main, &c.old, &c.nw} {
		if f.rank == rankSuspect {
			for _, l := range list {
				if l == f {
					return f, name(f)
				}
			}
		}
	}
	return nil, ""
}

// ---- settling ----

func removeIfExists(c *fileCopy) bool {
	return c.exists() && os.Remove(c.path) == nil
}

// asideName is a free name for a file that is kept but no longer wanted.
func asideName(path string) string {
	base := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// settle makes the card tidy after a possibly interrupted save: the best copy is
// state.json, and the other files of the save are gone. A state.json that is not
// the best (damaged, or older than a complete .new) is moved aside, never
// replaced: a copy that still parses and was sealed goes to .old and is dropped
// once the best copy is in place; anything else is kept as path.corrupt-<unix>.
// Only the first step that changes anything can leave the card without a
// state.json, and a failure after it puts the file back. Settling is idempotent,
// so a power cut during it is just another interrupted save.
func settle(path string, c copies, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Dir(path)
	best, which := c.best()
	switch which {
	case "":
		if c.main.exists() { // present but not a state: keep it for the user, start over
			aside := asideName(path)
			log.Error("state file is corrupt; starting empty", "path", path, "moved_to", aside)
			if err := renameFn(path, aside); err != nil {
				log.Warn("could not move the corrupt state file aside", "err", err)
			}
			syncDir(dir)
		}
		return nil
	case "main":
		if removeIfExists(&c.nw) || removeIfExists(&c.old) {
			syncDir(dir)
		}
		return nil
	}

	// .new or .old is the best copy: it goes to state.json.
	aside, discard := "", false
	if c.main.exists() {
		if c.main.rank == rankSealed && which == "new" {
			aside, discard = path+oldSuffix, true // an older, whole save: not worth keeping
			_ = os.Remove(aside)
		} else {
			aside = asideName(path)
		}
		if err := renameFn(path, aside); err != nil {
			return err
		}
		if !discard {
			log.Warn("the state file was damaged or older than a complete save; kept", "as", aside)
		}
	}
	if err := renameFn(best.path, path); err != nil {
		if aside != "" {
			_ = renameFn(aside, path)
		}
		return err
	}
	if discard {
		_ = os.Remove(aside)
	}
	other := &c.nw
	if which == "new" {
		other = &c.old
	}
	removeIfExists(other)
	syncDir(dir)
	return nil
}

// ---- saving ----

// writeFileAtomic saves data as path so that a power cut at any point leaves
// the file as it was or the new one for Open to find (see the top of this file).
// data should be a JSON object; it is sealed here.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	crash := func(step string) bool {
		h := testHook.Load()
		return h != nil && (*h)(path, step)
	}
	newP, oldP := path+newSuffix, path+oldSuffix

	// Start from a settled card, so that the files below are the only ones there
	// and the next sequence number is known.
	c := scanCopies(path, isObject, os.ReadFile)
	if c.main.err != nil && !errors.Is(c.main.err, os.ErrNotExist) {
		return c.main.err // unreadable: never replaced
	}
	seq := uint64(1)
	if best, _ := c.best(); best != nil {
		seq = best.seq + 1
	}
	if err := settle(path, c, nil); err != nil {
		return err
	}
	data = seal(data, seq)

	// The file holds the WiFi passphrase in clear text and the web password
	// hash: owner only (FAT has no modes, so the chmod below is best effort).
	f, err := os.OpenFile(newP, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(newP)
		return err
	}
	half := len(data) / 2
	if _, err := f.Write(data[:half]); err != nil {
		return fail(err)
	}
	if crash("mid-write") {
		return errCrashed
	}
	if _, err := f.Write(data[half:]); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(newP)
		return err
	}
	_ = os.Chmod(newP, 0o600)
	syncDir(dir)
	if crash("new-written") {
		return errCrashed
	}

	aside := false
	if _, err := os.Lstat(path); err == nil {
		// Free the name .old (settle left nothing there; this is belt and braces),
		// then move the current file there: neither step replaces anything.
		if err := os.Remove(oldP); err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Remove(newP)
			return err
		}
		if crash("old-removed") {
			return errCrashed
		}
		if err := renameFn(path, oldP); err != nil {
			os.Remove(newP)
			return err
		}
		aside = true
		syncDir(dir)
		if crash("moved-aside") {
			return errCrashed
		}
	}
	if err := renameFn(newP, path); err != nil {
		if !aside {
			return err // first save: .new is the only copy of the new state; Open finds it
		}
		if rerr := renameFn(oldP, path); rerr != nil {
			return fmt.Errorf("%w (and the old file could not be put back: %v)", err, rerr)
		}
		os.Remove(newP)
		return err
	}
	syncDir(dir)
	if crash("installed") {
		return errCrashed
	}
	_ = os.Remove(oldP)
	syncDir(dir)
	return nil
}
