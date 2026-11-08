package rtpmidi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func startServer(t *testing.T, name string, h Handler) *Server {
	t.Helper()
	s := NewServer(Config{Name: name, BindAddr: testBind, Logger: quietLogger()}, h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func dial(t *testing.T, s *Server, name string, h Handler) *Client {
	t.Helper()
	cp, _ := s.Ports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Name: name, Logger: quietLogger()}, "127.0.0.1", cp, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestHandshakeAndMIDIBothWays(t *testing.T) {
	var srvRx, cliRx collector
	var peersChanged atomic.Int32
	s := startServer(t, "Esben's DX7", Handler{OnMIDI: srvRx.onMIDI, OnPeers: func() { peersChanged.Add(1) }})
	if cp, dp := s.Ports(); dp != cp+1 || cp == 0 {
		t.Fatalf("ports %d,%d", cp, dp)
	}
	c := dial(t, s, "MacBook", Handler{OnMIDI: cliRx.onMIDI})

	waitFor(t, "peer registered", func() bool { return len(s.Peers()) == 1 })
	p := s.Peers()[0]
	if p.Name != "MacBook" || p.SSRC == 0 || p.Addr == "" {
		t.Fatalf("peer info %+v", p)
	}
	// OnPeers fires just after the peer becomes visible.
	waitFor(t, "OnPeers", func() bool { return peersChanged.Load() == 1 })
	if c.Peer().Name != "Esben's DX7" {
		t.Fatalf("client sees peer %+v", c.Peer())
	}

	c.Send([]byte{0x90, 60, 100})
	c.Send([]byte{0x80, 60, 0, 0xB0, 7, 99}) // two messages in one Send
	waitFor(t, "server received", func() bool { return len(srvRx.got()) == 9 })
	if !bytes.Equal(srvRx.got(), []byte{0x90, 60, 100, 0x80, 60, 0, 0xB0, 7, 99}) {
		t.Fatalf("server got %x", srvRx.got())
	}
	srvRx.mu.Lock()
	if srvRx.peers[0].Name != "MacBook" {
		t.Fatalf("OnMIDI peer %+v", srvRx.peers[0])
	}
	srvRx.mu.Unlock()

	s.Send([]byte{0xC0, 5})
	s.Send([]byte{0xF8})
	waitFor(t, "client received", func() bool { return len(cliRx.got()) == 3 })
	if !bytes.Equal(cliRx.got(), []byte{0xC0, 5, 0xF8}) {
		t.Fatalf("client got %x", cliRx.got())
	}
}

// The default is to listen on all interfaces, with one dual-stack socket where
// IPv6 exists: an IPv4 peer then arrives with an IPv4-mapped address, and the
// session checks that compare addresses have to cope with that. Every other
// test binds loopback (see testBind), so this one keeps the default honest. It
// takes a fresh endpoint when one does not answer within two seconds, because
// on macOS such a socket is occasionally deaf to IPv4 for good (see testBind).
func TestDefaultBindAcceptsIPv4(t *testing.T) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		var rx collector
		s := NewServer(Config{Name: "wild", Logger: quietLogger()}, Handler{OnMIDI: rx.onMIDI})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		if la := s.ctrl.LocalAddr().(*net.UDPAddr); !la.IP.IsUnspecified() {
			s.Close()
			t.Fatalf("default bind is %v, want all interfaces", la)
		}
		cp, _ := s.Ports()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := Dial(ctx, Config{Name: "cli", Logger: quietLogger()}, "127.0.0.1", cp, Handler{})
		cancel()
		if err != nil {
			last = err
			s.Close()
			continue
		}
		defer s.Close()
		defer c.Close()
		waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
		c.Send([]byte{0x90, 60, 100})
		waitFor(t, "note through the default endpoint", func() bool { return bytes.Equal(rx.got(), []byte{0x90, 60, 100}) })
		return
	}
	t.Fatalf("three fresh endpoints on the default bind, none reachable over IPv4: %v", last)
}

func TestClockSynchronisation(t *testing.T) {
	s := startServer(t, "srv", Handler{})
	c := dial(t, s, "cli", Handler{})
	waitFor(t, "client synced", c.Synced)
	waitFor(t, "server synced", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, l := range s.links {
			return l.synced.Load()
		}
		return false
	})
	s.mu.Lock()
	var off int64
	for _, l := range s.links {
		off = l.offset.Load()
	}
	s.mu.Unlock()
	// Both clocks started within a moment of each other: offsets are tiny
	// (units of 100 µs), certainly less than a minute.
	if off > 600000 || off < -600000 {
		t.Fatalf("implausible clock offset %d ticks", off)
	}
}

