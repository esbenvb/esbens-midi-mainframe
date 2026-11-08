package rtpmidi

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type delivery struct {
	p    PeerInfo
	data []byte
	// charge is what data holds in budget (reassembled SysEx); it is returned
	// when the Handler is done with it, or when the delivery is discarded.
	charge int64
	budget *ByteBudget
}

func (d delivery) release() {
	if d.budget != nil {
		d.budget.release(d.charge)
	}
}

// rxHook is called, when set, by the data goroutine after it has decoded a
// packet and before the result is queued. Tests use it to hold that moment open.
var rxHook atomic.Pointer[func()]

type pendingLink struct {
	token, ssrc uint32
	name        string
	ctrl        *net.UDPAddr
	created     time.Time
}

// Server is the responder side of one network MIDI endpoint: one UDP control
// port and the data port next to it. Any number of peers (Macs, iPads, …) can
// connect (up to maxPeers at a time); MIDI from all of them arrives through
// Handler.OnMIDI, and Send broadcasts to all of them.
//
// AppleMIDI has no authentication, so the Server defends itself where it
// cheaply can: half-open invitations are few and expire; a packet that names an
// established session (BY, CK, RS, RTP) is only honoured when it comes from the
// same IP address as that session; SysEx memory is budgeted; and what a stranger
// can make it log is short and rate limited.
type Server struct {
	cfg  Config
	h    Handler
	log  *slog.Logger
	clk  clock
	ssrc uint32

	nameMu sync.RWMutex
	name   string

	ctrl, data *net.UDPConn

	mu      sync.Mutex
	links   map[uint32]*link        // established, by peer SSRC
	pending map[uint32]*pendingLink // half-open, by initiator token

	budget  *ByteBudget
	ignored atomic.Uint64 // packets that named another machine's session, ignored
	// Rate limiters for log lines that anybody on the network can provoke. A
	// session costs a stranger four packets and no credentials, so even "peer
	// connected" and "peer left" are among them; logTimeout has its own so that
	// a flood of those cannot hide a peer that really timed out.
	logFull, logForeign, logBudget, logPeers, logTimeout rateLog

	sendMu sync.Mutex
	enc    *encoder

	deliverCh chan delivery
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	started   bool
}

// NewServer prepares a Server; call Start to bind its sockets.
func NewServer(cfg Config, h Handler) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	ssrc := cfg.SSRC
	if ssrc == 0 {
		ssrc = randUint32()
	}
	budget := cfg.SysexBudget
	if budget == nil {
		budget = SysexBudget
	}
	return &Server{
		cfg: cfg, h: h, log: log.With("component", "rtpmidi", "endpoint", cfg.Name),
		clk: newClock(), ssrc: ssrc, name: cfg.Name, budget: budget,
		links:     map[uint32]*link{},
		pending:   map[uint32]*pendingLink{},
		enc:       newEncoder(),
		deliverCh: make(chan delivery, 1024),
		done:      make(chan struct{}),
	}
}

// listenPair binds a control port and the data port right after it.
func listenPair(bind string, port int) (ctrl, data *net.UDPConn, err error) {
	var ip net.IP
	if bind != "" {
		if ip = net.ParseIP(bind); ip == nil {
			return nil, nil, fmt.Errorf("rtpmidi: bad bind address %q", bind)
		}
	}
	try := func(p int) (*net.UDPConn, *net.UDPConn, error) {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: p})
		if err != nil {
			return nil, nil, err
		}
		cp := c.LocalAddr().(*net.UDPAddr).Port
		d, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: cp + 1})
		if err != nil {
			c.Close()
			return nil, nil, err
		}
		return c, d, nil
	}
	if port != 0 {
		return try(port)
	}
	for i := 0; i < 100; i++ {
		if ctrl, data, err = try(0); err == nil {
			return
		}
	}
	return nil, nil, fmt.Errorf("rtpmidi: no free adjacent UDP port pair: %w", err)
}

