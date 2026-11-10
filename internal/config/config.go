// Package config holds EMM's persisted state (state.json): connections,
// per-port settings, the last-known description of every port that has been
// seen, network port allocations and the settings the web UI can change.
//
// The file is rewritten rarely, and so that a power cut at any moment leaves a
// complete state behind: the appliance keeps it on a FAT32 partition, which has
// no journal. A save never replaces a file (that is several directory writes on
// FAT); it writes a sealed copy, state.json.new, and swaps it in, and Open picks
// the newest complete copy of state.json, .new and .old. The protocol, and what
// "sealed" means, is described in atomic.go.
//
// The settings live in two files in the same folder, so that what is harmless
// and what is not can be treated differently (backed up, restored, shared):
//
//   - state.json: the application state: connections, per-port settings, the
//     remembered ports, presets, and the RTP and BLE switches.
//   - system.json: the device's network (WiFi, access point, Ethernet), the web
//     password hash and the SSH settings. Never part of a backup.
//
// State is one struct in memory; the Web, SSH and Network fields are tagged
// json:"-" so that encoding a State can never leak them, and the second file is
// written from them explicitly (marshalSystem). A file is only written when its
// own part changed. An installation from before the split has everything in
// state.json: Open moves the secrets to system.json (written first) and rewrites
// state.json without them.
//
// Open never fails because of the file, so that a damaged card cannot keep the
// device from starting, but it treats the three bad cases differently, by what
// can still be saved:
//
//   - Missing file: an empty state (or the copy an interrupted first save
//     left); the first save creates the file.
//   - Corrupt JSON with no good copy beside it: the file is moved aside
//     (path.corrupt-<unix>) and an empty state is used; saving works as usual.
//   - Unreadable file (an I/O error, permission denied, a directory in its
//     place, ...): the file may well hold good data that one read failed to
//     deliver (the web password hash and the saved WiFi among it), so it is never
//     replaced. The store starts empty in memory and is marked unreadable:
//     Update still changes the in-memory state, but nothing is written, Flush
//     reports an error, Writable is false (the UI warns that settings will not
//     survive a restart) and no retry is scheduled. Reading the file again takes
//     a restart; when the file is fine then, nothing was lost. Note that the
//     empty state has no web password, so the web UI is open until then.
package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"emm/internal/router"
)

// CurrentVersion is the schema version written to state.json.
const CurrentVersion = 1