func TestByeRemovesPeer(t *testing.T) {
	var changes atomic.Int32
	s := startServer(t, "srv", Handler{OnPeers: func() { changes.Add(1) }})
	c := dial(t, s, "cli", Handler{})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	c.Close()
	waitFor(t, "peer gone after BY", func() bool { return len(s.Peers()) == 0 })
	waitFor(t, "OnPeers twice (join, leave)", func() bool { return changes.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if changes.Load() != 2 {
		t.Fatalf("OnPeers called %d times, want exactly 2", changes.Load())
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("client Done not closed")
	}
	if c.Err() != nil {
		t.Fatalf("Err after Close: %v", c.Err())
	}
}

func TestServerCloseSaysBye(t *testing.T) {
	s := startServer(t, "srv", Handler{})
	c := dial(t, s, "cli", Handler{})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	s.Close()
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client did not notice the server leaving")
	}
	if !errors.Is(c.Err(), ErrPeerLeft) {
		t.Fatalf("Err = %v", c.Err())
	}
}

func TestPeerKeepaliveAndTimeout(t *testing.T) {
	setTimers(t, 400*time.Millisecond, 40*time.Millisecond, 60*time.Millisecond)
	s := startServer(t, "srv", Handler{})
	c := dial(t, s, "cli", Handler{})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	time.Sleep(1200 * time.Millisecond) // three timeouts' worth, but CK keeps it alive
	if len(s.Peers()) != 1 {
		t.Fatal("peer dropped although it keeps sending CK")
	}
	select {
	case <-c.Done():
		t.Fatalf("client gave up: %v", c.Err())
	default:
	}
	c.Close()
	waitFor(t, "gone", func() bool { return len(s.Peers()) == 0 })

	// Now a peer that goes silent: no more CK.
	ckInterval = time.Hour
	c2 := dial(t, s, "silent", Handler{})
	waitFor(t, "peer 2", func() bool { return len(s.Peers()) == 1 })
	waitFor(t, "silent peer reaped", func() bool { return len(s.Peers()) == 0 })
	select {
	case <-c2.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client does not notice a dead server")
	}
	// Either side may notice first: the client's own watchdog, or the BY the
	// server sends when it reaps the silent peer.
	if err := c2.Err(); !errors.Is(err, ErrPeerTimeout) && !errors.Is(err, ErrPeerLeft) {
		t.Fatalf("Err = %v", err)
	}
}

// TestClientNoticesDeadServer: a responder that completes the handshake and
// then falls silent must make the Client give up with ErrPeerTimeout.
func TestClientNoticesDeadServer(t *testing.T) {
	setTimers(t, 300*time.Millisecond, 30*time.Millisecond, time.Hour)
	lo := net.IPv4(127, 0, 0, 1)
	var ctrl, data *net.UDPConn
	var err error
	for i := 0; i < 50; i++ { // find an adjacent pair
		if ctrl, err = net.ListenUDP("udp4", &net.UDPAddr{IP: lo}); err != nil {
			t.Fatal(err)
		}
		if data, err = net.ListenUDP("udp4", &net.UDPAddr{IP: lo, Port: ctrl.LocalAddr().(*net.UDPAddr).Port + 1}); err == nil {
			break
		}
		ctrl.Close()
	}
	if err != nil {
		t.Skip("no adjacent UDP ports")
	}
	defer ctrl.Close()
	defer data.Close()
	answer := func(c *net.UDPConn) {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if p, _ := parseApple(buf[:n]); p != nil {
				if inv, ok := p.(*invitation); ok && inv.cmd == cmdIN {
					c.WriteToUDP(invitation{cmd: cmdOK, version: 2, token: inv.token, ssrc: 77, name: "zombie"}.marshal(), from)
				}
			}
		}
	}
	go answer(ctrl)
	go answer(data)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Name: "cli", Logger: quietLogger()}, "127.0.0.1", ctrl.LocalAddr().(*net.UDPAddr).Port, Handler{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Peer().Name != "zombie" || c.Peer().SSRC != 77 {
		t.Fatalf("peer %+v", c.Peer())
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client never gave up on a silent server")
	}
	if !errors.Is(c.Err(), ErrPeerTimeout) {
		t.Fatalf("Err = %v", c.Err())
	}
}

