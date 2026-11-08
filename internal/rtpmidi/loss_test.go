package rtpmidi

import (
	"bytes"
	"net"
	"testing"
	"time"

	"emm/internal/router"
)

func TestHeldNotesTracker(t *testing.T) {
	var hn heldNotes
	hn.observe(h(t, "903C64 913E50 90 40 7F  B00764 F8 C005 E00040"))
	hn.observe(h(t, "903C00"))               // velocity-0 note on is a note off
	hn.observe(h(t, "814000"))               // note off on another channel: does not release ch0 note 64
	hn.observe(h(t, "F0 7E 7F 06 01 F7 F8")) // SysEx and realtime are skipped without confusing the parser
	hn.observe(h(t, "F2 01 02 F3 05 F6 90")) // system common, and a status byte with no data at the end
	got := hn.release()
	// Held now: ch0 note 64 (0x40), ch1 note 62 (0x3E).
	if !bytes.Equal(got, h(t, "804000 813E00")) {
		t.Fatalf("release: %x", got)
	}
	if again := hn.release(); len(again) != 0 {
		t.Fatalf("release did not clear: %x", again)
	}
}

func recvWire(t *testing.T, rcv *link, snd *link, enc *encoder, msg string, drop bool) []byte {
	t.Helper()
	var out []byte
	for _, p := range snd.packetsFor(packLists(enc.encode(h(t, msg))), 1, 0) {
		if drop {
			continue
		}
		hd, pl, err := parseRTP(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rcv.receive(hd, pl, quietLogger())...)
	}
	return out
}

func TestLossReleasesHeldNotes(t *testing.T) {
	for _, release := range []bool{true, false} {
		snd := newLink("s", 1, 1, nil, nil, false)
		rcv := newLink("r", 2, 1, nil, nil, false)
		rcv.releaseOnLoss = release
		enc := newEncoder()

		got := recvWire(t, rcv, snd, enc, "903C64 913E50", false)
		if !bytes.Equal(got, h(t, "903C64 913E50")) {
			t.Fatalf("first packet: %x", got)
		}
		recvWire(t, rcv, snd, enc, "803C00", true) // the Note Off is lost on the way
		got = recvWire(t, rcv, snd, enc, "C005", false)
		if release {
			// Both held notes are released before the new traffic.
			if !bytes.Equal(got, h(t, "803C00 813E00 C005")) {
				t.Fatalf("with release: %x", got)
			}
			// Nothing is released twice.
			recvWire(t, rcv, snd, enc, "803C00", true)
			if got = recvWire(t, rcv, snd, enc, "C006", false); !bytes.Equal(got, h(t, "C006")) {
				t.Fatalf("second loss with nothing held: %x", got)
			}
		} else if !bytes.Equal(got, h(t, "C005")) {
			t.Fatalf("without release: %x", got)
		}
	}
}

func TestNoReleaseWithoutLoss(t *testing.T) {
	snd := newLink("s", 1, 1, nil, nil, false)
	rcv := newLink("r", 2, 1, nil, nil, false)
	rcv.releaseOnLoss = true
	enc := newEncoder()
	var out []byte
	for _, m := range []string{"903C64", "C005", "F8", "B00764", "913E50"} {
		out = append(out, recvWire(t, rcv, snd, enc, m, false)...)
	}
	if !bytes.Equal(out, h(t, "903C64 C005 F8 B00764 913E50")) {
		t.Fatalf("held notes must survive normal traffic: %x", out)
	}
}

