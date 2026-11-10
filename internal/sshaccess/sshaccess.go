// Package sshaccess lets the web UI switch SSH access to the appliance on and
// off. It exists only on the SD-card image, which ships Dropbear and a unit,
// emm-ssh.service, that nothing starts by itself.
//
// What "on" means: a host key kept on the boot partition (so the fingerprint
// survives reboots, although the root filesystem lives in RAM), root's password
// in /etc/shadow and/or public keys in /root/.ssh/authorized_keys, taken from
// the daemon's saved settings, and the unit running. "Off" stops the unit and
// puts root's password back to empty (the console login the image has always had)
// and removes the keys. Nothing here survives a reboot by itself: emmd applies
// the saved settings at every start.
//
// Dropbear refuses a login with an empty password, so SSH never works without
// a password or a key being set first.
package sshaccess

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// Config is what the user chose.
type Config struct {
	Enabled bool
	// PasswordHash is root's SSH password as a crypt string (see HashPassword);
	// empty means no password login.
	PasswordHash string
	// AuthorizedKeys are public key lines for root.
	AuthorizedKeys []string
}

// Controller applies a Config to the system.
type Controller struct {
	// Dir holds the host key; it should be on persistent storage.
	Dir string
	// The following default to the appliance's paths.
	EnvFile    string // read by the unit: where the dropbear arguments go
	ShadowFile string
	RootHome   string
	Unit       string
	Log        *slog.Logger
	// Run runs a command and returns its combined output. Defaults to os/exec.
	Run func(name string, args ...string) ([]byte, error)

	mu          sync.Mutex
	applied     *Config
	fingerprint string
}

