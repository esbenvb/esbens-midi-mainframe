package sshaccess

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reference values come from `openssl passwd -6 -salt <salt> <password>`.
func TestSHA512CryptMatchesOpenSSL(t *testing.T) {
	for _, c := range []struct{ pw, salt, want string }{
		{"Hello world!", "saltstring", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"a", "0123456789abcdef", "$6$0123456789abcdef$qn2kMgR/Cs9pZI2hZfok3cbi0oHyRjH7NUyfGhPKp6EBvlZWbL3JMmASYlyEBxhP4jB3zti0119AKDkHdAVHI0"},
		{"a much longer password than 64 bytes, to cover the chunked part of digest A.....xyz", "abcdefghijklmnop", "$6$abcdefghijklmnop$Ma.jINZmBCss9Lj.NMu/1eWio0.HXKu9wPiSV/umNleBcmxamloHlr/mKrLvi89xqlzuvEEsRWCt3l316FE.d1"},
		{"x", "s", "$6$s$SBlV1TrR7m/.1RRLMAAvh/x6acOyBKfXVqXOixcaFNbUhqE84RFnq6y5mvfWTW8EA6yIr1RuWU8IRsKA/9jfE."},
		{"pw", "toolongsaltvalue_ABCDEFGH", "$6$toolongsaltvalue$a2/amHaR8aDtI5REgaIE25iCUQ6Ea9vfoNWB87011BJVSAS6jmbhIB8rAUYctmgGNoLrPrckzCpmw/u0y4gBj0"},
	} {
		if got := sha512Crypt([]byte(c.pw), []byte(c.salt), defaultRounds); got != c.want {
			t.Errorf("sha512Crypt(%q, %q):\n got %s\nwant %s", c.pw, c.salt, got, c.want)
		}
	}
	if got := sha512Crypt([]byte("x"), []byte("s"), 1000); !strings.HasPrefix(got, "$6$rounds=1000$s$") {
		t.Errorf("non-default rounds are not written out: %s", got)
	}
}

func TestHashPasswordIsSaltedAndShaped(t *testing.T) {
	a, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := HashPassword("correct horse")
	if a == b {
		t.Error("two hashes of one password are identical: the salt is not random")
	}
	parts := strings.Split(a, "$")
	if len(parts) != 4 || parts[1] != "6" || len(parts[2]) != 16 || len(parts[3]) != 86 {
		t.Errorf("unexpected shape: %s", a)
	}
	if strings.ContainsAny(a, ":\n ") {
		t.Errorf("not safe for /etc/shadow: %q", a)
	}
	if _, err := HashPassword(""); err == nil {
		t.Error("an empty password must be refused")
	}
}

