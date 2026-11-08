package netcfg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs an external command and returns its stdout. It exists so the
// whole package can be tested with a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// RunError is returned by ExecRunner when a command fails.
type RunError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *RunError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("%s: %v", e.Cmd, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Cmd, e.Err, msg)
}

func (e *RunError) Unwrap() error { return e.Err }

// ExecRunner runs real commands.
type ExecRunner struct {
	// Timeout bounds each command (default 60 s); the caller's context may be
	// shorter.
	Timeout time.Duration
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	to := r.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = cmdEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &RunError{Cmd: name + " " + strings.Join(args[:min(len(args), 3)], " "), Stderr: errb.String(), Err: err}
	}
	return out.String(), nil
}

// cmdEnv returns base with the locale pinned to "C". nmcli and friends
// translate their messages and (outside terse mode) their state words, and the
// package matches English text, so every command runs untranslated whatever
// LANG the system or the service manager hands the daemon. LANGUAGE is cleared
// as well: gettext prefers it over LC_ALL for anything but the exact "C" locale.
func cmdEnv(base []string) []string {
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "LC_ALL", "LANG", "LANGUAGE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C", "LANG=C", "LANGUAGE=")
}

// isNotFound reports whether err means the executable does not exist.
func isNotFound(err error) bool {
	var ee *exec.Error
	return errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound)
}

// stderrOf returns the stderr of a RunError, or the error text.
func stderrOf(err error) string {
	var re *RunError
	if errors.As(err, &re) && strings.TrimSpace(re.Stderr) != "" {
		return strings.TrimSpace(re.Stderr)
	}
	return err.Error()
}

// friendlyNMError turns nmcli's complaints into something a person can act on.
func friendlyNMError(err error) string {
	s := stderrOf(err)
	s = strings.TrimPrefix(strings.TrimSpace(s), "Error: ")
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "secrets were required"), strings.Contains(low, "(7)"):
		return "wrong password, or the network refused the connection"
	case strings.Contains(low, "no network with ssid"):
		return "network not found"
	case strings.Contains(low, "timeout"), strings.Contains(low, "timed out"), errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return s
}
