package rtpmidi

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- helpers ---------------------------------------------------------------

// logCapture is a slog.Handler that keeps what was logged.
type logCapture struct {
	mu    sync.Mutex
	lines []string
	msgs  []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	c.mu.Lock()
	c.lines = append(c.lines, sb.String())
	c.msgs = append(c.msgs, r.Message)
	c.mu.Unlock()
	return nil
}

// count returns how many records had exactly this message.
func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

func (c *logCapture) longest() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if len(l) > n {
			n = len(l)
		}
	}
	return n
}

func (c *logCapture) logger() *slog.Logger { return slog.New(c) }

// newTestServer starts a server like startServerCfg but keeps cfg.Logger if set.
func newTestServer(t *testing.T, cfg Config, hd Handler) *Server {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	if cfg.Name == "" {
		cfg.Name = "srv"
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = testBind
	}
	s := NewServer(cfg, hd)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// rawPeer speaks AppleMIDI to a Server byte by byte from two plain sockets.
type rawPeer struct {
	t          *testing.T
	ctrl, data *net.UDPConn
	cp, dp     int
	ssrc       uint32
	token      uint32
	seq        uint16
}

var loopback = net.IPv4(127, 0, 0, 1)

func newRawPeer(t *testing.T, s *Server) *rawPeer {
	t.Helper()
	ctrl, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	data, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctrl.Close(); data.Close() })
	p := &rawPeer{t: t, ctrl: ctrl, data: data}
	p.cp, p.dp = s.Ports()
	return p
}

func (p *rawPeer) send(c *net.UDPConn, port int, pkt []byte) {
	p.t.Helper()
	if _, err := c.WriteToUDP(pkt, &net.UDPAddr{IP: loopback, Port: port}); err != nil {
		p.t.Fatal(err)
	}
}

// recvInv waits for the next packet on c and parses it as an invitation answer.
func (p *rawPeer) recvInv(c *net.UDPConn) *invitation {
	p.t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		p.t.Fatalf("no answer: %v", err)
	}
	a, _ := parseApple(buf[:n])
	inv, _ := a.(*invitation)
	if inv == nil {
		p.t.Fatalf("not an invitation answer: %x", buf[:n])
	}
	return inv
}

func (p *rawPeer) in(token, ssrc uint32, name string) []byte {
	return invitation{cmd: cmdIN, version: appleVersion, token: token, ssrc: ssrc, name: name}.marshal()
}

// ctrlIN sends a control-port invitation and returns the answer's command.
func (p *rawPeer) ctrlIN(token, ssrc uint32, name string) cmdKind {
	p.send(p.ctrl, p.cp, p.in(token, ssrc, name))
	return p.recvInv(p.ctrl).cmd
}

func (p *rawPeer) dataIN(token, ssrc uint32, name string) cmdKind {
	p.send(p.data, p.dp, p.in(token, ssrc, name))
	return p.recvInv(p.data).cmd
}

// connect runs the whole two-step handshake and fails unless both steps are OK.
func (p *rawPeer) connect(token, ssrc uint32, name string) {
	p.t.Helper()
	p.token, p.ssrc = token, ssrc
	if got := p.ctrlIN(token, ssrc, name); got != cmdOK {
		p.t.Fatalf("control IN answered %v", got)
	}
	if got := p.dataIN(token, ssrc, name); got != cmdOK {
		p.t.Fatalf("data IN answered %v", got)
	}
}

func (p *rawPeer) rtpPacket(seq uint16, list []byte) []byte {
	pkt := rtpHeader{marker: true, pt: payloadType, seq: seq, ts: uint32(seq) * 10, ssrc: p.ssrc}.appendTo(nil)
	return append(pkt, buildSection(list, nil)...)
}

// rtp sends one RTP-MIDI packet with the next sequence number.
func (p *rawPeer) rtp(list []byte) {
	p.t.Helper()
	p.seq++
	p.send(p.data, p.dp, p.rtpPacket(p.seq, list))
}

func (p *rawPeer) bye() {
	p.t.Helper()
	p.send(p.ctrl, p.cp, invitation{cmd: cmdBY, version: appleVersion, token: p.token, ssrc: p.ssrc}.marshal())
}

