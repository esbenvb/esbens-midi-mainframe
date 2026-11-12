package router

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestInjectReportsRefusal(t *testing.T) {
	r := New(Options{QueueBytes: 8})
	slow := newPort("slow", false, true)
	slow.gate = make(chan struct{})
	r.AddPort(slow)
	defer close(slow.gate)
	// The first message is taken by the writer (blocked in Write); the queue then
	// holds 8 bytes. Keep offering until a message does not fit.
	var err error
	for i := 0; i < 20 && err == nil; i++ {
		err = r.Inject("slow", []byte{0x90, 60, 100})
	}
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	if err := r.Inject("nope", []byte{0xFA}); !errors.Is(err, ErrUnknownPort) {
		t.Fatalf("unknown port: %v", err)
	}
	if err := r.Inject("slow2", []byte{0xFA}); err == nil {
		t.Fatal("expected an error for a missing port")
	}
}

func TestSendSysExWaitsForTheDevice(t *testing.T) {
	r := newRouter()
	p := newPort("p", false, true)
	r.AddPort(p)
	msg := append([]byte{0xF0, 0x7D}, bytes.Repeat([]byte{1}, 1000)...)
	msg = append(msg, 0xF7)
	if err := r.SendSysEx(context.Background(), "p", msg); err != nil {
		t.Fatal(err)
	}
	// SendSysEx returns once the port has taken everything.
	if !bytes.Equal(p.got(), msg) {
		t.Fatalf("device got %d bytes, want %d", len(p.got()), len(msg))
	}
	if len(p.chunks()) < 2 {
		t.Fatalf("a long message should go out in chunks, got %d", len(p.chunks()))
	}
}

func TestSendSysExCancelClosesTheMessage(t *testing.T) {
	r := newRouter()
	p := newPort("p", false, true)
	p.gate = make(chan struct{})
	r.AddPort(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	msg := append([]byte{0xF0, 0x7D}, bytes.Repeat([]byte{1}, 600)...)
	msg = append(msg, 0xF7)
	go func() { done <- r.SendSysEx(ctx, "p", msg) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	for i := 0; i < 10; i++ { // let the writer go on
		select {
		case p.gate <- struct{}{}:
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendSysEx did not return after cancel")
	}
}

func TestSendSysExErrors(t *testing.T) {
	r := newRouter()
	r.AddPort(newPort("in", true, false))
	if err := r.SendSysEx(context.Background(), "in", []byte{0xF0, 0xF7}); !errors.Is(err, ErrNotOutput) {
		t.Fatalf("input-only port: %v", err)
	}
	if err := r.SendSysEx(context.Background(), "x", []byte{0xF0, 0xF7}); !errors.Is(err, ErrUnknownPort) {
		t.Fatalf("missing port: %v", err)
	}
}

func recvCapture(t *testing.T, ch <-chan SysExCapture, wantData bool) SysExCapture {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatal("tap closed")
			}
			if wantData && c.Data == nil && !c.TooLong {
				continue // a progress report
			}
			return c
		case <-deadline:
			t.Fatal("no capture")
		}
	}
}

func TestTapSysExAssemblesChunks(t *testing.T) {
	r := New(Options{SysExChunk: 16, Logger: newRouter().log})
	in := newPort("in", true, false)
	h := r.AddPort(in)
	ch, cancel, err := r.TapSysEx("in")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	msg := append([]byte{0xF0, 0x43}, bytes.Repeat([]byte{5}, 100)...)
	msg = append(msg, 0xF7)
	h.Receive([]byte{0x90, 60, 100}) // other traffic is not captured
	h.Receive(msg[:40])
	h.Receive(msg[40:])
	c := recvCapture(t, ch, true)
	if !c.Complete || !bytes.Equal(c.Data, msg) || c.Bytes != len(msg) {
		t.Fatalf("capture: complete=%v bytes=%d want %d", c.Complete, c.Bytes, len(msg))
	}
	// A second message follows the first.
	h.Receive([]byte{0xF0, 1, 2, 0xF7})
	c = recvCapture(t, ch, true)
	if !bytes.Equal(c.Data, []byte{0xF0, 1, 2, 0xF7}) {
		t.Fatalf("second message: % X", c.Data)
	}
}

func TestTapSysExIsPassive(t *testing.T) {
	r := newRouter()
	in, out := newPort("in", true, false), newPort("out", false, true)
	h := r.AddPort(in)
	r.AddPort(out)
	mustConnect(t, r, "in", "out", Filter{})
	ch, cancel, _ := r.TapSysEx("in")
	defer cancel()
	h.Receive([]byte{0xF0, 1, 2, 3, 0xF7})
	recvCapture(t, ch, true)
	waitFor(t, "routed copy", func() bool { return bytes.Equal(out.got(), []byte{0xF0, 1, 2, 3, 0xF7}) })
}

func TestTapSysExStartedMidMessageWaitsForTheNext(t *testing.T) {
	r := New(Options{SysExChunk: 8, Logger: newRouter().log})
	h := r.AddPort(newPort("in", true, false))
	// Start a message before anyone listens.
	h.Receive([]byte{0xF0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	ch, cancel, _ := r.TapSysEx("in")
	defer cancel()
	h.Receive([]byte{10, 11, 0xF7}) // the end of the old message: not delivered
	h.Receive([]byte{0xF0, 9, 0xF7})
	c := recvCapture(t, ch, true)
	if !bytes.Equal(c.Data, []byte{0xF0, 9, 0xF7}) {
		t.Fatalf("got % X", c.Data)
	}
}

func TestTapSysExCutShortAndClose(t *testing.T) {
	r := newRouter()
	h := r.AddPort(newPort("in", true, false))
	ch, cancel, _ := r.TapSysEx("in")
	defer cancel()
	h.Receive([]byte{0xF0, 1, 2, 0x90, 60, 100}) // a status byte cuts the SysEx
	c := recvCapture(t, ch, true)
	if c.Complete || !bytes.Equal(c.Data, []byte{0xF0, 1, 2}) {
		t.Fatalf("cut message: complete=%v data=% X", c.Complete, c.Data)
	}
	r.RemovePort("in")
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the tap to close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tap not closed when the port went away")
	}
	if _, _, err := r.TapSysEx("in"); !errors.Is(err, ErrUnknownPort) {
		t.Fatalf("tap on a missing port: %v", err)
	}
}

func TestTapSysExRefusesOutputOnly(t *testing.T) {
	r := newRouter()
	r.AddPort(newPort("out", false, true))
	if _, _, err := r.TapSysEx("out"); !errors.Is(err, ErrNotInput) {
		t.Fatalf("got %v", err)
	}
}

func TestTapSysExFlagsOverrun(t *testing.T) {
	r := newRouter()
	h := r.AddPort(newPort("in", true, false))
	ch, cancel, _ := r.TapSysEx("in")
	defer cancel()
	for i := 0; i < 70; i++ { // nobody reads: the channel (64) fills up
		h.Receive([]byte{0xF0, byte(i), 0xF7})
	}
	n := 0
	for len(ch) > 0 {
		<-ch
		n++
	}
	if n != 64 {
		t.Fatalf("buffered %d, want 64", n)
	}
	h.Receive([]byte{0xF0, 99, 0xF7})
	c := recvCapture(t, ch, true)
	if !c.Overrun {
		t.Fatal("the message after a loss should say so")
	}
}
