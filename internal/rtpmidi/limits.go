package rtpmidi

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// DefaultSysexBudget is the default size of SysexBudget: the most memory all
// endpoints and sessions of the process together spend on SysEx that is being
// reassembled from segments, or waits to be handed to the router.
const DefaultSysexBudget = 64 << 20

// SysexBudget is the process-wide allowance used by every Server and Client
// unless Config.SysexBudget says otherwise. AppleMIDI has no authentication, so
// without it every peer of every endpoint could pin a whole message's worth of
// memory (maxSysexReassembly) at the same time. A message that cannot get
// budget is dropped, never blocked: the stream carries on with the next one.
var SysexBudget = NewByteBudget(DefaultSysexBudget)

// ByteBudget is a shared allowance of bytes. Callers reserve before they
// allocate and release when they let go; the budget never blocks, a refused
// reservation is the caller's cue to drop what it was about to buffer.
type ByteBudget struct {
	limit atomic.Int64
	used  atomic.Int64
}

// NewByteBudget returns a budget of limit bytes.
func NewByteBudget(limit int64) *ByteBudget {
	b := &ByteBudget{}
	b.limit.Store(limit)
	return b
}

// SetLimit changes the size of the budget. Memory already handed out stays
// handed out; new reservations are measured against the new limit.
func (b *ByteBudget) SetLimit(limit int64) { b.limit.Store(limit) }

// Limit returns the size of the budget.
func (b *ByteBudget) Limit() int64 { return b.limit.Load() }

// InUse returns the bytes currently reserved.
func (b *ByteBudget) InUse() int64 { return b.used.Load() }

func (b *ByteBudget) reserve(n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		u := b.used.Load()
		if u+n > b.limit.Load() {
			return false
		}
		if b.used.CompareAndSwap(u, u+n) {
			return true
		}
	}
}

// force charges n bytes whatever the limit says. It is for memory that has
// already been allocated and only changes hands (see Server.handleRTP).
func (b *ByteBudget) force(n int64) {
	if n > 0 {
		b.used.Add(n)
	}
}

func (b *ByteBudget) release(n int64) {
	if n > 0 {
		b.used.Add(-n)
	}
}

// logBurst is how many lines of one rateLog the info method lets through in a
// row before it falls back to what warn always does. It equals the default
// maxPeers, so a whole set of legitimate peers joining at the same moment is
// still logged in full, while a stranger who loops the handshake costs at most
// logBurst lines per logEvery.
const logBurst = 16

// rateLog lets a few log lines through per interval (one for warn, logBurst for
// info) and counts what it swallowed in between, so a flood of identical events
// costs a handful of lines.
type rateLog struct {
	mu      sync.Mutex
	until   time.Time // end of the current window
	used    int       // lines let through in the current window
	skipped int
}

// allow reports whether to log now and how many calls were swallowed since
// the last line that went out. One line per interval.
func (r *rateLog) allow(every time.Duration) (ok bool, skipped int) {
	return r.allowBurst(every, 1)
}

// allowBurst is allow with room for burst lines per interval.
func (r *rateLog) allowBurst(every time.Duration, burst int) (ok bool, skipped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !now.Before(r.until) {
		r.until = now.Add(every)
		r.used = 0
	}
	if r.used >= max(burst, 1) {
		r.skipped++
		return false, 0
	}
	r.used++
	skipped, r.skipped = r.skipped, 0
	return true, skipped
}

// warn logs at Warn, rate limited, with the number of suppressed lines.
func (r *rateLog) warn(log *slog.Logger, msg string, args ...any) {
	if ok, n := r.allowBurst(logEvery, 1); ok {
		if n > 0 {
			args = append(args, "suppressed", n)
		}
		log.Warn(msg, args...)
	}
}

// info logs at Info for events that are ordinary one at a time (a peer joins or
// leaves) and only a flood when somebody provokes them in a loop: up to
// logBurst lines per logEvery, then the rest is swallowed and counted, and the
// next line that goes out says how many.
func (r *rateLog) info(log *slog.Logger, msg string, args ...any) {
	if ok, n := r.allowBurst(logEvery, logBurst); ok {
		if n > 0 {
			args = append(args, "suppressed", n)
		}
		log.Info(msg, args...)
	}
}

// Peer names come straight off the network: they end up in log lines and in
// the web UI. Keep them short and printable.
const (
	maxPeerNameRunes = 64
	maxPeerNameScan  = 1024 // bytes looked at; the rest of a long name is never read
)

// cleanPeerName turns the name field of an AppleMIDI packet into something
// safe to log and show: valid UTF-8, no control characters, at most 64 runes.
func cleanPeerName(b []byte) string {
	if len(b) > maxPeerNameScan {
		b = b[:maxPeerNameScan]
	}
	var sb strings.Builder
	n := 0
	for len(b) > 0 && n < maxPeerNameRunes {
		r, size := utf8.DecodeRune(b)
		b = b[size:] // an invalid byte decodes as U+FFFD, width 1
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			continue
		}
		sb.WriteRune(r)
		n++
	}
	return sb.String()
}