// Start binds the sockets and starts serving.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("rtpmidi: server already started")
	}
	s.started = true
	s.mu.Unlock()

	ctrl, data, err := listenPair(s.cfg.BindAddr, s.cfg.ControlPort)
	if err != nil {
		return err
	}
	s.ctrl, s.data = ctrl, data
	for _, c := range []*net.UDPConn{ctrl, data} {
		_ = c.SetReadBuffer(4 << 20) // best effort; SysEx bursts need room
		_ = c.SetWriteBuffer(1 << 20)
	}
	s.wg.Add(4)
	go s.readLoop(ctrl, false)
	go s.readLoop(data, true)
	go s.reapLoop()
	go s.deliverLoop()
	cp, dp := s.Ports()
	s.log.Info("RTP-MIDI endpoint listening", "control", cp, "data", dp, "ssrc", s.ssrc)
	return nil
}

// Ports returns the bound control and data UDP ports (0, 0 before Start).
func (s *Server) Ports() (control, data int) {
	if s.ctrl == nil || s.data == nil {
		return 0, 0
	}
	return s.ctrl.LocalAddr().(*net.UDPAddr).Port, s.data.LocalAddr().(*net.UDPAddr).Port
}

// SetName changes the session name offered to peers that connect from now on.
func (s *Server) SetName(name string) {
	s.nameMu.Lock()
	s.name = name
	s.nameMu.Unlock()
}

func (s *Server) currentName() string {
	s.nameMu.RLock()
	defer s.nameMu.RUnlock()
	return s.name
}

// Peers lists the connected peers, oldest first.
func (s *Server) Peers() []PeerInfo {
	s.mu.Lock()
	out := make([]PeerInfo, 0, len(s.links))
	for _, l := range s.links {
		out = append(out, l.info())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Close says goodbye to all peers, closes the sockets and waits for the
// goroutines to finish.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		links := make([]*link, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, l)
		}
		s.links = map[uint32]*link{}
		s.pending = map[uint32]*pendingLink{}
		s.mu.Unlock()
		if s.ctrl != nil {
			for _, l := range links {
				s.writeTo(s.ctrl, l.ctrl, invitation{cmd: cmdBY, version: appleVersion, token: l.token, ssrc: s.ssrc}.marshal())
			}
			s.ctrl.Close()
		}
		if s.data != nil {
			s.data.Close()
		}
		for _, l := range links {
			s.retire(l, false) // nobody is listening any more: no Note Offs
		}
		s.wg.Wait()
		// Every producer is gone now: whatever is still queued goes back to the budget.
		for {
			select {
			case d := <-s.deliverCh:
				d.release()
			default:
				return
			}
		}
	})
	return nil
}

func (s *Server) writeTo(c *net.UDPConn, to *net.UDPAddr, b []byte) {
	if c == nil || to == nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.WriteToUDP(b, to); err != nil {
		s.log.Debug("udp write failed", "to", to, "err", err)
	}
}

func (s *Server) notifyPeers() {
	if s.h.OnPeers != nil {
		s.h.OnPeers()
	}
}

func (s *Server) readLoop(c *net.UDPConn, isData bool) {
	defer s.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-s.done:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		pkt := buf[:n]
		switch {
		case isAppleMIDI(pkt):
			s.handleApple(c, isData, from, pkt)
		case isData:
			s.handleRTP(from, pkt)
		}
	}
}

func (s *Server) handleApple(c *net.UDPConn, isData bool, from *net.UDPAddr, pkt []byte) {
	p, err := parseApple(pkt)
	if err != nil {
		s.log.Debug("bad AppleMIDI packet", "from", from, "err", err)
		return
	}
	switch v := p.(type) {
	case *invitation:
		switch v.cmd {
		case cmdIN:
			s.onInvite(c, isData, from, v)
		case cmdBY:
			s.onBye(from, v)
		}
	case *clockSync:
		s.onClock(c, from, v)
	case *feedback:
		if l := s.peer(v.ssrc, from, "RS"); l != nil {
			l.touch()
			if l.journal != nil {
				l.journal.ack(v.seq)
			}
		}
	}
}