func (s *Server) linkBySSRC(ssrc uint32) *link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links[ssrc]
}

func (s *Server) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// ---- decision 2: half-open invitations -------------------------------------

func TestHalfOpenInvitationsDoNotCountAgainstMaxPeers(t *testing.T) {
	setVar(t, &maxPeers, 2)
	s := newTestServer(t, Config{}, Handler{})
	p := newRawPeer(t, s)
	// Ten invitations that are never completed: every one is answered OK ...
	for i := 0; i < 10; i++ {
		if got := p.ctrlIN(uint32(1000+i), uint32(2000+i), ""); got != cmdOK {
			t.Fatalf("half-open IN #%d answered %v", i, got)
		}
	}
	// ... they are few, ...
	if n := s.pendingCount(); n > maxPendingPerIP {
		t.Fatalf("%d half-open entries from one address, want at most %d", n, maxPendingPerIP)
	}
	// ... and a real peer still gets in, although maxPeers is 2.
	dial(t, s, "real one", Handler{})
	dial(t, s, "real two", Handler{})
	waitFor(t, "both real peers", func() bool { return len(s.Peers()) == 2 })
}

func TestPendingTableBounds(t *testing.T) {
	s := NewServer(Config{Logger: quietLogger()}, Handler{})
	base := time.Now()
	add := func(tok uint32, ip string, age time.Duration) {
		s.mu.Lock()
		s.addPendingLocked(&pendingLink{token: tok, ssrc: tok, ctrl: &net.UDPAddr{IP: net.ParseIP(ip), Port: 1}, created: base.Add(age)})
		s.mu.Unlock()
	}
	has := func(tok uint32) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.pending[tok]
		return ok
	}
	// One address may hold maxPendingPerIP entries; newer ones push out the oldest.
	for i := 0; i < maxPendingPerIP+3; i++ {
		add(uint32(i), "10.0.0.1", time.Duration(i)*time.Second)
	}
	if s.pendingCount() != maxPendingPerIP {
		t.Fatalf("%d entries from one address", s.pendingCount())
	}
	if has(0) || has(1) || has(2) || !has(uint32(maxPendingPerIP+2)) {
		t.Fatal("the oldest entries must be the ones evicted")
	}
	// Many addresses: the table as a whole is capped, again oldest out.
	for i := 0; i < 3*maxPending; i++ {
		add(uint32(1000+i), fmt.Sprintf("10.1.%d.1", i), time.Duration(100+i)*time.Second)
	}
	if s.pendingCount() != maxPending {
		t.Fatalf("%d entries in total, want %d", s.pendingCount(), maxPending)
	}
	if has(1000) || !has(uint32(1000+3*maxPending-1)) {
		t.Fatal("oldest entry not evicted / newest missing")
	}
}

func TestRepeatedInvitationDoesNotRefreshPendingEntry(t *testing.T) {
	s := newTestServer(t, Config{}, Handler{})
	p := newRawPeer(t, s)
	created := func() time.Time {
		s.mu.Lock()
		defer s.mu.Unlock()
		if pl := s.pending[7]; pl != nil {
			return pl.created
		}
		return time.Time{}
	}
	if p.ctrlIN(7, 8, "") != cmdOK {
		t.Fatal("first IN")
	}
	first := created()
	time.Sleep(30 * time.Millisecond)
	if p.ctrlIN(7, 8, "") != cmdOK { // the OK is repeated ...
		t.Fatal("repeated IN")
	}
	if got := created(); !got.Equal(first) { // ... but the entry does not get any younger
		t.Fatalf("a repeated IN refreshed the entry (%v -> %v)", first, got)
	}
	// A repeat of the token from somebody else's SSRC is not a repeat.
	if got := p.ctrlIN(7, 9, ""); got != cmdNO {
		t.Fatalf("same token, other ssrc: %v", got)
	}
}