func TestParseKeys(t *testing.T) {
	ed := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl esben@laptop"
	got, err := ParseKeys("# my keys\n\n" + ed + "\r\n" + ed + "\n  ssh-rsa   AAAAB3NzaC1yc2EAAAADAQABAAABAQC7   work \n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != ed || got[1] != "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 work" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{
		"command=\"rm -rf /\" " + ed,
		"hello world",
		"ssh-ed25519",
		"ssh-ed25519 not*base64*at*all!!",
		"ssh-dss AAAAB3NzaC1kc3MAAACBAP1",
	} {
		if _, err := ParseKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if k, err := ParseKeys("\n  \n# only comments\n"); err != nil || len(k) != 0 {
		t.Errorf("empty input: %v %v", k, err)
	}
}

type fixture struct {
	c    *Controller
	root string
	cmds []string
	fail map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{root: dir, fail: map[string]bool{}}
	shadow := filepath.Join(dir, "shadow")
	if err := os.WriteFile(shadow, []byte("root::10933:0:99999:7:::\nbin:*:10933:0:99999:7:::\nnobody:*:10933:0:99999:7:::\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.c = &Controller{
		Dir: filepath.Join(dir, "boot", "ssh"), EnvFile: filepath.Join(dir, "emm-ssh.env"),
		ShadowFile: shadow, RootHome: filepath.Join(dir, "root"), Unit: "emm-ssh.service",
		Run: func(name string, args ...string) ([]byte, error) {
			cmd := name + " " + strings.Join(args, " ")
			f.cmds = append(f.cmds, cmd)
			if f.fail[name] {
				return []byte("boom"), errors.New("exit 1")
			}
			switch {
			case name == "dropbearkey" && args[0] == "-t":
				return nil, os.WriteFile(args[3], []byte("key"), 0o600)
			case name == "dropbearkey" && args[0] == "-y":
				return []byte("Public key portion is:\nssh-ed25519 AAAA root@emm\nFingerprint: SHA256:abcdef\n"), nil
			}
			return nil, nil
		},
	}
	return f
}

func (f *fixture) rootHash(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.c.ShadowFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.Split(string(b), "\n")[0], ":")[1]
}

func (f *fixture) count(prefix string) int {
	n := 0
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl esben@laptop"

func TestEnableWithPassword(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Apply(Config{Enabled: true, PasswordHash: "$6$salt$hash"}); err != nil {
		t.Fatal(err)
	}
	if h := f.rootHash(t); h != "$6$salt$hash" {
		t.Errorf("root hash %q", h)
	}
	b, _ := os.ReadFile(f.c.ShadowFile)
	if !strings.Contains(string(b), "bin:*:10933") || !strings.HasSuffix(string(b), "\n") {
		t.Errorf("the rest of the shadow file changed: %q", b)
	}
	if fi, _ := os.Stat(f.c.ShadowFile); fi.Mode().Perm() != 0o640 {
		t.Errorf("shadow mode %v", fi.Mode().Perm())
	}
	env, _ := os.ReadFile(f.c.EnvFile)
	if !strings.Contains(string(env), "-r "+f.c.keyFile()) || strings.Contains(string(env), " -s") {
		t.Errorf("env file: %q", env)
	}
	if f.count("dropbearkey -t ed25519") != 1 || f.count("systemctl restart emm-ssh.service") != 1 {
		t.Errorf("commands: %v", f.cmds)
	}
	if _, err := os.Stat(filepath.Join(f.c.RootHome, ".ssh", "authorized_keys")); err == nil {
		t.Error("authorized_keys written without keys")
	}
}

func TestEnableWithKeyOnlyForbidsPasswords(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Apply(Config{Enabled: true, AuthorizedKeys: []string{testKey}}); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(f.c.EnvFile)
	if !strings.Contains(string(env), " -s") {
		t.Errorf("password logins not switched off: %q", env)
	}
	file := filepath.Join(f.c.RootHome, ".ssh", "authorized_keys")
	b, err := os.ReadFile(file)
	if err != nil || string(b) != testKey+"\n" {
		t.Fatalf("authorized_keys: %q %v", b, err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys mode %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(file)); di.Mode().Perm() != 0o700 {
		t.Errorf(".ssh mode %v", di.Mode().Perm())
	}
	if h := f.rootHash(t); h != "" {
		t.Errorf("a password was set: %q", h)
	}
}

func TestEnableNeedsAPasswordOrAKey(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Apply(Config{Enabled: true}); err == nil {
		t.Fatal("enabled with no way to log in")
	}
	if f.count("systemctl") != 0 {
		t.Errorf("the service was touched: %v", f.cmds)
	}
}

func TestDisableRestoresTheEmptyPasswordAndRemovesKeys(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Apply(Config{Enabled: true, PasswordHash: "$6$salt$hash", AuthorizedKeys: []string{testKey}}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Apply(Config{}); err != nil {
		t.Fatal(err)
	}
	if h := f.rootHash(t); h != "" {
		t.Errorf("root hash %q after disabling", h)
	}
	if _, err := os.Stat(filepath.Join(f.c.RootHome, ".ssh", "authorized_keys")); err == nil {
		t.Error("authorized_keys is still there")
	}
	if f.count("systemctl stop emm-ssh.service") != 1 {
		t.Errorf("commands: %v", f.cmds)
	}
}

func TestApplyingTheSameSettingsTwiceDoesNothing(t *testing.T) {
	f := newFixture(t)
	cfg := Config{Enabled: true, PasswordHash: "$6$s$h", AuthorizedKeys: []string{testKey}}
	_ = f.c.Apply(cfg)
	n := len(f.cmds)
	if err := f.c.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != n {
		t.Errorf("second Apply ran commands: %v", f.cmds[n:])
	}
	// A change restarts the server so it sees it, and keeps the host key.
	cfg.PasswordHash = "$6$s$other"
	_ = f.c.Apply(cfg)
	if f.count("systemctl restart") != 2 || f.count("dropbearkey -t") != 1 {
		t.Errorf("commands: %v", f.cmds)
	}
}

func TestFailuresAreReportedAndRetried(t *testing.T) {
	f := newFixture(t)
	f.fail["systemctl"] = true
	if err := f.c.Apply(Config{Enabled: true, PasswordHash: "$6$s$h"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error: %v", err)
	}
	f.fail["systemctl"] = false
	if err := f.c.Apply(Config{Enabled: true, PasswordHash: "$6$s$h"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.count("systemctl restart") != 2 {
		t.Errorf("a failed apply was remembered as done: %v", f.cmds)
	}

	g := newFixture(t)
	g.fail["dropbearkey"] = true
	if err := g.c.Apply(Config{Enabled: true, PasswordHash: "$6$s$h"}); err == nil {
		t.Error("host key failure was ignored")
	}
	if g.count("systemctl") != 0 {
		t.Error("the server was started without a host key")
	}
}

func TestRejectsBadHashes(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Apply(Config{Enabled: true, PasswordHash: "x:0:0\nroot::0"}); err == nil {
		t.Error("a hash that would corrupt /etc/shadow was accepted")
	}
}

func TestStatusReportsFingerprintOnceAKeyExists(t *testing.T) {
	f := newFixture(t)
	if run, fp := f.c.Status(); fp != "" || !run && false {
		t.Errorf("fingerprint before a key exists: %q", fp)
	}
	_ = f.c.Apply(Config{Enabled: true, PasswordHash: "$6$s$h"})
	run, fp := f.c.Status()
	if !run || fp != "SHA256:abcdef" {
		t.Errorf("status: running=%v fingerprint=%q", run, fp)
	}
	f.fail["systemctl"] = true
	if run, _ := f.c.Status(); run {
		t.Error("a stopped unit is reported as running")
	}
}