func (c *Controller) init() {
	if c.EnvFile == "" {
		c.EnvFile = "/run/emm-ssh.env"
	}
	if c.ShadowFile == "" {
		c.ShadowFile = "/etc/shadow"
	}
	if c.RootHome == "" {
		c.RootHome = "/root"
	}
	if c.Unit == "" {
		c.Unit = "emm-ssh.service"
	}
	if c.Run == nil {
		c.Run = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Available reports whether this system is the appliance image with SSH
// support: the release stamp its build writes, and the SSH server.
func Available() bool {
	if _, err := os.Stat("/etc/emm-release"); err != nil {
		return false
	}
	for _, bin := range []string{"dropbear", "dropbearkey", "systemctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}

func (c *Controller) keyFile() string { return filepath.Join(c.Dir, "dropbear_ed25519_host_key") }

// Apply makes the system match cfg. Applying the same settings twice does
// nothing the second time, so a running session is not cut off.
func (c *Controller) Apply(cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if c.applied != nil && reflect.DeepEqual(*c.applied, cfg) {
		return nil
	}
	if cfg.Enabled {
		if err := c.enable(cfg); err != nil {
			return err
		}
	} else if err := c.disable(); err != nil {
		return err
	}
	cp := cfg
	cp.AuthorizedKeys = append([]string(nil), cfg.AuthorizedKeys...)
	c.applied = &cp
	return nil
}

func (c *Controller) enable(cfg Config) error {
	if cfg.PasswordHash == "" && len(cfg.AuthorizedKeys) == 0 {
		return errors.New("set a password or a public key first: SSH cannot be used without one")
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", c.Dir, err)
	}
	if _, err := os.Stat(c.keyFile()); err != nil {
		if out, err := c.Run("dropbearkey", "-t", "ed25519", "-f", c.keyFile()); err != nil {
			return fmt.Errorf("cannot create the SSH host key: %v: %s", err, strings.TrimSpace(string(out)))
		}
		c.fingerprint = ""
	}
	if err := c.setRootPassword(cfg.PasswordHash); err != nil {
		return err
	}
	if err := c.writeKeys(cfg.AuthorizedKeys); err != nil {
		return err
	}
	// -s: no password logins when there is no password (the empty one is refused
	// anyway, this makes it explicit); -T: attempts per connection.
	args := "-r " + c.keyFile() + " -p 22 -T 3"
	if cfg.PasswordHash == "" {
		args += " -s"
	}
	if err := os.WriteFile(c.EnvFile, []byte("DROPBEAR_ARGS="+args+"\n"), 0o644); err != nil {
		return err
	}
	// restart, not start: new settings must reach a server that is already running.
	if out, err := c.Run("systemctl", "restart", c.Unit); err != nil {
		return fmt.Errorf("cannot start SSH: %v: %s", err, strings.TrimSpace(string(out)))
	}
	c.Log.Info("SSH access is on", "password", cfg.PasswordHash != "", "keys", len(cfg.AuthorizedKeys))
	return nil
}

func (c *Controller) disable() error {
	var errs []error
	if out, err := c.Run("systemctl", "stop", c.Unit); err != nil {
		errs = append(errs, fmt.Errorf("cannot stop SSH: %v: %s", err, strings.TrimSpace(string(out))))
	}
	if err := c.setRootPassword(""); err != nil {
		errs = append(errs, err)
	}
	if err := c.writeKeys(nil); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		c.Log.Info("SSH access is off")
	}
	return errors.Join(errs...)
}

// Status reports whether the server is running and the host key's fingerprint
// (empty until a key exists).
func (c *Controller) Status() (running bool, fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if _, err := c.Run("systemctl", "is-active", "--quiet", c.Unit); err == nil {
		running = true
	}
	if c.fingerprint == "" {
		if _, err := os.Stat(c.keyFile()); err == nil {
			if out, err := c.Run("dropbearkey", "-y", "-f", c.keyFile()); err == nil {
				for _, l := range strings.Split(string(out), "\n") {
					if f, ok := strings.CutPrefix(strings.TrimSpace(l), "Fingerprint: "); ok {
						c.fingerprint = f
					}
				}
			}
		}
	}
	return running, c.fingerprint
}

// setRootPassword puts hash into root's entry in the shadow file ("" empties it).
func (c *Controller) setRootPassword(hash string) error {
	if strings.ContainsAny(hash, ":\n\r") {
		return errors.New("invalid password hash")
	}
	data, err := os.ReadFile(c.ShadowFile)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", c.ShadowFile, err)
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, l := range lines {
		f := strings.Split(l, ":")
		if len(f) >= 2 && f[0] == "root" {
			if f[1] == hash {
				return nil
			}
			f[1] = hash
			lines[i] = strings.Join(f, ":")
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no root entry in %s", c.ShadowFile)
	}
	return writeKeepingMode(c.ShadowFile, []byte(strings.Join(lines, "\n")))
}

func (c *Controller) writeKeys(keys []string) error {
	dir := filepath.Join(c.RootHome, ".ssh")
	file := filepath.Join(dir, "authorized_keys")
	if len(keys) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(file, 0o600)
}

// writeKeepingMode replaces path atomically, keeping its permissions.
func writeKeepingMode(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".emm-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ParseKeys checks pasted authorized_keys text and returns the cleaned lines.
// Only plain "<type> <base64> [comment]" lines are accepted: no options (a
// "command=" prefix would be a way to surprise the owner), comments or blanks
// are skipped.
func ParseKeys(text string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for i, raw := range strings.Split(text, "\n") {
		l := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 || !validKeyType(f[0]) || !validBase64(f[1]) {
			return nil, fmt.Errorf("line %d is not a public key (expected \"ssh-ed25519 AAAA… comment\")", i+1)
		}
		if len(l) > 2000 {
			return nil, fmt.Errorf("line %d is too long", i+1)
		}
		l = strings.Join(f, " ")
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > 20 {
		return nil, errors.New("too many keys (max 20)")
	}
	return out, nil
}

func validKeyType(t string) bool {
	switch t {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	}
	return false
}

func validBase64(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
		default:
			return false
		}
	}
	return true
}