func TestHalfOpenEntriesExpire(t *testing.T) {
	setVar(t, &pendingTimeout, 100*time.Millisecond)
	setVar(t, &reapInterval, 20*time.Millisecond)
	s := newTestServer(t, Config{}, Handler{})
	p := newRawPeer(t, s)
	if p.ctrlIN(1, 2, "") != cmdOK {
		t.Fatal("IN")
	}
	waitFor(t, "half-open entry to expire", func() bool { return s.pendingCount() == 0 })
}

func TestPromotionRechecksEstablishedCap(t *testing.T) {
	setVar(t, &maxPeers, 1)
	s := newTestServer(t, Config{}, Handler{})
	p1, p2 := newRawPeer(t, s), newRawPeer(t, s)
	// Both pass the control step (nobody is established yet) ...
	if p1.ctrlIN(1, 11, "one") != cmdOK || p2.ctrlIN(2, 22, "two") != cmdOK {
		t.Fatal("control INs")
	}
	// ... the first completes, so the second no longer fits at the data port.
	if got := p1.dataIN(1, 11, "one"); got != cmdOK {
		t.Fatalf("first data IN: %v", got)
	}
	if got := p2.dataIN(2, 22, "two"); got != cmdNO {
		t.Fatalf("second data IN: %v, want NO", got)
	}
	if n := len(s.Peers()); n != 1 {
		t.Fatalf("%d peers", n)
	}
	if s.pendingCount() != 0 {
		t.Fatal("the refused entry is still pending")
	}
}

// ---- decision 4: session identity ------------------------------------------

func TestSessionPacketsFromAnotherAddressAreIgnored(t *testing.T) {
	var rx collector
	s := newTestServer(t, Config{ReleaseNotesOnLoss: true}, Handler{OnMIDI: rx.onMIDI})
	p := newRawPeer(t, s)
	p.connect(0x2222, 0x1111, "victim")
	l := s.linkBySSRC(0x1111)
	if l == nil {
		t.Fatal("no link")
	}
	// The attacker is on another machine. (Only handlers are called with the
	// foreign address; replies, if any, go to loopback and are harmless.)
	foreign := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 40000}
	ignored := func() uint64 { return s.ignored.Load() }

	// BY with the victim's SSRC.
	s.handleApple(s.ctrl, false, foreign, invitation{cmd: cmdBY, version: appleVersion, token: 0, ssrc: 0x1111}.marshal())
	s.handleApple(s.data, true, foreign, invitation{cmd: cmdBY, version: appleVersion, token: 0x2222, ssrc: 0x1111}.marshal())
	if len(s.Peers()) != 1 {
		t.Fatal("a BY from another address ended the session")
	}
	// A new invitation that names the victim's SSRC neither evicts the session
	// nor leaves a half-open entry behind.
	before := s.pendingCount()
	s.handleApple(s.ctrl, false, foreign, p.in(0x3333, 0x1111, "evil"))
	if len(s.Peers()) != 1 || s.pendingCount() != before {
		t.Fatalf("foreign IN: %d peers, %d pending", len(s.Peers()), s.pendingCount())
	}
	// Clock sync and feedback do not count as signs of life.
	seen := l.lastSeen()
	ck := clockSync{ssrc: 0x1111, count: 0, ts: [3]uint64{1}}.marshal()
	s.handleApple(s.data, true, foreign, ck)
	s.handleApple(s.ctrl, false, foreign, feedback{ssrc: 0x1111, seq: 1}.marshal())
	if !l.lastSeen().Equal(seen) {
		t.Fatal("CK/RS from another address kept the session alive")
	}
	// RTP is not delivered, and does not disturb the peer's sequence tracking.
	s.handleRTP(foreign, p.rtpPacket(500, h(t, "903C64")))
	time.Sleep(30 * time.Millisecond)
	if len(rx.got()) != 0 {
		t.Fatalf("MIDI from another address delivered: %x", rx.got())
	}
	if got := ignored(); got != 6 {
		t.Fatalf("ignored = %d, want 6", got)
	}

	// The data-port step of a handshake must come from the machine that did the
	// control step.
	p2 := newRawPeer(t, s)
	if p2.ctrlIN(0x4444, 0x5555, "half") != cmdOK {
		t.Fatal("control IN")
	}
	s.handleApple(s.data, true, foreign, p2.in(0x4444, 0x5555, "half"))
	if s.linkBySSRC(0x5555) != nil {
		t.Fatal("a session was established from two different addresses")
	}

	// Everything works from the right address.
	p.rtp(h(t, "903C64"))
	waitFor(t, "MIDI from the peer", func() bool { return bytes.Equal(rx.got(), h(t, "903C64")) })
	p.bye()
	waitFor(t, "BY from the right address", func() bool { return len(s.Peers()) == 0 })
	waitFor(t, "note off for the held note", func() bool { return bytes.Equal(rx.got(), h(t, "903C64 803C00")) })
}