func (s *Server) okFor(v *invitation) []byte {
	return invitation{cmd: cmdOK, version: appleVersion, token: v.token, ssrc: s.ssrc, name: s.currentName()}.marshal()
}

// peer returns the established session with this SSRC, provided the packet came
// from the machine the session was set up with (the port may differ: control
// and data sockets do). A packet that names someone else's session from
// another address is counted and ignored.
func (s *Server) peer(ssrc uint32, from *net.UDPAddr, what string) *link {
	s.mu.Lock()
	l := s.links[ssrc]
	s.mu.Unlock()
	if l == nil {
		return nil
	}
	if !l.fromHost(from) {
		s.ignoreForeign(what, from)
		return nil
	}
	return l
}

// ignoreForeign counts a packet that used an established peer's SSRC from a
// different address. It is logged at most once per logEvery, with a count.
func (s *Server) ignoreForeign(what string, from *net.UDPAddr) {
	n := s.ignored.Add(1)
	s.logForeign.warn(s.log, "ignoring packets that name another machine's session", "kind", what, "from", from, "total", n)
}

func (s *Server) onInvite(c *net.UDPConn, isData bool, from *net.UDPAddr, v *invitation) {
	reject := func() {
		s.writeTo(c, from, invitation{cmd: cmdNO, version: appleVersion, token: v.token, ssrc: s.ssrc}.marshal())
	}
	if v.version != appleVersion {
		reject()
		return
	}
	s.mu.Lock()
	est := s.links[v.ssrc]
	if est != nil {
		if !est.fromHost(from) {
			// Somebody else's SSRC: it neither replaces the session nor is it answered.
			s.mu.Unlock()
			s.ignoreForeign("IN", from)
			return
		}
		if est.token == v.token {
			// The peer did not see our OK and repeated itself.
			s.mu.Unlock()
			s.writeTo(c, from, s.okFor(v))
			return
		}
	}
	if isData {
		s.inviteData(c, from, v, est, reject)
	} else {
		s.inviteControl(c, from, v, est, reject)
	}
}

// inviteControl handles the first step of the handshake. s.mu is held on entry
// and released before it returns. est is the established session with the same
// SSRC (from the same machine, with another token), if any: the peer restarted
// without saying BY and that session is over.
func (s *Server) inviteControl(c *net.UDPConn, from *net.UDPAddr, v *invitation, est *link, reject func()) {
	// Only established sessions count against maxPeers; half-open invitations
	// have a table of their own.
	if est == nil && len(s.links) >= maxPeers {
		s.mu.Unlock()
		s.logFull.warn(s.log, "rejecting invitation: too many peers", "from", from)
		reject()
		return
	}
	if pl := s.pending[v.token]; pl != nil {
		// A repeat: the OK went missing. It is answered again but does not make
		// the entry any younger, so repeating cannot keep a slot alive.
		if pl.ssrc != v.ssrc || !pl.ctrl.IP.Equal(from.IP) {
			s.mu.Unlock()
			reject()
			return
		}
	} else {
		s.addPendingLocked(&pendingLink{token: v.token, ssrc: v.ssrc, name: v.name, ctrl: from, created: time.Now()})
	}
	if est != nil {
		delete(s.links, v.ssrc)
	}
	s.mu.Unlock()
	s.writeTo(c, from, s.okFor(v))
	if est != nil {
		s.retire(est, true)
		s.notifyPeers()
	}
}

