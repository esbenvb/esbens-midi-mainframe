package rtpmidi

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

func TestByteBudget(t *testing.T) {
	b := NewByteBudget(100)
	if !b.reserve(60) || !b.reserve(40) {
		t.Fatal("reservations within the limit refused")
	}
	if b.reserve(1) {
		t.Fatal("reservation beyond the limit granted")
	}
	b.release(40)
	if !b.reserve(40) || b.InUse() != 100 {
		t.Fatalf("in use %d", b.InUse())
	}
	b.force(5) // memory that already exists is accounted whatever the limit
	if b.InUse() != 105 || b.reserve(1) {
		t.Fatalf("force: in use %d", b.InUse())
	}
	b.release(105)
	if b.InUse() != 0 {
		t.Fatal("not back to zero")
	}
	b.SetLimit(10)
	if b.Limit() != 10 || b.reserve(11) || !b.reserve(10) {
		t.Fatal("SetLimit")
	}
	if DefaultSysexBudget != 64<<20 || SysexBudget.Limit() != 64<<20 || maxSysexReassembly != 16<<20 {
		t.Fatalf("defaults: %d / %d / %d", DefaultSysexBudget, SysexBudget.Limit(), maxSysexReassembly)
	}
	// Concurrent use never over-commits.
	c := NewByteBudget(1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.reserve(100) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 10 || c.InUse() != 1000 {
		t.Fatalf("granted %d, in use %d", granted, c.InUse())
	}
}

func TestRateLog(t *testing.T) {
	var r rateLog
	if ok, n := r.allow(50 * time.Millisecond); !ok || n != 0 {
		t.Fatal("first call must pass")
	}
	for i := 0; i < 5; i++ {
		if ok, _ := r.allow(50 * time.Millisecond); ok {
			t.Fatal("a call inside the interval passed")
		}
	}
	time.Sleep(60 * time.Millisecond)
	if ok, n := r.allow(50 * time.Millisecond); !ok || n != 5 {
		t.Fatalf("after the interval: ok=%v suppressed=%d", ok, n)
	}
}

// segs builds the three kinds of SysEx command around n bytes of data.
func segs(n int) (first, middle, last []byte) {
	body := bytes.Repeat([]byte{0x11}, n)
	first = append(append([]byte{0xF0}, body...), 0xF0)
	middle = append(append([]byte{0xF7}, body...), 0xF0)
	last = append(append([]byte{0xF7}, body...), 0xF7)
	return
}

func TestDecoderChargesAndReleasesBudget(t *testing.T) {
	first, middle, last := segs(1000)
	b := NewByteBudget(1 << 20)
	d := listDecoder{budget: b}
	mustDecode := func(list []byte) []byte {
		t.Helper()
		out, err := d.decode(list, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	consistent := func(what string) {
		t.Helper()
		if b.InUse() != d.held {
			t.Fatalf("%s: budget has %d, decoder thinks it holds %d", what, b.InUse(), d.held)
		}
	}

	mustDecode(first)
	if d.held < 1001 {
		t.Fatalf("a buffered message must be paid for: held %d", d.held)
	}
	consistent("after first")
	mustDecode(middle)
	mustDecode(middle)
	consistent("after middle")
	if got := mustDecode(last); len(got) != 1+3*1000+1000+1 {
		t.Fatalf("completed message is %d bytes", len(got))
	}
	if b.InUse() != 0 {
		t.Fatalf("completion left %d bytes charged", b.InUse())
	}

	// Cancel.
	mustDecode(first)
	mustDecode([]byte{0xF7, 0xF4})
	if b.InUse() != 0 || d.inSx {
		t.Fatalf("cancel: %d charged", b.InUse())
	}
	// A new first segment replaces the unfinished message: no leak, no double charge.
	mustDecode(first)
	mustDecode(middle)
	mustDecode(first)
	consistent("after restart")
	if d.sx[0] != 0xF0 || len(d.sx) != 1001 {
		t.Fatalf("restart kept old data: %d bytes", len(d.sx))
	}
	// A complete message in one segment while another is unfinished drops the old one.
	mustDecode(append(append([]byte{0xF0}, 0x01, 0x02), 0xF7))
	if b.InUse() != 0 {
		t.Fatalf("complete-in-one-segment: %d charged", b.InUse())
	}
	// reset (sequence gap) and discard (session over).
	mustDecode(first)
	d.reset()
	if b.InUse() != 0 {
		t.Fatalf("reset: %d", b.InUse())
	}
	mustDecode(first)
	mustDecode(middle)
	d.discard()
	if b.InUse() != 0 || d.inSx {
		t.Fatalf("discard: %d", b.InUse())
	}
}

func TestDecoderDropsMessageThatCannotGetBudget(t *testing.T) {
	first, middle, last := segs(1000)
	b := NewByteBudget(8192)
	d := listDecoder{budget: b}
	dec := func(list []byte) []byte {
		t.Helper()
		out, err := d.decode(list, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	dec(first) // 4 KiB are reserved for the buffer
	dec(middle)
	dec(middle)
	dec(middle) // 4001 bytes: still fits
	dec(middle) // growing the buffer needs 6 KiB more while the 4 KiB are in use: out of budget
	if !d.takeStarved() {
		t.Fatal("running out of budget was not reported")
	}
	if d.takeStarved() {
		t.Fatal("the report is for one drop only")
	}
	if d.inSx || b.InUse() != 0 || d.dropped != 1 {
		t.Fatalf("after the drop: inSx=%v charged=%d dropped=%d", d.inSx, b.InUse(), d.dropped)
	}
	// The rest of the message is orphaned and ignored; the stream carries on.
	dec(middle)
	if got := dec(last); len(got) != 0 {
		t.Fatalf("a truncated message came out: %d bytes", len(got))
	}
	if got := dec(h(t, "F0 01 02 F7")); !bytes.Equal(got, h(t, "F0 01 02 F7")) {
		t.Fatalf("next message: %x", got)
	}
	if b.InUse() != 0 {
		t.Fatalf("charged %d", b.InUse())
	}
	// With room, the same message goes through.
	b.SetLimit(1 << 20)
	dec(first)
	dec(middle)
	if got := dec(last); len(got) != 1+1000+1000+1000+1 {
		t.Fatalf("message with budget: %d bytes", len(got))
	}
	if b.InUse() != 0 {
		t.Fatalf("charged %d", b.InUse())
	}
}

func TestPerMessageCap(t *testing.T) {
	setVar(t, &maxSysexReassembly, 5000)
	first, middle, last := segs(1000)
	b := NewByteBudget(1 << 20)
	d := listDecoder{budget: b}
	dec := func(list []byte) []byte {
		out, _ := d.decode(list, false, nil)
		return out
	}
	dec(first)
	for i := 0; i < 3; i++ { // 4003 bytes buffered
		dec(middle)
	}
	if !d.inSx {
		t.Fatal("dropped too early")
	}
	dec(middle) // 5004 > 5000
	if d.inSx || b.InUse() != 0 || d.dropped != 1 {
		t.Fatalf("over the cap: inSx=%v charged=%d dropped=%d", d.inSx, b.InUse(), d.dropped)
	}
	if d.takeStarved() {
		t.Fatal("hitting the per-message cap is not a lack of budget")
	}
	if got := dec(last); len(got) != 0 {
		t.Fatal("the end of an oversized message came out")
	}
	// The closing segment counts too: it may not push a message past the cap.
	d = listDecoder{budget: b}
	dec(first)
	dec(middle)
	dec(middle)
	dec(middle)
	if got := dec(last); len(got) != 0 || b.InUse() != 0 { // 5004 bytes
		t.Fatalf("final segment skipped the cap: %d bytes out, %d charged", len(got), b.InUse())
	}
	// Exactly at the cap is fine.
	f2, _, l2 := segs(2499)
	if got := dec(f2); len(got) != 0 {
		t.Fatal("first segment emitted")
	}
	if got := dec(l2); len(got) != 1+2499+2499+1 {
		t.Fatalf("message of exactly the cap: %d bytes", len(got))
	}
}

// feedLink pushes data through an encoder and the link's receive, like the wire.
type linkFeeder struct {
	t   *testing.T
	enc *encoder
	snd *link
	rcv *link
}

func newLinkFeeder(t *testing.T, b *ByteBudget) *linkFeeder {
	rcv := newLink("rcv", 2, 1, nil, nil, false)
	rcv.rx.dec.budget = b
	return &linkFeeder{t: t, enc: newEncoder(), snd: newLink("snd", 1, 1, nil, nil, false), rcv: rcv}
}

func (f *linkFeeder) feed(data []byte, drop func(i int) bool) []byte {
	f.t.Helper()
	var out []byte
	for i, p := range f.snd.packetsFor(packLists(f.enc.encode(data)), 1, 0) {
		if drop != nil && drop(i) {
			continue
		}
		hd, pl, err := parseRTP(p)
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, f.rcv.receive(hd, pl, quietLogger())...)
	}
	return out
}

func TestBudgetReleasedOnSequenceGapAndCompletion(t *testing.T) {
	b := NewByteBudget(1 << 20)
	f := newLinkFeeder(t, b)
	half := append([]byte{0xF0}, bytes.Repeat([]byte{0x22}, 6000)...)

	f.feed(half, nil)
	if b.InUse() < 6000 {
		t.Fatalf("buffered SysEx is not on the budget: %d", b.InUse())
	}
	// A packet is lost: whatever was half-received is thrown away.
	f.feed(bytes.Repeat([]byte{0x33}, 3000), func(i int) bool { return i == 0 })
	f.feed([]byte{0x34}, nil) // this one arrives after the gap
	if b.InUse() != 0 {
		t.Fatalf("a sequence gap left %d bytes charged", b.InUse())
	}
	// Completion hands the memory back too.
	f.feed(half, nil)
	if got := f.feed(append(bytes.Repeat([]byte{0x44}, 10), 0xF7), nil); len(got) != 1+6000+10+1 {
		t.Fatalf("completed message: %d bytes", len(got))
	}
	if b.InUse() != 0 {
		t.Fatalf("completion left %d bytes charged", b.InUse())
	}
	// And so does retiring a link with a half-received message.
	f.feed(half, nil)
	if b.InUse() == 0 {
		t.Fatal("nothing buffered")
	}
	f.rcv.retire(false, nil)
	if b.InUse() != 0 {
		t.Fatalf("retire left %d bytes charged", b.InUse())
	}
}

// ---- the budget end to end -------------------------------------------------

const budgetDropMsg = "dropping a SysEx message: the SysEx memory budget is used up"

func TestSysexBudgetExhaustionDropsCleanly(t *testing.T) {
	var lc logCapture
	b := NewByteBudget(256 << 10)
	var rx collector
	s := newTestServer(t, Config{Logger: lc.logger(), SysexBudget: b}, Handler{OnMIDI: rx.onMIDI})
	c := dial(t, s, "cli", Handler{})
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	note := h(t, "903C64")

	// A 1 MiB dump does not fit into a 256 KiB budget: it is dropped, whole ...
	for i, ch := range split(randomSysEx(1<<20, 11), 256) {
		c.Send(ch)
		if i%32 == 31 {
			time.Sleep(time.Millisecond)
		}
	}
	c.Send(note) // ... and what comes after it is not affected.
	waitFor(t, "traffic after the dropped dump", func() bool { return bytes.Equal(rx.got(), note) })
	waitFor(t, "budget back to zero", func() bool { return b.InUse() == 0 })
	if n := lc.count(budgetDropMsg); n != 1 {
		t.Fatalf("%d log lines about the dropped message, want exactly 1", n)
	}
	l := s.linkBySSRC(s.Peers()[0].SSRC)
	l.rxMu.Lock()
	dropped := l.rx.dec.dropped
	l.rxMu.Unlock()
	if dropped == 0 {
		t.Fatal("the drop was not counted")
	}

	// A dump that fits is delivered intact, and its memory is back once handled.
	ok := randomSysEx(60<<10, 12)
	for _, ch := range split(ok, 256) {
		c.Send(ch)
	}
	want := append(append([]byte(nil), note...), ok...)
	waitFor(t, "a dump that fits", func() bool { return bytes.Equal(rx.got(), want) })
	waitFor(t, "budget back to zero after delivery", func() bool { return b.InUse() == 0 })
}

func TestSysexBudgetReturnsToZeroWhenSessionsEnd(t *testing.T) {
	first, middle, _ := segs(3000)
	cases := []struct {
		name   string
		timers bool
		end    func(t *testing.T, s *Server, p *rawPeer)
	}{
		{"BY", false, func(t *testing.T, s *Server, p *rawPeer) { p.bye() }},
		{"timeout", true, func(t *testing.T, s *Server, p *rawPeer) {}},
		{"Close", false, func(t *testing.T, s *Server, p *rawPeer) { s.Close() }},
		{"re-invitation", false, func(t *testing.T, s *Server, p *rawPeer) {
			if got := p.ctrlIN(99, p.ssrc, "again"); got != cmdOK {
				t.Fatalf("re-invite: %v", got)
			}
		}},
		{"sequence gap", false, func(t *testing.T, s *Server, p *rawPeer) {
			p.seq += 3
			p.rtp(h(t, "C005"))
		}},
		{"cancel", false, func(t *testing.T, s *Server, p *rawPeer) { p.rtp([]byte{0xF7, 0xF4}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timers {
				setTimers(t, 500*time.Millisecond, 20*time.Millisecond, time.Hour)
			}
			b := NewByteBudget(8 << 20)
			s := newTestServer(t, Config{SysexBudget: b, ReleaseNotesOnLoss: true}, Handler{})
			p := newRawPeer(t, s)
			p.connect(1, 9, "p")
			p.rtp(first)
			for i := 0; i < 3; i++ {
				p.rtp(middle)
			}
			waitFor(t, "reassembly buffered", func() bool { return b.InUse() >= 4*3000 })
			tc.end(t, s, p)
			waitFor(t, "budget back to zero", func() bool { return b.InUse() == 0 })
		})
	}
}

func TestSysexDeliveryStaysOnBudgetUntilHandled(t *testing.T) {
	b := NewByteBudget(8 << 20)
	release := make(chan struct{})
	var rx collector
	s := newTestServer(t, Config{SysexBudget: b}, Handler{OnMIDI: func(p PeerInfo, d []byte) {
		<-release
		rx.onMIDI(p, d)
	}})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open) // before the server is closed: Close waits for a handler that is stuck
	p := newRawPeer(t, s)
	p.connect(1, 9, "p")
	first, middle, last := segs(3000)
	const msgLen = 1 + 3000 + 3*3000 + 3001
	// Three complete messages arrive while the handler is busy with the first.
	for i := 0; i < 3; i++ {
		p.rtp(first)
		for j := 0; j < 3; j++ {
			p.rtp(middle)
		}
		p.rtp(last)
	}
	waitFor(t, "queued messages are on the budget", func() bool { return b.InUse() == 3*msgLen })
	open()
	waitFor(t, "all delivered", func() bool { return len(rx.got()) == 3*msgLen })
	waitFor(t, "budget back to zero", func() bool { return b.InUse() == 0 })
}

func TestClientReturnsSysexBudget(t *testing.T) {
	for _, how := range []string{"Close", "peer leaves"} {
		t.Run(how, func(t *testing.T) {
			s := startServer(t, "srv", Handler{})
			b := NewByteBudget(8 << 20)
			cp, _ := s.Ports()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, Config{Name: "cli", Logger: quietLogger(), SysexBudget: b}, "127.0.0.1", cp, Handler{})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
			s.Send(append([]byte{0xF0}, bytes.Repeat([]byte{0x11}, 20000)...)) // no F7: never ends
			waitFor(t, "client buffers the dump", func() bool { return b.InUse() >= 20000 })
			if how == "Close" {
				c.Close()
			} else {
				s.Close() // BY: the client ends the session by itself
				select {
				case <-c.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("client did not notice the server leaving")
				}
			}
			waitFor(t, "budget back to zero", func() bool { return b.InUse() == 0 })
		})
	}
}

func TestServerCloseReturnsQueuedSysexBudget(t *testing.T) {
	b := NewByteBudget(8 << 20)
	release := make(chan struct{})
	var rx collector
	s := NewServer(Config{Name: "srv", BindAddr: testBind, Logger: quietLogger(), SysexBudget: b}, Handler{OnMIDI: func(p PeerInfo, d []byte) {
		<-release
		rx.onMIDI(p, d)
	}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	open := sync.OnceFunc(func() { close(release) })
	defer open()
	p := newRawPeer(t, s)
	p.connect(1, 9, "p")
	first, middle, last := segs(3000)
	const msgLen = 1 + 3000 + 3*3000 + 3001
	const n = 12
	for i := 0; i < n; i++ {
		p.rtp(first)
		for j := 0; j < 3; j++ {
			p.rtp(middle)
		}
		p.rtp(last)
	}
	waitFor(t, "queued messages are on the budget", func() bool { return b.InUse() == n*msgLen })

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond) // Close is waiting for the stuck handler
	open()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	// Whatever the delivery loop did not get to is given back by Close.
	if got := b.InUse(); got != 0 {
		t.Fatalf("%d bytes still charged after Close", got)
	}
}

func TestClientCloseReturnsQueuedSysexBudget(t *testing.T) {
	s := startServer(t, "srv", Handler{})
	b := NewByteBudget(8 << 20)
	release := make(chan struct{})
	cp, _ := s.Ports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Name: "cli", Logger: quietLogger(), SysexBudget: b}, "127.0.0.1", cp,
		Handler{OnMIDI: func(PeerInfo, []byte) { <-release }})
	if err != nil {
		t.Fatal(err)
	}
	open := sync.OnceFunc(func() { close(release) })
	defer open()
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
	const n = 12
	for i := 0; i < n; i++ {
		s.Send(randomSysEx(20000, int64(i)))
	}
	waitFor(t, "queued messages are on the budget", func() bool { return b.InUse() == n*20002 })
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	open()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if got := b.InUse(); got != 0 {
		t.Fatalf("%d bytes still charged after Close", got)
	}
}