func TestReinviteFromSameAddressStillReplaces(t *testing.T) {
	s := newTestServer(t, Config{}, Handler{})
	p := newRawPeer(t, s)
	p.connect(1, 77, "first")
	if got := p.ctrlIN(2, 77, "second"); got != cmdOK { // restart with a new token
		t.Fatalf("re-invite: %v", got)
	}
	if len(s.Peers()) != 0 {
		t.Fatal("old session should be gone")
	}
	if got := p.dataIN(2, 77, "second"); got != cmdOK {
		t.Fatalf("data: %v", got)
	}
	if ps := s.Peers(); len(ps) != 1 || ps[0].Name != "second" {
		t.Fatalf("peers %+v", ps)
	}
}

// ---- decision 5: names and log flooding ------------------------------------

func TestCleanPeerName(t *testing.T) {
	long := strings.Repeat("x", 60000)
	if got := cleanPeerName([]byte(long)); got != strings.Repeat("x", maxPeerNameRunes) {
		t.Fatalf("long name: %d bytes", len(got))
	}
	if got := cleanPeerName([]byte("Mac\x00Book\x1b[31m\r\n\x7f\u0085 Pro")); got != "MacBook[31m Pro" {
		t.Fatalf("control characters: %q", got)
	}
	// Runes, not bytes: 64 two-byte runes survive whole, a 65th does not.
	two := strings.Repeat("ø", 70)
	if got := cleanPeerName([]byte(two)); got != strings.Repeat("ø", 64) {
		t.Fatalf("multi-byte: %q", got)
	}
	if got := cleanPeerName([]byte("a\xffb\xc3")); !utf8.ValidString(got) || got != "a�b�" {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if got := cleanPeerName([]byte("  ok")); got != "ok" {
		t.Fatalf("line separators: %q", got)
	}
	if cleanPeerName(nil) != "" {
		t.Fatal("empty name")
	}
	// Controls do not count towards the 64, and the scan is bounded.
	if got := cleanPeerName([]byte(strings.Repeat("\x01", 3000) + "late")); got != "" {
		t.Fatalf("scan bound: %q", got)
	}
	// What parseApple hands on is the cleaned name.
	pkt := invitation{cmd: cmdIN, version: 2, token: 1, ssrc: 2, name: long}.marshal()
	p, err := parseApple(pkt)
	if err != nil || len(p.(*invitation).name) != maxPeerNameRunes {
		t.Fatalf("parseApple name: %v %v", err, p)
	}
}

func TestHugePeerNameCannotFloodTheLog(t *testing.T) {
	var lc logCapture
	s := newTestServer(t, Config{Logger: lc.logger()}, Handler{})
	p := newRawPeer(t, s)
	name := strings.Repeat("A", 8000) // under macOS's 9216-byte datagram limit
	p.connect(1, 2, name)
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	if got := s.Peers()[0].Name; utf8.RuneCountInString(got) > maxPeerNameRunes {
		t.Fatalf("peer name of %d runes", utf8.RuneCountInString(got))
	}
	p.bye()
	waitFor(t, "peer gone", func() bool { return len(s.Peers()) == 0 })
	if n := lc.longest(); n > 1000 {
		t.Fatalf("a log line of %d bytes", n)
	}
}

func TestRejectionLogIsRateLimited(t *testing.T) {
	setVar(t, &maxPeers, 1)
	setVar(t, &logEvery, 500*time.Millisecond)
	var lc logCapture
	s := newTestServer(t, Config{Logger: lc.logger()}, Handler{})
	good := newRawPeer(t, s)
	good.connect(1, 1, "good")

	flood := newRawPeer(t, s)
	const n = 20
	for i := 0; i < n; i++ {
		if got := flood.ctrlIN(uint32(100+i), uint32(200+i), ""); got != cmdNO {
			t.Fatalf("IN #%d answered %v, want NO", i, got)
		}
	}
	if got := lc.count("rejecting invitation: too many peers"); got != 1 {
		t.Fatalf("%d rejection lines for %d rejected invitations, want 1", got, n)
	}
	time.Sleep(550 * time.Millisecond)
	if got := flood.ctrlIN(999, 998, ""); got != cmdNO {
		t.Fatalf("answered %v", got)
	}
	if got := lc.count("rejecting invitation: too many peers"); got != 2 {
		t.Fatalf("after the interval: %d lines, want 2", got)
	}
	lc.mu.Lock()
	last := lc.lines[len(lc.lines)-1]
	lc.mu.Unlock()
	if !strings.Contains(last, fmt.Sprintf("suppressed=%d", n-1)) {
		t.Fatalf("second line does not report the suppressed ones: %q", last)
	}
}

// endWindow ends the current rate limiting window of r, as the passing of
// logEvery would, without the test having to wait for it.
func (r *rateLog) endWindow() {
	r.mu.Lock()
	r.until = time.Time{}
	r.mu.Unlock()
}

// text returns everything logged, one record per line.
func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// lastLine returns the line logged last.
func (c *logCapture) lastLine() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == 0 {
		return ""
	}
	return c.lines[len(c.lines)-1]
}

