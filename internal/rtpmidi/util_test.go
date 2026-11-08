package rtpmidi

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testBind is the address tests bind their endpoints to. Not the default "all
// interfaces": on macOS a fresh dual-stack wildcard UDP socket (what the
// default is) occasionally never receives a single IPv4 datagram for as long as
// it lives, roughly once in a few thousand sockets, and no retry helps. A plain
// Go program without any of this package reproduces it; a socket bound to
// 127.0.0.1 never showed it. A test that dials such an endpoint fails after its
// timeout with "context deadline exceeded" or "no answer to invitation", the
// server having seen no packet at all. Binding loopback also keeps test runs
// from opening ports on the network. TestDefaultBindAcceptsIPv4 covers the
// default itself.
const testBind = "127.0.0.1"

// collector gathers the MIDI delivered through Handler.OnMIDI.
type collector struct {
	mu    sync.Mutex
	buf   []byte
	peers []PeerInfo
}

func (c *collector) onMIDI(p PeerInfo, d []byte) {
	c.mu.Lock()
	c.buf = append(c.buf, d...)
	c.peers = append(c.peers, p)
	c.mu.Unlock()
}

func (c *collector) got() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// setTimers shortens the protocol timers for the duration of a test.
func setTimers(t *testing.T, peer, reap, ck time.Duration) {
	t.Helper()
	op, or, oc := peerTimeout, reapInterval, ckInterval
	peerTimeout, reapInterval, ckInterval = peer, reap, ck
	t.Cleanup(func() { peerTimeout, reapInterval, ckInterval = op, or, oc })
}

// processAlive reports whether a process still exists (a killed child that
// has been reaped is gone).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