// inviteData handles the second step: the invitation on the data port turns a
// half-open entry into a session. s.mu is held on entry and released before it
// returns.
func (s *Server) inviteData(c *net.UDPConn, from *net.UDPAddr, v *invitation, est *link, reject func()) {
	pl := s.pending[v.token]
	if pl == nil || pl.ssrc != v.ssrc || !pl.ctrl.IP.Equal(from.IP) {
		s.mu.Unlock()
		reject()
		return
	}
	// The table may have filled up between the two steps.
	if est == nil && len(s.links) >= maxPeers {
		delete(s.pending, v.token)
		s.mu.Unlock()
		s.logFull.warn(s.log, "rejecting invitation: too many peers", "from", from)
		reject()
		return
	}
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	delete(s.pending, v.token)
	delete(s.links, v.ssrc)
	l := newLink(pl.name, pl.ssrc, pl.token, pl.ctrl, from, s.cfg.Journal)
	l.releaseOnLoss = s.cfg.ReleaseNotesOnLoss
	l.rx.dec.budget = s.budget
	s.links[l.ssrc] = l
	s.mu.Unlock()
	s.writeTo(c, from, s.okFor(v))
	if est != nil {
		s.retire(est, true)
	}
	s.logPeers.info(s.log, "peer connected", "name", l.name, "addr", from)
	s.notifyPeers()
}

// addPendingLocked records a half-open invitation. It never refuses: when the
// source address already has maxPendingPerIP entries, or the table is full, the
// oldest entries make room. s.mu must be held.
func (s *Server) addPendingLocked(pl *pendingLink) {
	for {
		n := 0
		var oldest *pendingLink
		for _, p := range s.pending {
			if p.ctrl.IP.Equal(pl.ctrl.IP) {
				n++
				if oldest == nil || p.created.Before(oldest.created) {
					oldest = p
				}
			}
		}
		if n < max(maxPendingPerIP, 1) {
			break
		}
		delete(s.pending, oldest.token)
	}
	for len(s.pending) >= max(maxPending, 1) {
		var oldest *pendingLink
		for _, p := range s.pending {
			if oldest == nil || p.created.Before(oldest.created) {
				oldest = p
			}
		}
		delete(s.pending, oldest.token)
	}
	s.pending[pl.token] = pl
}

func (s *Server) onBye(from *net.UDPAddr, v *invitation) {
	s.mu.Lock()
	l := s.links[v.ssrc]
	foreign := false
	if l != nil {
		if l.fromHost(from) {
			delete(s.links, v.ssrc)
		} else {
			l, foreign = nil, true
		}
	}
	if pl := s.pending[v.token]; pl != nil && pl.ssrc == v.ssrc && pl.ctrl.IP.Equal(from.IP) {
		delete(s.pending, v.token)
	}
	s.mu.Unlock()
	if foreign {
		s.ignoreForeign("BY", from)
		return
	}
	if l != nil {
		s.logPeers.info(s.log, "peer left", "name", l.name)
		s.retire(l, true)
		s.notifyPeers()
	}
}

func (s *Server) onClock(c *net.UDPConn, from *net.UDPAddr, v *clockSync) {
	l := s.peer(v.ssrc, from, "CK")
	if l == nil {
		return
	}
	l.touch()
	switch v.count {
	case 0:
		s.writeTo(c, from, clockSync{ssrc: s.ssrc, count: 1, ts: [3]uint64{v.ts[0], s.clk.ticks(), 0}}.marshal())
	case 2:
		l.offset.Store(int64(v.ts[0]/2 + v.ts[2]/2 - v.ts[1]))
		l.synced.Store(true)
	}
}