func TestRateLogInfoBurstThenSuppressed(t *testing.T) {
	setVar(t, &logEvery, time.Hour) // the window ends when the test says so
	var lc logCapture
	var r rateLog
	const calls = 3 * logBurst
	for i := 0; i < calls; i++ {
		r.info(lc.logger(), "event", "i", i)
	}
	if got := lc.count("event"); got != logBurst {
		t.Fatalf("%d lines for %d events, want a burst of %d", got, calls, logBurst)
	}
	if strings.Contains(lc.lastLine(), "suppressed") {
		t.Fatalf("a line inside the burst claims suppression: %q", lc.lastLine())
	}
	r.endWindow()
	r.info(lc.logger(), "event", "i", calls)
	if got := lc.count("event"); got != logBurst+1 {
		t.Fatalf("%d lines after the window, want %d", got, logBurst+1)
	}
	if want := fmt.Sprintf("suppressed=%d", calls-logBurst); !strings.Contains(lc.lastLine(), want) {
		t.Fatalf("first line of the next window lacks %q: %q", want, lc.lastLine())
	}
	// warn keeps its stricter meaning: one line per window.
	var w rateLog
	for i := 0; i < 5; i++ {
		w.warn(lc.logger(), "warning")
	}
	if got := lc.count("warning"); got != 1 {
		t.Fatalf("warn let %d lines through, want 1", got)
	}
}

// A completed handshake followed by BY costs a stranger four packets and no
// credentials, and the link is gone again afterwards, so maxPeers does not stop
// the loop. What it makes the server log must still be short.
func TestSessionLoopCannotFloodTheLog(t *testing.T) {
	setVar(t, &logEvery, time.Hour)
	var lc logCapture
	var changes atomic.Int32
	s := newTestServer(t, Config{Logger: lc.logger()}, Handler{OnPeers: func() { changes.Add(1) }})
	p := newRawPeer(t, s)
	const cycles = 200
	for i := 0; i < cycles; i++ {
		p.connect(uint32(1000+i), 77, "stranger") // fresh token, one SSRC
		p.bye()
	}
	// OnPeers follows the log line of each join and leave, so once it has been
	// called for all of them, everything that was going to be logged is logged.
	waitFor(t, "last BY handled", func() bool { return changes.Load() >= 2*cycles })
	joined, left := lc.count("peer connected"), lc.count("peer left")
	if joined+left > logBurst {
		t.Fatalf("%d 'peer connected' and %d 'peer left' lines for %d sessions, want at most %d together", joined, left, cycles, logBurst)
	}
	if joined == 0 {
		t.Fatal("not even the first session was logged")
	}
	if n := lc.longest(); n > 300 {
		t.Fatalf("a log line of %d bytes", n)
	}

	// The next window opens with a line that says what was swallowed.
	s.logPeers.endWindow()
	p.connect(5000, 78, "honest")
	waitFor(t, "honest peer logged", func() bool { return changes.Load() >= 2*cycles+1 })
	if want := fmt.Sprintf("suppressed=%d", 2*cycles-logBurst); !strings.Contains(lc.lastLine(), want) || !strings.Contains(lc.lastLine(), "peer connected") {
		t.Fatalf("line after the flood lacks %q: %q", want, lc.lastLine())
	}
}