func TestTwoClientsAtOnce(t *testing.T) {
	var srvRx, rx1, rx2 collector
	s := startServer(t, "srv", Handler{OnMIDI: srvRx.onMIDI})
	c1 := dial(t, s, "one", Handler{OnMIDI: rx1.onMIDI})
	c2 := dial(t, s, "two", Handler{OnMIDI: rx2.onMIDI})
	waitFor(t, "two peers", func() bool { return len(s.Peers()) == 2 })

	s.Send([]byte{0x90, 64, 1})
	waitFor(t, "both receive the broadcast", func() bool {
		return len(rx1.got()) == 3 && len(rx2.got()) == 3
	})

	c1.Send([]byte{0xC0, 1})
	waitFor(t, "from one", func() bool { return len(srvRx.got()) == 2 })
	c2.Send([]byte{0xC1, 2})
	waitFor(t, "from two", func() bool { return len(srvRx.got()) == 4 })
	srvRx.mu.Lock()
	names := []string{srvRx.peers[0].Name, srvRx.peers[1].Name}
	srvRx.mu.Unlock()
	if names[0] != "one" || names[1] != "two" {
		t.Fatalf("peer attribution wrong: %v", names)
	}
	// A client's MIDI is not echoed to the other client by the server.
	time.Sleep(50 * time.Millisecond)
	if len(rx1.got()) != 3 || len(rx2.got()) != 3 {
		t.Fatal("server echoed client traffic")
	}

	c1.Close()
	waitFor(t, "one left", func() bool { return len(s.Peers()) == 1 })
	s.Send([]byte{0xF8})
	waitFor(t, "remaining peer still served", func() bool { return len(rx2.got()) == 4 })
}

func TestLargeSysExLoopback(t *testing.T) {
	var srvRx, cliRx collector
	s := startServer(t, "srv", Handler{OnMIDI: srvRx.onMIDI})
	c := dial(t, s, "cli", Handler{OnMIDI: cliRx.onMIDI})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })

	msg := randomSysEx(100*1024, 42)
	// Like the router: 256-byte chunks, one Send call each.
	for _, ch := range split(msg, 256) {
		c.Send(ch)
	}
	c.Send([]byte{0x90, 60, 100}) // traffic after the dump
	waitFor(t, "dump arrived at server", func() bool { return len(srvRx.got()) == len(msg)+3 })
	if !bytes.Equal(srvRx.got(), append(append([]byte(nil), msg...), 0x90, 60, 100)) {
		t.Fatal("client->server SysEx corrupted")
	}

	msg2 := randomSysEx(100*1024, 43)
	s.Send(msg2) // as one giant call this time
	waitFor(t, "dump arrived at client", func() bool { return len(cliRx.got()) == len(msg2) })
	if !bytes.Equal(cliRx.got(), msg2) {
		t.Fatal("server->client SysEx corrupted")
	}
}

func TestSendWithoutPeersKeepsEncoderConsistent(t *testing.T) {
	var cliRx collector
	s := startServer(t, "srv", Handler{})
	s.Send([]byte{0xF0, 1, 2}) // nobody is listening: first half of a dump
	c := dial(t, s, "cli", Handler{OnMIDI: cliRx.onMIDI})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	s.Send([]byte{3, 4, 0xF7}) // the late joiner sees only the tail: must not crash or emit garbage
	s.Send([]byte{0xC0, 9})
	waitFor(t, "program change", func() bool { return bytes.Equal(cliRx.got(), []byte{0xC0, 9}) })
	_ = c
}

