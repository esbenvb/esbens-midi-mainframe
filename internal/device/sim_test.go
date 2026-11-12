package device

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"emm/internal/router"
)

type sink struct {
	mu   sync.Mutex
	data []byte
}

func (s *sink) Info() router.PortInfo {
	return router.PortInfo{ID: "test:sink", Name: "sink", Kind: router.KindVirtual, Output: true}
}
func (s *sink) Write(b []byte) error {
	s.mu.Lock()
	s.data = append(s.data, b...)
	s.mu.Unlock()
	return nil
}
func (s *sink) Close() error { return nil }
func (s *sink) len() int     { s.mu.Lock(); defer s.mu.Unlock(); return len(s.data) }

func TestSimDevicesAppearAndProduceTraffic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := router.New(router.Options{Logger: log})
	snk := &sink{}
	r.AddPort(snk)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = NewSim(SimOptions{Logger: log}).Run(ctx, r); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for !(r.HasPort("sim:keyboard") && r.HasPort("sim:drums") && r.HasPort("sim:synth") && r.HasPort("sim:sampler")) {
		if time.Now().After(deadline) {
			t.Fatal("sim ports did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.Connect("sim:keyboard", "test:sink", router.Filter{}); err != nil {
		t.Fatal(err)
	}
	for snk.len() < 3 {
		if time.Now().After(deadline.Add(3 * time.Second)) {
			t.Fatal("keyboard produced no traffic")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The synth answers a note-on with CC 74; route it back to the sink.
	_ = r.Connect("sim:synth", "test:sink", router.Filter{Block: []string{"note"}})
	before := snk.len()
	if err := r.Inject("sim:synth", []byte{0x90, 60, 100}); err != nil {
		t.Fatal(err)
	}
	for snk.len() == before {
		if time.Now().After(deadline.Add(6 * time.Second)) {
			t.Fatal("synth never answered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sim did not stop")
	}
	if r.HasPort("sim:keyboard") {
		t.Fatal("ports must be removed on shutdown")
	}
}

func TestSimSynthAnswersSysExRequests(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := router.New(router.Options{Logger: log})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = NewSim(SimOptions{Logger: log}).Run(ctx, r) }()
	deadline := time.Now().Add(3 * time.Second)
	for !r.HasPort("sim:synth") {
		if time.Now().After(deadline) {
			t.Fatal("sim synth did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ch, stop, err := r.TapSysEx("sim:synth")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next := func() router.SysExCapture {
		for {
			select {
			case c, ok := <-ch:
				if !ok {
					t.Fatal("tap closed")
				}
				if c.Data != nil {
					return c
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no SysEx from the synth")
			}
		}
	}
	if err := r.SendSysEx(ctx, "sim:synth", simIdentityRequest); err != nil {
		t.Fatal(err)
	}
	if c := next(); !c.Complete || c.Data[3] != 0x06 || c.Data[4] != 0x02 {
		t.Fatalf("identity reply: % X", c.Data)
	}
	if err := r.SendSysEx(ctx, "sim:synth", simDumpRequest); err != nil {
		t.Fatal(err)
	}
	if c := next(); !c.Complete || len(c.Data) != 3+1024+1 || c.Data[2] != 0x02 {
		t.Fatalf("dump: %d bytes", len(c.Data))
	}
}