// Real use stays fully logged: a few peers coming and going are not a flood.
func TestOrdinarySessionsAreAllLogged(t *testing.T) {
	setVar(t, &logEvery, time.Hour)
	var lc logCapture
	s := newTestServer(t, Config{Logger: lc.logger()}, Handler{})
	p := newRawPeer(t, s)
	const sessions = 5
	for i := 0; i < sessions; i++ {
		p.connect(uint32(10+i), uint32(20+i), "mac")
		waitFor(t, "peer connected line", func() bool { return lc.count("peer connected") == i+1 })
		p.bye()
		waitFor(t, "peer left line", func() bool { return lc.count("peer left") == i+1 })
	}
	if strings.Contains(lc.text(), "suppressed") {
		t.Fatal("ordinary use reported suppressed lines")
	}
}

// "peer timed out" is limited like the other two, and by a limiter of its own:
// a flood of joins that has used up the other one must not hide it.
func TestPeerTimeoutLogIsRateLimited(t *testing.T) {
	setVar(t, &logEvery, time.Hour)
	setVar(t, &maxPeers, 64)
	setVar(t, &reapInterval, 10*time.Millisecond)
	var lc logCapture
	s := newTestServer(t, Config{Logger: lc.logger()}, Handler{})
	p := newRawPeer(t, s)
	const peers = 3 * logBurst
	for i := 0; i < peers; i++ {
		p.connect(uint32(100+i), uint32(200+i), "silent") // and then says nothing
	}
	// Let them all fall silent at once (a real timeout would also send BY to
	// the control socket, in the middle of the handshakes above).
	s.mu.Lock()
	for _, l := range s.links {
		l.seenMu.Lock()
		l.seen = time.Now().Add(-time.Hour)
		l.seenMu.Unlock()
	}
	s.mu.Unlock()
	waitFor(t, "all of them timed out", func() bool { return len(s.Peers()) == 0 })
	s.Close() // returns when the reaper has finished logging
	if got := lc.count("peer connected"); got != logBurst {
		t.Fatalf("%d 'peer connected' lines for %d peers, want %d", got, peers, logBurst)
	}
	got := lc.count("peer timed out")
	if got == 0 {
		t.Fatal("no 'peer timed out' line: the join flood hid it")
	}
	if got > logBurst {
		t.Fatalf("%d 'peer timed out' lines for %d silent peers, want at most %d", got, peers, logBurst)
	}
}

// ---- decision 8: release must not overtake delivery ------------------------

func TestByeDoesNotOvertakeInFlightNoteOn(t *testing.T) {
	var rx collector
	s := newTestServer(t, Config{ReleaseNotesOnLoss: true}, Handler{OnMIDI: rx.onMIDI})
	p := newRawPeer(t, s)
	p.connect(1, 5, "peer")

	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open) // runs before the server is closed, which would wait for a stuck hook
	reached := make(chan struct{}, 1)
	hook := func() {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-gate
	}
	rxHook.Store(&hook)
	t.Cleanup(func() { rxHook.Store(nil) })

	p.rtp(h(t, "903C64"))
	select {
	case <-reached: // decoded and recorded as held, not yet queued for delivery
	case <-time.After(3 * time.Second):
		t.Fatal("packet never reached the hook")
	}
	p.bye() // the control goroutine retires the link and releases the held note
	time.Sleep(100 * time.Millisecond)
	open()
	waitFor(t, "note on and note off", func() bool { return len(rx.got()) >= 6 })
	time.Sleep(20 * time.Millisecond)
	if got := rx.got(); !bytes.Equal(got, h(t, "903C64 803C00")) {
		t.Fatalf("delivered %x: the Note Off overtook the Note On (stuck note)", got)
	}
}