func TestInviteValidation(t *testing.T) {
	s := startServer(t, "srv", Handler{})
	cp, dp := s.Ports()
	ctrl, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	data, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	sAddr := func(p int) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p} }
	ask := func(c *net.UDPConn, to *net.UDPAddr, pkt []byte) *invitation {
		t.Helper()
		if _, err := c.WriteToUDP(pkt, to); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1500)
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		p, err := parseApple(buf[:n])
		inv, _ := p.(*invitation)
		if err != nil || inv == nil {
			t.Fatalf("unparseable answer %x (%v)", buf[:n], err)
		}
		return inv
	}

	// Wrong protocol version.
	if a := ask(ctrl, sAddr(cp), invitation{cmd: cmdIN, version: 1, token: 7, ssrc: 100, name: "old"}.marshal()); a.cmd != cmdNO || a.token != 7 {
		t.Fatalf("wrong version: %+v", a)
	}
	// Data-port invitation without a control-port one first.
	if a := ask(data, sAddr(dp), invitation{cmd: cmdIN, version: 2, token: 8, ssrc: 101, name: "x"}.marshal()); a.cmd != cmdNO {
		t.Fatalf("data without control: %+v", a)
	}
	// Proper two-step handshake; the invitation is repeated (lost OK) and must be idempotent.
	in := invitation{cmd: cmdIN, version: 2, token: 9, ssrc: 102, name: "raw"}.marshal()
	for i := 0; i < 2; i++ {
		a := ask(ctrl, sAddr(cp), in)
		if a.cmd != cmdOK || a.token != 9 || a.name != "srv" || a.ssrc == 102 {
			t.Fatalf("control OK #%d: %+v", i, a)
		}
	}
	if len(s.Peers()) != 0 {
		t.Fatal("peer listed before the data-port step")
	}
	// A data invite with a wrong SSRC for that token is refused.
	if a := ask(data, sAddr(dp), invitation{cmd: cmdIN, version: 2, token: 9, ssrc: 999, name: "raw"}.marshal()); a.cmd != cmdNO {
		t.Fatalf("ssrc mismatch: %+v", a)
	}
	for i := 0; i < 2; i++ {
		if a := ask(data, sAddr(dp), in); a.cmd != cmdOK || a.token != 9 {
			t.Fatalf("data OK #%d: %+v", i, a)
		}
	}
	if len(s.Peers()) != 1 {
		t.Fatalf("want one peer, got %d", len(s.Peers()))
	}
	// A peer that restarts with the same SSRC and a new token replaces the old session.
	ask(ctrl, sAddr(cp), invitation{cmd: cmdIN, version: 2, token: 10, ssrc: 102, name: "raw2"}.marshal())
	if len(s.Peers()) != 0 {
		t.Fatal("old session should be dropped when the peer re-invites")
	}
	ask(data, sAddr(dp), invitation{cmd: cmdIN, version: 2, token: 10, ssrc: 102, name: "raw2"}.marshal())
	if p := s.Peers(); len(p) != 1 || p[0].Name != "raw2" {
		t.Fatalf("peers %+v", p)
	}
}

func TestMaxPeersRejects(t *testing.T) {
	old := maxPeers
	maxPeers = 2
	t.Cleanup(func() { maxPeers = old }) // registered first: runs after the servers have stopped
	s := startServer(t, "srv", Handler{})
	dial(t, s, "a", Handler{})
	dial(t, s, "b", Handler{})
	cp, _ := s.Ports()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Dial(ctx, Config{Name: "c", Logger: quietLogger()}, "127.0.0.1", cp, Handler{})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("third client: %v", err)
	}
}

func TestDialFailures(t *testing.T) {
	// Nobody answers.
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	ot, ot2 := handshakeTry, handshakeTries
	handshakeTry, handshakeTries = 40*time.Millisecond, 3
	defer func() { handshakeTry, handshakeTries = ot, ot2 }()
	_, err = Dial(context.Background(), Config{Name: "x", Logger: quietLogger()}, "127.0.0.1", silent.LocalAddr().(*net.UDPAddr).Port, Handler{})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	// Context cancelled.
	handshakeTry, handshakeTries = time.Second, 10
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Dial(ctx, Config{Name: "x", Logger: quietLogger()}, "127.0.0.1", silent.LocalAddr().(*net.UDPAddr).Port, Handler{})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("context not honoured: %v after %v", err, time.Since(start))
	}
}

func TestStartFailsOnBusyPort(t *testing.T) {
	a := startServer(t, "a", Handler{})
	cp, _ := a.Ports()
	b := NewServer(Config{Name: "b", ControlPort: cp, BindAddr: testBind, Logger: quietLogger()}, Handler{})
	if err := b.Start(); err == nil {
		b.Close()
		t.Fatal("binding a busy port succeeded")
	}
	if c, d := b.Ports(); c != 0 || d != 0 {
		t.Fatalf("ports before start: %d,%d", c, d)
	}
	b.Close()
	if err := a.Start(); err == nil {
		t.Fatal("double Start must fail")
	}
}

func TestServerRename(t *testing.T) {
	s := startServer(t, "before", Handler{})
	c1 := dial(t, s, "one", Handler{})
	s.SetName("after")
	c2 := dial(t, s, "two", Handler{})
	if c1.Peer().Name != "before" || c2.Peer().Name != "after" {
		t.Fatalf("names %q %q", c1.Peer().Name, c2.Peer().Name)
	}
}