// PortSettings are the user's per-port choices.
type PortSettings struct {
	Alias  string `json:"alias,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
	// RTP controls whether a USB port is exposed as its own network MIDI
	// endpoint. nil means "default", which is on.
	RTP *bool `json:"rtp,omitempty"`
	// RTPPort is the UDP control port allocated to this port's endpoint; it is
	// kept stable so remote sessions keep working across reboots.
	RTPPort int `json:"rtp_port,omitempty"`
	// RTPDefaultsDone records that the default connections between the port
	// and its network endpoint were created once, so deleting them sticks.
	RTPDefaultsDone bool `json:"rtp_defaults_done,omitempty"`
}

// RTPEnabled reports the effective RTP exposure of a port (default on).
func (p PortSettings) RTPEnabled() bool { return p.RTP == nil || *p.RTP }

// KnownPort remembers what a port looked like when it was last seen.
type KnownPort struct {
	Name     string          `json:"name"`
	Kind     router.PortKind `json:"kind"`
	Group    string          `json:"group,omitempty"`
	Input    bool            `json:"input"`
	Output   bool            `json:"output"`
	Detail   string          `json:"detail,omitempty"`
	LastSeen int64           `json:"last_seen"` // unix seconds
	// Twin records that a device of the same model, identified by its USB port
	// rather than a serial number, was attached at the same time: this is one of
	// several identical units, so it must never be mistaken for one that moved to
	// another hub port (see app.adoptMoved).
	Twin bool `json:"twin,omitempty"`
}

// VirtualRTP is an extra network port that is not tied to a device.
type VirtualRTP struct {
	ID   string `json:"id"` // "rtp:net-<slug>"
	Name string `json:"name"`
	Port int    `json:"port,omitempty"`
}

// Preset is a saved routing: the whole connection matrix with its filters, under
// a name, to bring back with one click.
type Preset struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Saved       int64             `json:"saved"` // unix seconds
	Connections []router.ConnSpec `json:"connections"`
}

// State is everything that is persisted.
type State struct {
	Version     int                     `json:"version"`
	Connections []router.ConnSpec       `json:"connections"`
	Ports       map[string]PortSettings `json:"ports"`
	Known       map[string]KnownPort    `json:"known"`
	VirtualRTP  []VirtualRTP            `json:"virtual_rtp,omitempty"`
	Presets     []Preset                `json:"presets,omitempty"`

	RTP struct {
		Enabled *bool `json:"enabled,omitempty"` // nil = on
	} `json:"rtp"`
	BLE struct {
		Enabled *bool  `json:"enabled,omitempty"` // nil = on
		Name    string `json:"name,omitempty"`
	} `json:"ble"`
	// Web, SSH and Network are stored in system.json, not state.json (see the
	// package comment); json:"-" keeps them out of anything that encodes a State.
	Web struct {
		Salt string `json:"salt,omitempty"`
		Hash string `json:"hash,omitempty"` // sha256(salt || password), hex
	} `json:"-"`
	// SSH is only used on the appliance image (see package sshaccess).
	SSH struct {
		Enabled bool `json:"enabled,omitempty"`
		// PasswordHash is root's SSH password as a crypt(3) string ($6$...).
		PasswordHash   string   `json:"password_hash,omitempty"`
		AuthorizedKeys []string `json:"authorized_keys,omitempty"`
	} `json:"-"`

	// Network is owned by package netcfg (its Saved struct); kept opaque here
	// so this package does not depend on it.
	Network json.RawMessage `json:"-"`
}

// systemFile is the part of a State that is stored in system.json.
type systemFile struct {
	Version int             `json:"version"`
	Web     any             `json:"web"`
	SSH     any             `json:"ssh"`
	Network json.RawMessage `json:"network,omitempty"`
}

// marshalSystem encodes the device's network, web password and SSH settings (the
// content of system.json).
func (s *State) marshalSystem(indent bool) ([]byte, error) {
	d := systemFile{Version: CurrentVersion, Web: s.Web, SSH: s.SSH, Network: s.Network}
	if indent {
		return json.MarshalIndent(&d, "", "  ")
	}
	return json.Marshal(&d)
}

// unmarshalSystem fills the Web, SSH and Network fields from the content of system.json.
func (s *State) unmarshalSystem(data []byte) error {
	var d struct{ Web, SSH, Network json.RawMessage }
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	var web = s.Web
	var ssh = s.SSH
	if len(d.Web) > 0 {
		if err := json.Unmarshal(d.Web, &web); err != nil {
			return err
		}
	}
	if len(d.SSH) > 0 {
		if err := json.Unmarshal(d.SSH, &ssh); err != nil {
			return err
		}
	}
	s.Web, s.SSH = web, ssh
	s.Network = nil
	if n := bytes.TrimSpace(d.Network); len(n) > 0 && string(n) != "null" {
		s.Network = append(json.RawMessage(nil), n...)
	}
	return nil
}

// legacySecrets are the keys that state.json held before system.json existed.
type legacySecrets struct {
	Web     json.RawMessage `json:"web"`
	SSH     json.RawMessage `json:"ssh"`
	Network json.RawMessage `json:"network"`
}

func (s *State) normalise() {
	if s.Ports == nil {
		s.Ports = map[string]PortSettings{}
	}
	if s.Known == nil {
		s.Known = map[string]KnownPort{}
	}
	s.Version = CurrentVersion
}

// RTPOn reports whether RTP-MIDI is enabled in the saved settings (default on).
func (s *State) RTPOn() bool { return s.RTP.Enabled == nil || *s.RTP.Enabled }

// BLEOn reports whether Bluetooth MIDI is enabled in the saved settings (default on).
func (s *State) BLEOn() bool { return s.BLE.Enabled == nil || *s.BLE.Enabled }

// SetWebPassword sets (or with "" clears) the web UI password.
func (s *State) SetWebPassword(pw string) {
	if pw == "" {
		s.Web.Salt, s.Web.Hash = "", ""
		return
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	s.Web.Salt = hex.EncodeToString(salt)
	s.Web.Hash = hashPassword(s.Web.Salt, pw)
}

// WebPasswordSet reports whether a UI password is saved.
func (s *State) WebPasswordSet() bool { return s.Web.Hash != "" }

// CheckWebPassword compares pw against the saved password in constant time.
func (s *State) CheckWebPassword(pw string) bool {
	if s.Web.Hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(s.Web.Salt, pw)), []byte(s.Web.Hash)) == 1
}

func hashPassword(salt, pw string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + pw))
	return hex.EncodeToString(sum[:])
}

// Retry delays for a save that failed: the first retry comes after
// defaultRetryBase, and the delay doubles with every further failure up to
// defaultRetryMax.
const (
	defaultRetryBase = 5 * time.Second
	defaultRetryMax  = 60 * time.Second
)

// Store owns the state file.
type Store struct {
	path    string // state.json
	sysPath string // system.json, next to it
	log     *slog.Logger

	// flushMu serialises Flush: at most one write is in flight, so a second
	// Flush waits for it instead of reporting success while the data is still
	// in a temp file, and snapshots reach the disk in the order they were
	// taken. Lock order: flushMu, then mu.
	flushMu sync.Mutex

	mu       sync.Mutex
	st       State
	cur      []byte // JSON of the state.json part of st as of the last Update/Open; lets no-op updates skip the write
	sysCur   []byte // the same for the system.json part
	dirty    bool   // state.json owes a save
	sysDirty bool   // system.json owes a save
	timer    *time.Timer
	retrying bool          // timer is a retry after a failed save, not the debounce timer
	retry    time.Duration // delay of the next retry; 0 while saves succeed
	writable bool
	lastErr  error
	// unreadable is why state.json could not be read at Open (see the package
	// comment). While it is set nothing is ever written.
	unreadable error
	// sysUnreadable is the same for system.json; it only stops system.json from being written.
	sysUnreadable error

	// Test hooks and tuning; set before the first Update.
	retryBase, retryMax time.Duration
	write               func(path string, data []byte) error

	// SaveDelay debounces writes (default 500 ms). Set before first Update.
	SaveDelay time.Duration
	// OnSaved, if set, is called after every write attempt with its error.
	OnSaved func(error)
}

// readFile is os.ReadFile; a variable so that tests can make the read fail the
// way a marginal card does, whoever runs them.
var readFile = os.ReadFile

// SystemFileName is system.json's name; it sits in the same folder as the state file.
const SystemFileName = "system.json"

// Open loads the state file at path and system.json next to it. For each file: a
// missing file yields nothing, unless a save was interrupted: then the newest
// complete copy among the file, .new and .old is used and put back (see the
// package comment and settle). A corrupt file is moved aside (<file>.corrupt-<unix>)
// and, when no other copy is good, nothing is used. A file that cannot be read at
// all (I/O error, permission, a directory in its place) is left exactly where it
// is: that file is never written (see Unreadable), and the store runs on the best
// other copy of it if there is one. Either way a damaged card never prevents the
// device from starting.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		path: path, sysPath: filepath.Join(filepath.Dir(path), SystemFileName),
		log: log.With("component", "config"), SaveDelay: 500 * time.Millisecond, writable: true,
		retryBase: defaultRetryBase, retryMax: defaultRetryMax, write: writeFileAtomic,
	}

	appData, appErr, appTidy := s.load(path, validState)
	sysData, sysErr, sysTidy := s.load(s.sysPath, validSystem)
	s.unreadable, s.sysUnreadable = appErr, sysErr
	if appErr != nil && sysData == nil && sysErr == nil {
		// system.json does not exist yet, so the secrets may be in the state.json that
		// could not be read (from before the split). Writing a system.json now would
		// hide them from the next start: do not.
		s.sysUnreadable = appErr
	}
	s.dirty, s.sysDirty = appTidy, sysTidy

	if appData != nil {
		s.st, _ = parseState(appData)
	}
	var leg legacySecrets
	if appData != nil {
		_ = json.Unmarshal(appData, &leg)
	}
	hasLegacy := len(leg.Web) > 0 || len(leg.SSH) > 0 || len(leg.Network) > 0
	if sysData != nil {
		if err := s.st.unmarshalSystem(sysData); err != nil { // checked by validSystem; belt and braces
			s.log.Error("system file is not usable", "path", s.sysPath, "err", err)
		}
	} else if hasLegacy {
		// state.json from before the split: it still has the secrets. Use them.
		if err := s.st.unmarshalSystem(legacyAsSystem(leg)); err != nil {
			s.log.Error("could not read the web, SSH and network settings from the old state file", "err", err)
		}
	}
	s.st.normalise()
	s.cur, _ = json.Marshal(&s.st)
	s.sysCur, _ = s.st.marshalSystem(false)

	if hasLegacy {
		s.log.Info("moving the web password, SSH and network settings from state.json to system.json")
		if sysData == nil && sysErr == nil {
			// The secrets go to system.json first, and state.json is only rewritten
			// without them once that has worked: until then the old file keeps everything.
			s.mu.Lock()
			appOwed := s.dirty
			s.dirty, s.sysDirty = false, true
			s.mu.Unlock()
			if err := s.Flush(); err != nil {
				s.mu.Lock()
				s.dirty = appOwed
				s.mu.Unlock()
				return s, nil // the next save retries the migration step by step
			}
		}
		if appErr == nil {
			s.mu.Lock()
			s.dirty = true // state.json is rewritten without the secrets
			s.mu.Unlock()
			_ = s.Flush()
		}
	}
	return s, nil
}

// legacyAsSystem presents the secrets found in an old state.json as a system.json document.
func legacyAsSystem(l legacySecrets) []byte {
	b, _ := json.Marshal(l)
	return b
}

// load reads one of the two files for Open. It returns the content to run on
// (nil when there is none), why the file cannot be read (nil if it can), and
// whether tidying up after an interrupted save failed, which makes the file owe a save.
func (s *Store) load(path string, validate func([]byte) error) (data []byte, unreadable error, tidyFailed bool) {
	c := scanCopies(path, validate, readFile)
	if c.main.err != nil && !errors.Is(c.main.err, os.ErrNotExist) {
		s.log.Error("cannot read state file; leaving the file alone: nothing will be saved to it until emmd is restarted and can read it",
			"path", path, "err", c.main.err)
		// A complete copy left by an interrupted save is still better than nothing.
		if best, which := c.bestOf(&c.nw, &c.old); best != nil {
			s.log.Warn("running on the copy an interrupted save left", "path", path, "from", which)
			return best.data, c.main.err, false
		}
		return nil, c.main.err, false
	}
	best, which := c.best()
	if best != nil && which != "main" {
		s.log.Warn("recovering from an interrupted save", "from", filepath.Base(path)+suffixOf(which))
	}
	if err := settle(path, c, s.log); err != nil {
		s.log.Warn("could not tidy up after an interrupted save; the next save will try again", "path", path, "err", err)
		tidyFailed = true
	}
	if best == nil {
		return nil, nil, tidyFailed
	}
	return best.data, nil, tidyFailed
}

// parseState reads a state file's content. A file that a power cut left empty,
// zero-filled or cut short does not parse; "null" would, so it is refused too.
func parseState(data []byte) (State, error) {
	var st State
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' {
		return st, errors.New("not a JSON object")
	}
	err := json.Unmarshal(data, &st)
	return st, err
}

func validState(data []byte) error {
	_, err := parseState(data)
	return err
}

// validSystem says whether data is a system.json that can be used.
func validSystem(data []byte) error {
	if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' {
		return errors.New("not a JSON object")
	}
	var st State
	return st.unmarshalSystem(data)
}

// Path returns the state file path.
func (s *Store) Path() string { return s.path }

// SystemPath returns the path of system.json, which holds the network, web
// password and SSH settings.
func (s *Store) SystemPath() string { return s.sysPath }

// View calls fn with the live state under the lock. fn must not retain it.
func (s *Store) View(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

// Snapshot returns a deep copy of the state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloneLocked()
}

func (s *Store) cloneLocked() State {
	b, _ := json.Marshal(&s.st)
	var c State
	_ = json.Unmarshal(b, &c)
	if sys, err := s.st.marshalSystem(false); err == nil {
		_ = c.unmarshalSystem(sys)
	}
	c.normalise()
	return c
}

// Update mutates the state under the lock and schedules a save, unless fn
// changed nothing (so periodic reconciliation never rewrites the file). A save
// that is still owed from an earlier failure is not forgotten either way: it
// has its own retry timer (see Flush).
func (s *Store) Update(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	s.st.normalise()
	now, _ := json.Marshal(&s.st)
	nowSys, _ := s.st.marshalSystem(false)
	changed := false
	if !bytes.Equal(now, s.cur) {
		s.cur, s.dirty, changed = now, true, true
	}
	if !bytes.Equal(nowSys, s.sysCur) {
		s.sysCur, s.sysDirty, changed = nowSys, true, true
	}
	if changed && s.retrying && s.timer != nil {
		// A retry is waiting out its back-off; a real change is retried
		// after the normal debounce instead.
		s.timer.Stop()
		s.timer, s.retrying = nil, false
	}
	// A file that could not be read at startup is never written, so there is
	// nothing to schedule for it.
	if s.owesLocked() && s.timer == nil {
		s.timer = time.AfterFunc(s.SaveDelay, func() { _ = s.Flush() })
	}
}

// owesLocked reports whether a file that may be written owes a save. mu must be held.
func (s *Store) owesLocked() bool {
	return (s.dirty && s.unreadable == nil) || (s.sysDirty && s.sysUnreadable == nil)
}

// Flush writes pending changes now. Flushes are serialised: a Flush that
// arrives while another is writing waits for it, then writes whatever is still
// pending, and returns only once the data it covers is on disk (or the write
// failed). A failed write keeps the data pending and schedules a retry with
// back-off (5 s, doubling to 60 s), so a card that recovers gets its settings
// even if nothing else ever changes. A store whose file was unreadable at Open
// (see Unreadable) never writes: Flush then returns an error whenever there are
// changes, and does not retry.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	wrote, cb, err := s.flushLocked()
	s.flushMu.Unlock()
	if wrote && cb != nil {
		cb(err)
	}
	return err
}

// flushLocked does the work of Flush with flushMu held. It reports whether a
// write was attempted at all, the OnSaved callback and the write error.
func (s *Store) flushLocked() (wrote bool, cb func(error), err error) {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer, s.retrying = nil, false
	}
	if !s.dirty && !s.sysDirty {
		s.mu.Unlock()
		return false, nil, nil
	}
	// A file that could not be read at startup is not replaced: that would throw
	// away whatever it holds. Not a write attempt: no OnSaved, no retry timer, no
	// log line per call (Open said it once).
	var refused error
	type job struct {
		path string
		data []byte
		sys  bool
	}
	var jobs []job
	s.st.normalise()
	if s.sysDirty {
		if s.sysUnreadable != nil {
			refused = fmt.Errorf("settings not saved: %s could not be read at startup and is left untouched: %w", s.sysPath, s.sysUnreadable)
		} else if data, merr := s.st.marshalSystem(true); merr != nil {
			err = merr
		} else {
			jobs = append(jobs, job{s.sysPath, append(data, '\n'), true})
			s.sysDirty = false
		}
	}
	if s.dirty {
		if s.unreadable != nil {
			refused = fmt.Errorf("state not saved: %s could not be read at startup and is left untouched: %w", s.path, s.unreadable)
		} else if data, merr := json.MarshalIndent(&s.st, "", "  "); merr != nil {
			err = merr
		} else {
			jobs = append(jobs, job{s.path, append(data, '\n'), false})
			s.dirty = false
		}
	}
	write := s.write
	s.mu.Unlock()
	if len(jobs) == 0 {
		if err == nil {
			err = refused
		}
		return false, nil, err
	}
	// The secrets first: a changed password or WiFi is the one to get onto the card
	// before anything else, and a migration needs them in place before state.json
	// stops holding them.
	failed := map[bool]bool{}
	for _, j := range jobs {
		if werr := write(j.path, j.data); werr != nil {
			failed[j.sys] = true
			if err == nil {
				err = werr
			}
			s.log.Error("could not save state", "path", j.path, "err", werr)
		}
	}

	s.mu.Lock()
	s.lastErr = err
	s.writable = err == nil
	cb = s.OnSaved
	if failed[true] {
		s.sysDirty = true // still owed
	}
	if failed[false] {
		s.dirty = true
	}
	if err == nil {
		s.retry = 0
		err = refused // everything that could be written was; something was refused
	} else {
		s.armRetryLocked()
	}
	s.mu.Unlock()
	return true, cb, err
}

// armRetryLocked advances the back-off and, unless a timer is already pending
// (a change that arrived during the failed write has its own debounce timer),
// schedules the next attempt. mu must be held.
func (s *Store) armRetryLocked() {
	if s.retry == 0 {
		s.retry = s.retryBase
	} else {
		s.retry = min(2*s.retry, s.retryMax)
	}
	if s.timer == nil {
		s.timer = time.AfterFunc(s.retry, func() { _ = s.Flush() })
		s.retrying = true
	}
}

// Writable reports whether the last save attempt succeeded (true before any).
// It is always false for a store whose file was unreadable at Open.
func (s *Store) Writable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writable && s.unreadable == nil && s.sysUnreadable == nil
}

// Unreadable returns the error that kept Open from reading the state file, or
// nil. While it is not nil the store runs on an empty in-memory state and
// writes nothing, so that the file on disk stays as it was.
func (s *Store) Unreadable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unreadable != nil {
		return s.unreadable
	}
	return s.sysUnreadable
}

// CheckWritable probes the state directory for write access without touching
// state.json, so the UI can warn before the first change is lost.
func (s *Store) CheckWritable() bool {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.setWritable(false)
		return false
	}
	f, err := os.CreateTemp(dir, ".emm-probe-*")
	if err != nil {
		s.setWritable(false)
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	s.setWritable(true)
	return true
}

func (s *Store) setWritable(v bool) {
	s.mu.Lock()
	s.writable = v
	s.mu.Unlock()
}