func startServerCfg(t *testing.T, cfg Config, hd Handler) *Server {
	t.Helper()
	cfg.Logger = quietLogger()
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

func TestServerReleasesNotesWhenPeerLeaves(t *testing.T) {
	for _, release := range []bool{true, false} {
		var rx collector
		s := startServerCfg(t, Config{Name: "srv", ReleaseNotesOnLoss: release}, Handler{OnMIDI: rx.onMIDI})
		c := dial(t, s, "cli", Handler{})
		waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })
		c.Send([]byte{0x90, 60, 100, 0x91, 62, 90})
		waitFor(t, "notes", func() bool { return len(rx.got()) == 6 })
		c.Close() // BY while holding two notes
		waitFor(t, "peer gone", func() bool { return len(s.Peers()) == 0 })
		if release {
			waitFor(t, "note offs", func() bool { return len(rx.got()) == 12 })
			if !bytes.Equal(rx.got(), h(t, "903C64 913E5A 803C00 813E00")) {
				t.Fatalf("got %x", rx.got())
			}
		} else {
			time.Sleep(100 * time.Millisecond)
			if len(rx.got()) != 6 {
				t.Fatalf("release happened although it is off: %x", rx.got())
			}
		}
	}
}

func TestServerReleasesNotesOnTimeoutAndLoss(t *testing.T) {
	setTimers(t, 300*time.Millisecond, 30*time.Millisecond, time.Hour)
	var rx collector
	s := startServerCfg(t, Config{Name: "srv", ReleaseNotesOnLoss: true}, Handler{OnMIDI: rx.onMIDI})
	cp, dp := s.Ports()
	lo := net.IPv4(127, 0, 0, 1)
	ctrl, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	data, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	defer ctrl.Close()
	defer data.Close()
	send := func(c *net.UDPConn, port int, pkt []byte) {
		if _, err := c.WriteToUDP(pkt, &net.UDPAddr{IP: lo, Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(c *net.UDPConn) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		if _, _, err := c.ReadFromUDP(buf); err != nil {
			t.Fatalf("no answer: %v", err)
		}
	}
	send(ctrl, cp, h(t, "FFFF 494E 00000002 0000AAAA 0000BBBB 78 00"))
	read(ctrl)
	send(data, dp, h(t, "FFFF 494E 00000002 0000AAAA 0000BBBB 78 00"))
	read(data)
	waitFor(t, "peer", func() bool { return len(s.Peers()) == 1 })

	// seq 1: note on. seq 2 (a Note Off) never arrives. seq 3: something else.
	send(data, dp, h(t, "8061 0001 00000100 0000BBBB  03 903C64"))
	waitFor(t, "note on", func() bool { return len(rx.got()) == 3 })
	send(data, dp, h(t, "8061 0003 00000300 0000BBBB  02 C005"))
	waitFor(t, "release + program change", func() bool { return len(rx.got()) == 3+3+2 })
	if !bytes.Equal(rx.got(), h(t, "903C64 803C00 C005")) {
		t.Fatalf("after loss: %x", rx.got())
	}

	// A second note is on when the peer goes silent: the timeout releases it.
	send(data, dp, h(t, "8061 0004 00000400 0000BBBB  03 913E50"))
	waitFor(t, "second note", func() bool { return len(rx.got()) == 11 })
	waitFor(t, "peer reaped", func() bool { return len(s.Peers()) == 0 })
	waitFor(t, "note off after timeout", func() bool { return len(rx.got()) == 14 })
	if !bytes.Equal(rx.got()[11:], h(t, "813E00")) {
		t.Fatalf("timeout release: %x", rx.got()[11:])
	}
}

func TestManagerReleasesNotesWhenPeerLeaves(t *testing.T) {
	host := newRecHost()
	m := newManager(t, host, nil, nil)
	if err := m.Sync([]Spec{{ID: "rtp:synth", Name: "Synth"}}); err != nil {
		t.Fatal(err)
	}
	synth := newMemPort("synth", false, true)
	host.AddPort(synth)
	if err := host.Connect("rtp:synth", "synth", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	c := dialManaged(t, m, "rtp:synth", "iPad", Handler{})
	waitFor(t, "peer", func() bool { return len(m.Status()[0].Peers) == 1 })
	c.Send([]byte{0x90, 60, 100})
	waitFor(t, "note on at synth", func() bool { return len(synth.bytes()) == 3 })
	c.Close()
	waitFor(t, "note off at synth", func() bool { return bytes.Equal(synth.bytes(), h(t, "903C64 803C00")) })
}
