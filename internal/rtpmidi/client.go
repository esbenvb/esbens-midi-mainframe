package rtpmidi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Client is the initiator side: it opens a session to a remote RTP-MIDI
// endpoint (an EMM Server, a Mac's network session, rtpMIDI, rtpmidid, …).
type Client struct {
	cfg  Config
	h    Handler
	log  *slog.Logger
	clk  clock
	ssrc uint32

	ctrl, data   *net.UDPConn
	rctrl, rdata *net.UDPAddr
	l            *link
	budget       *ByteBudget
	logBudget    rateLog

	sendMu sync.Mutex
	enc    *encoder

	deliverCh chan delivery
	done      chan struct{}
	wg        sync.WaitGroup
	once      sync.Once

	errMu sync.Mutex
	err   error
}

var (
	// ErrRejected is returned by Dial when the peer answers the invitation with NO.
	ErrRejected = errors.New("rtpmidi: invitation rejected")
	// ErrTimeout is returned by Dial when the peer does not answer.
	ErrTimeout = errors.New("rtpmidi: no answer to invitation")
	// ErrPeerLeft is the Err of a Client whose peer ended the session.
	ErrPeerLeft = errors.New("rtpmidi: peer ended the session")
	// ErrPeerTimeout is the Err of a Client whose peer went silent.
	ErrPeerTimeout = errors.New("rtpmidi: peer timed out")
)

// Dial invites the endpoint at host:controlPort (data port = controlPort+1)
// and returns once the two-port handshake is complete. Clock synchronisation
// continues in the background. ctx only bounds the handshake.
func Dial(ctx context.Context, cfg Config, host string, controlPort int, h Handler) (*Client, error) {
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(controlPort)))
	if err != nil {
		return nil, err
	}
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
	c := &Client{
		cfg: cfg, h: h, log: log.With("component", "rtpmidi-client", "peer", raddr.String()),
		clk: newClock(), ssrc: ssrc, enc: newEncoder(), budget: budget,
		rctrl:     raddr,
		rdata:     &net.UDPAddr{IP: raddr.IP, Port: raddr.Port + 1, Zone: raddr.Zone},
		deliverCh: make(chan delivery, 1024),
		done:      make(chan struct{}),
	}
	network := "udp4"
	if raddr.IP.To4() == nil {
		network = "udp6"
	}
	if c.ctrl, c.data, err = listenClientPair(network, cfg.BindAddr); err != nil {
		return nil, err
	}
	_ = c.ctrl.SetReadBuffer(4 << 20)
	_ = c.data.SetReadBuffer(4 << 20)

	token := randUint32()
	inv := invitation{cmd: cmdIN, version: appleVersion, token: token, ssrc: ssrc, name: cfg.Name}.marshal()
	okCtrl, err := c.invite(ctx, c.ctrl, c.rctrl, inv, token)
	if err == nil {
		_, err = c.invite(ctx, c.data, c.rdata, inv, token)
	}
	if err != nil {
		c.ctrl.Close()
		c.data.Close()
		return nil, err
	}
	_ = c.ctrl.SetReadDeadline(time.Time{})
	_ = c.data.SetReadDeadline(time.Time{})

	c.l = newLink(okCtrl.name, okCtrl.ssrc, token, c.rctrl, c.rdata, cfg.Journal)
	c.l.rx.dec.budget = c.budget
	c.wg.Add(4)
	go c.readLoop(c.ctrl, false)
	go c.readLoop(c.data, true)
	go c.clockLoop()
	go c.deliverLoop()
	c.log.Info("RTP-MIDI session established", "name", c.l.name)
	return c, nil
}

func listenClientPair(network, bind string) (ctrl, data *net.UDPConn, err error) {
	var ip net.IP
	if bind != "" {
		if ip = net.ParseIP(bind); ip == nil {
			return nil, nil, fmt.Errorf("rtpmidi: bad bind address %q", bind)
		}
	}
	for i := 0; i < 100; i++ {
		ctrl, err = net.ListenUDP(network, &net.UDPAddr{IP: ip})
		if err != nil {
			continue
		}
		cp := ctrl.LocalAddr().(*net.UDPAddr).Port
		if data, err = net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: cp + 1}); err == nil {
			return ctrl, data, nil
		}
		ctrl.Close()
	}
	return nil, nil, fmt.Errorf("rtpmidi: no free local UDP port pair: %w", err)
}

// invite sends pkt (an IN) until an OK or NO with our token arrives.
func (c *Client) invite(ctx context.Context, conn *net.UDPConn, to *net.UDPAddr, pkt []byte, token uint32) (*invitation, error) {
	buf := make([]byte, 2048)
	for try := 0; try < handshakeTries; try++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.WriteToUDP(pkt, to); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(handshakeTry)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		for {
			_ = conn.SetReadDeadline(deadline)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return nil, err
			}
			p, _ := parseApple(buf[:n])
			if v, ok := p.(*invitation); ok && v.token == token {
				switch v.cmd {
				case cmdOK:
					return v, nil
				case cmdNO:
					return nil, fmt.Errorf("%w by %s", ErrRejected, to)
				}
			}
		}
	}
	return nil, fmt.Errorf("%w from %s", ErrTimeout, to)
}

// Peer describes the remote endpoint.
func (c *Client) Peer() PeerInfo { return c.l.info() }

// Synced reports whether a clock synchronisation exchange has completed.
func (c *Client) Synced() bool { return c.l.synced.Load() }