func TestPacketAfterRetireIsDropped(t *testing.T) {
	var rx collector
	s := newTestServer(t, Config{ReleaseNotesOnLoss: true}, Handler{OnMIDI: rx.onMIDI})
	p := newRawPeer(t, s)
	p.connect(1, 5, "peer")
	l := s.linkBySSRC(5)
	s.mu.Lock()
	delete(s.links, 5)
	s.mu.Unlock()
	s.retire(l, true)
	// A packet that looked the link up before it was removed gets here late.
	s.mu.Lock()
	s.links[5] = l
	s.mu.Unlock()
	p.rtp(h(t, "903C64"))
	time.Sleep(50 * time.Millisecond)
	if len(rx.got()) != 0 {
		t.Fatalf("a retired link delivered %x", rx.got())
	}
}

// ---- decision 9: stale announcement files ----------------------------------

func TestAvahiFilesSweepsStaleFiles(t *testing.T) {
	dir := t.TempDir()
	stale := []string{"emm-rtp-old.service", "emm-rtp-rtp_usb_1-0123abcd.service", "emm-rtp-half.service.tmp"}
	keep := []string{"emm-http.service", "other.service", "emm-rtp-readme.txt"}
	for _, n := range append(append([]string(nil), stale...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "emm-rtp-dir.service"), 0o755); err != nil {
		t.Fatal(err)
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }

	a := NewAvahiFiles(dir)
	if err := a.Announce("rtp:one", "One", 5004); err != nil {
		t.Fatal(err)
	}
	for _, n := range stale {
		if exists(n) {
			t.Errorf("stale file %s survived", n)
		}
	}
	for _, n := range append(keep, "emm-rtp-dir.service") {
		if !exists(n) {
			t.Errorf("%s must not be touched", n)
		}
	}
	// The sweep happens once: what this instance announced stays.
	if err := a.Announce("rtp:two", "Two", 5006); err != nil {
		t.Fatal(err)
	}
	if !exists(avahiFileName("rtp:one")) || !exists(avahiFileName("rtp:two")) {
		t.Fatal("a later Announce removed this instance's own files")
	}
	a.Close()
	if exists(avahiFileName("rtp:one")) || exists(avahiFileName("rtp:two")) {
		t.Fatal("Close left files behind")
	}
}

// ---- decision 3 and 10: timers ---------------------------------------------

func TestPeerLivenessUsesMonotonicClock(t *testing.T) {
	l := newLink("x", 1, 1, nil, nil, false)
	l.touch()
	if s := l.lastSeen().String(); !strings.Contains(s, "m=") {
		t.Fatalf("last-seen stamp %q has no monotonic reading: idle() would follow the wall clock, and an NTP step would drop (or immortalise) sessions", s)
	}
	l.seenMu.Lock()
	l.seen = time.Now().Add(-5 * time.Second)
	l.seenMu.Unlock()
	if d := l.idle(); d < 5*time.Second || d > 6*time.Second {
		t.Fatalf("idle = %v", d)
	}
}

// TestNoWallClockArithmeticForTimeouts guards the other half of the fix: no
// code in the package may turn a time into Unix nanoseconds (or back) to measure
// an interval. The one use that is left seeds a random number.
func TestNoWallClockArithmeticForTimeouts(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "time.Unix(") || (strings.Contains(line, "UnixNano()") && !strings.Contains(line, "uint32(time.Now().UnixNano())")) {
				t.Errorf("%s:%d uses the wall clock: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestPeerTimeoutHasMarginOverAppleCKLimit(t *testing.T) {
	// Apple initiators send a clock sync at least every 60 s.
	if peerTimeout < 75*time.Second || peerTimeout != 90*time.Second {
		t.Fatalf("peerTimeout = %v, want 90 s", peerTimeout)
	}
}