func (s *Server) handleRTP(from *net.UDPAddr, pkt []byte) {
	h, payload, err := parseRTP(pkt)
	if err != nil {
		return
	}
	l := s.peer(h.ssrc, from, "RTP")
	if l == nil {
		return
	}
	// From decoding to queueing under one lock: retire cannot slip its Note Offs
	// in between and have them overtake the Note Ons this packet carries.
	l.rxMu.Lock()
	defer l.rxMu.Unlock()
	if l.gone {
		return
	}
	l.touch()
	data := l.receive(h, payload, s.log)
	if l.rx.dec.takeStarved() {
		s.logBudget.warn(s.log, "dropping a SysEx message: the SysEx memory budget is used up",
			"peer", l.name, "in_use", s.budget.InUse(), "limit", s.budget.Limit())
	}
	if f := rxHook.Load(); f != nil {
		(*f)()
	}
	if len(data) == 0 {
		return
	}
	d := delivery{p: l.info(), data: data}
	if len(data) > smallDelivery {
		// Reassembled SysEx: it stays on the budget until the Handler is done with
		// it. The reassembly buffer it came from has just been given back, so this
		// is the same memory changing hands, not new demand; it is never refused
		// (that could swallow Note Offs sent along with it).
		d.charge, d.budget = int64(len(data)), s.budget
		s.budget.force(d.charge)
	}
	select {
	case s.deliverCh <- d:
	case <-s.done:
		d.release()
	}
}

// retire ends the receive side of l (BY, timeout, replacement, Close): late
// packets are dropped, its SysEx reassembly goes back to the budget, and with
// notes set the receiver gets Note Offs for what the peer still held, queued
// behind everything the peer delivered before.
func (s *Server) retire(l *link, notes bool) {
	l.retire(notes, func(offs []byte) {
		select {
		case s.deliverCh <- delivery{p: l.info(), data: offs}:
		case <-s.done:
		}
	})
}

func (s *Server) deliverLoop() {
	defer s.wg.Done()
	for {
		select {
		case d := <-s.deliverCh:
			if s.h.OnMIDI != nil {
				s.h.OnMIDI(d.p, d.data)
			}
			d.release()
		case <-s.done:
			return
		}
	}
}

// reapLoop drops silent peers and sends receiver feedback.
func (s *Server) reapLoop() {
	defer s.wg.Done()
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		now := time.Now()
		var gone, fb []*link
		s.mu.Lock()
		for ssrc, l := range s.links {
			if l.idle() > peerTimeout {
				gone = append(gone, l)
				delete(s.links, ssrc)
			} else if l.dirty.Swap(false) {
				fb = append(fb, l)
			}
		}
		for tok, p := range s.pending {
			if now.Sub(p.created) > pendingTimeout {
				delete(s.pending, tok)
			}
		}
		s.mu.Unlock()
		for _, l := range fb {
			s.writeTo(s.ctrl, l.ctrl, feedback{ssrc: s.ssrc, seq: uint16(l.rxSeq.Load())}.marshal())
		}
		for _, l := range gone {
			s.logTimeout.info(s.log, "peer timed out", "name", l.name)
			s.retire(l, true)
			s.writeTo(s.ctrl, l.ctrl, invitation{cmd: cmdBY, version: appleVersion, token: l.token, ssrc: s.ssrc}.marshal())
		}
		if len(gone) > 0 {
			s.notifyPeers()
		}
	}
}

// Send broadcasts raw MIDI to every connected peer. data may be any piece of
// the router's byte stream: whole messages or fragments of a SysEx. The
// encoder keeps SysEx state across calls. Send never blocks on a slow peer.
func (s *Server) Send(data []byte) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	cmds := s.enc.encode(data) // always, so SysEx state stays right with nobody connected
	if len(cmds) == 0 || s.data == nil {
		return
	}
	s.mu.Lock()
	links := make([]*link, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()
	if len(links) == 0 {
		return
	}
	lists := packLists(cmds)
	ts := uint32(s.clk.ticks())
	_ = s.data.SetWriteDeadline(time.Now().Add(writeTimeout))
	for _, l := range links {
		for _, pkt := range l.packetsFor(lists, s.ssrc, ts) {
			if _, err := s.data.WriteToUDP(pkt, l.data); err != nil {
				s.log.Debug("udp write failed", "to", l.data, "err", err)
				break
			}
		}
	}
}