// Done is closed when the session has ended (Close, BY from the peer, timeout).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err says why the session ended, or nil if it ended through Close.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Send transmits raw MIDI (whole messages or SysEx fragments) to the peer.
func (c *Client) Send(data []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	cmds := c.enc.encode(data)
	if len(cmds) == 0 {
		return
	}
	lists := packLists(cmds)
	ts := uint32(c.clk.ticks())
	_ = c.data.SetWriteDeadline(time.Now().Add(writeTimeout))
	for _, pkt := range c.l.packetsFor(lists, c.ssrc, ts) {
		if _, err := c.data.WriteToUDP(pkt, c.rdata); err != nil {
			c.log.Debug("udp write failed", "err", err)
			return
		}
	}
}

// Close ends the session politely (BY) and releases the sockets.
func (c *Client) Close() error {
	c.shutdown(nil, true)
	c.wg.Wait()
	return nil
}

// drainDeliveries gives back what is still queued for the Handler.
func (c *Client) drainDeliveries() {
	for {
		select {
		case d := <-c.deliverCh:
			d.release()
		default:
			return
		}
	}
}

func (c *Client) shutdown(reason error, bye bool) {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = reason
		c.errMu.Unlock()
		close(c.done)
		if bye {
			_ = c.ctrl.SetWriteDeadline(time.Now().Add(writeTimeout))
			_, _ = c.ctrl.WriteToUDP(invitation{cmd: cmdBY, version: appleVersion, token: c.l.token, ssrc: c.ssrc}.marshal(), c.rctrl)
		}
		c.ctrl.Close()
		c.data.Close()
		// Settle the receive side: a packet being delivered finishes (done is
		// closed, so it cannot block), later ones are dropped, and the SysEx held
		// in reassembly and in the queue goes back to the budget.
		c.l.retire(false, nil)
		c.drainDeliveries()
	})
}

func (c *Client) writeTo(conn *net.UDPConn, to *net.UDPAddr, b []byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := conn.WriteToUDP(b, to); err != nil {
		c.log.Debug("udp write failed", "err", err)
	}
}

func (c *Client) readLoop(conn *net.UDPConn, isData bool) {
	defer c.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-c.done:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		if !from.IP.Equal(c.rctrl.IP) {
			continue
		}
		pkt := buf[:n]
		if isAppleMIDI(pkt) {
			c.handleApple(conn, from, pkt)
			continue
		}
		if !isData {
			continue
		}
		h, payload, err := parseRTP(pkt)
		if err != nil {
			continue
		}
		c.receive(h, payload)
	}
}

// receive decodes one RTP-MIDI packet and queues what it carried. Decoding and
// queueing happen under the link's rx lock, so that shutdown can settle the
// receive side (link.retire) before it drains the queue.
func (c *Client) receive(h rtpHeader, payload []byte) {
	l := c.l
	l.rxMu.Lock()
	defer l.rxMu.Unlock()
	if l.gone {
		return
	}
	l.touch()
	data := l.receive(h, payload, c.log)
	if l.rx.dec.takeStarved() {
		c.logBudget.warn(c.log, "dropping a SysEx message: the SysEx memory budget is used up",
			"in_use", c.budget.InUse(), "limit", c.budget.Limit())
	}
	if len(data) == 0 {
		return
	}
	d := delivery{p: l.info(), data: data}
	if len(data) > smallDelivery {
		d.charge, d.budget = int64(len(data)), c.budget // see Server.handleRTP
		c.budget.force(d.charge)
	}
	select {
	case c.deliverCh <- d:
	case <-c.done:
		d.release()
	}
}

func (c *Client) handleApple(conn *net.UDPConn, from *net.UDPAddr, pkt []byte) {
	p, err := parseApple(pkt)
	if err != nil {
		return
	}
	switch v := p.(type) {
	case *invitation:
		if v.cmd == cmdBY && v.ssrc == c.l.ssrc {
			c.shutdown(ErrPeerLeft, false)
		}
	case *clockSync:
		c.l.touch()
		switch v.count {
		case 0: // some peers also initiate; be a good responder
			c.writeTo(conn, from, clockSync{ssrc: c.ssrc, count: 1, ts: [3]uint64{v.ts[0], c.clk.ticks(), 0}}.marshal())
		case 1:
			now := c.clk.ticks()
			c.writeTo(conn, from, clockSync{ssrc: c.ssrc, count: 2, ts: [3]uint64{v.ts[0], v.ts[1], now}}.marshal())
			c.l.offset.Store(int64(v.ts[0]/2 + now/2 - v.ts[1]))
			c.l.synced.Store(true)
		}
	case *feedback:
		c.l.touch()
		if c.l.journal != nil {
			c.l.journal.ack(v.seq)
		}
	}
}

func (c *Client) deliverLoop() {
	defer c.wg.Done()
	for {
		select {
		case d := <-c.deliverCh:
			if c.h.OnMIDI != nil {
				c.h.OnMIDI(d.p, d.data)
			}
			d.release()
		case <-c.done:
			return
		}
	}
}

// clockLoop runs the periodic CK0 exchange, receiver feedback, and the
// watchdog that ends the session when the peer goes silent.
func (c *Client) clockLoop() {
	defer c.wg.Done()
	sendCK0 := func() {
		c.writeTo(c.data, c.rdata, clockSync{ssrc: c.ssrc, count: 0, ts: [3]uint64{c.clk.ticks()}}.marshal())
	}
	sendCK0()
	ck := time.NewTicker(ckInterval)
	watch := time.NewTicker(reapInterval)
	defer ck.Stop()
	defer watch.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ck.C:
			sendCK0()
		case <-watch.C:
			if c.l.idle() > peerTimeout {
				c.log.Info("peer timed out")
				c.shutdown(ErrPeerTimeout, false)
				return
			}
			if c.l.dirty.Swap(false) {
				c.writeTo(c.ctrl, c.rctrl, feedback{ssrc: c.ssrc, seq: uint16(c.l.rxSeq.Load())}.marshal())
			}
		}
	}
}