// TestHandBuiltInitiator speaks to the server with packets assembled byte by
// byte the way CoreMIDI / rtpMIDI lay them out (including a recovery journal
// that must be skipped, the Z flag, long-form headers and receiver feedback).
func TestHandBuiltInitiator(t *testing.T) {
	setTimers(t, time.Minute, 30*time.Millisecond, time.Hour)
	var rx collector
	s := startServer(t, "srv", Handler{OnMIDI: rx.onMIDI})
	cp, dp := s.Ports()
	lo := net.IPv4(127, 0, 0, 1)
	ctrl, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	data, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	defer ctrl.Close()
	defer data.Close()
	send := func(c *net.UDPConn, port int, pkt []byte) {
		t.Helper()
		if _, err := c.WriteToUDP(pkt, &net.UDPAddr{IP: lo, Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	recv := func(c *net.UDPConn) []byte {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("timeout waiting for a packet: %v", err)
		}
		return buf[:n]
	}

	send(ctrl, cp, h(t, "FFFF 494E 00000002 DEADBEEF 01020304 4D6163 00"))
	ok := recv(ctrl)
	if !bytes.HasPrefix(ok, h(t, "FFFF 4F4B 00000002 DEADBEEF")) || !bytes.HasSuffix(ok, h(t, "73727600")) {
		t.Fatalf("control OK: %x", ok)
	}
	srvSSRC := ok[12:16]
	send(data, dp, h(t, "FFFF 494E 00000002 DEADBEEF 01020304 4D6163 00"))
	if ok := recv(data); !bytes.HasPrefix(ok, h(t, "FFFF 4F4B 00000002 DEADBEEF")) {
		t.Fatalf("data OK: %x", ok)
	}
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })

	// Clock sync: CK0 -> CK1 echoing ts1, then our CK2.
	send(data, dp, h(t, "FFFF 434B 01020304 00 000000 0000000000001111 0000000000000000 0000000000000000"))
	ck1 := recv(data)
	if len(ck1) != 36 || !bytes.HasPrefix(ck1, h(t, "FFFF 434B")) || !bytes.Equal(ck1[4:8], srvSSRC) || ck1[8] != 1 ||
		!bytes.Equal(ck1[12:20], h(t, "0000000000001111")) {
		t.Fatalf("CK1: %x", ck1)
	}
	send(data, dp, h(t, "FFFF 434B 01020304 02 000000 0000000000001111 "+hex(ck1[20:28])+" 0000000000001200"))

	// RTP-MIDI packets. SSRC 01020304; PT 0x61 with marker (E1) and without (61).
	// 1: note on, short form, with an (Apple style) journal after the command list.
	send(data, dp, h(t, "80E1 0001 00000100 01020304  43 903C64  A0 00 01 02 03 04 05"))
	// 2: Z=1, delta 5 then note off.
	send(data, dp, h(t, "8061 0002 00000200 01020304  24 05 803C40"))
	// 3: long form (B=1), 5 octets: control change, delta 0, clock.
	send(data, dp, h(t, "8061 0003 00000300 01020304  80 05 B00764 00 F8"))
	waitFor(t, "delivery", func() bool { return len(rx.got()) == 3+3+4 })
	if !bytes.Equal(rx.got(), h(t, "903C64 803C40 B00764 F8")) {
		t.Fatalf("delivered %x", rx.got())
	}

	// Receiver feedback with the sequence number in the upper 16 bits.
	var rs []byte
	for i := 0; i < 50; i++ {
		p := recv(ctrl)
		if bytes.HasPrefix(p, h(t, "FFFF 5253")) {
			rs = p
			if bytes.Equal(p[8:12], h(t, "00030000")) {
				break
			}
		}
	}
	if rs == nil || !bytes.Equal(rs[4:8], srvSSRC) || !bytes.Equal(rs[8:12], h(t, "00030000")) {
		t.Fatalf("RS: %x", rs)
	}

	// Server -> peer: an RTP-MIDI packet with the right shape.
	s.Send([]byte{0x90, 0x3C, 0x64})
	pkt := recv(data)
	hd, payload, err := parseRTP(pkt)
	if err != nil || hd.pt != 97 || !hd.marker || !bytes.Equal(pkt[8:12], srvSSRC) || !bytes.Equal(payload, h(t, "03 903C64")) {
		t.Fatalf("outgoing packet %x (%+v, %v)", pkt, hd, err)
	}
	s.Send([]byte{0x80, 0x3C, 0})
	hd2, _, _ := parseRTP(recv(data))
	if hd2.seq != hd.seq+1 || hd2.ts < hd.ts {
		t.Fatalf("seq/ts not advancing: %+v then %+v", hd, hd2)
	}

	// BY from the peer ends the session.
	send(ctrl, cp, h(t, "FFFF 4259 00000002 DEADBEEF 01020304"))
	waitFor(t, "peer gone", func() bool { return len(s.Peers()) == 0 })
}

func hex(b []byte) string {
	const d = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}
