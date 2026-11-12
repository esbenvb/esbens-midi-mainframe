package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"emm/internal/midi"
)

// ErrQueueFull: the destination did not take everything we offered (it is busy
// or too slow); part of the message was dropped.
var ErrQueueFull = errors.New("the device is not accepting data fast enough; nothing more was sent")

// PortInfo returns the description of a port that is present.
func (r *Router) PortInfo(id string) (PortInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ps := r.ports[id]
	if ps == nil {
		return PortInfo{}, false
	}
	return ps.info, true
}

// SendSysEx sends one SysEx message (F0 ... F7) to a destination port from the
// UI and returns once the port has taken all of it, so a caller sending several
// messages in a row is paced by the device. If ctx ends or the destination stops
// accepting data in the middle, the message is closed with an F7 so the device
// is not left waiting for the rest.
func (r *Router) SendSysEx(ctx context.Context, to string, msg []byte) error {
	r.mu.RLock()
	ps := r.ports[to]
	r.mu.RUnlock()
	if ps == nil {
		return ErrUnknownPort
	}
	if ps.out == nil {
		return ErrNotOutput
	}
	p := midi.NewParser()
	p.MaxChunk = r.opts.SysExChunk
	var failed error
	open := false
	p.Feed(msg, func(ev midi.Event) {
		if failed != nil {
			return
		}
		if err := ctx.Err(); err != nil {
			failed = err
		} else if !ps.out.enqueue("ui", ev) {
			failed = ErrQueueFull
		}
		if failed == nil {
			open = !ev.Last
		}
	})
	if failed != nil {
		if open {
			ps.out.push(queued{src: "ui", ev: midi.Event{Data: []byte{0xF7}, SysEx: true, Last: true}, ctl: ctlRelease}, false)
		}
		return failed
	}
	for ps.out.queuedBytes() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Millisecond):
		}
	}
	return nil
}

// ---- SysEx capture ------------------------------------------------------------

// MaxCaptureBytes is the largest SysEx message a capture keeps.
const MaxCaptureBytes = 16 << 20

// SysExCapture is what a SysEx tap delivers: either a finished message (Data
// set) or a progress report for one that is still arriving (Data nil).
type SysExCapture struct {
	Data     []byte // the whole message, F0 ... F7
	Complete bool   // it ended with F7 (false: another status byte cut it short)
	Bytes    int    // its size so far or in the end
	TooLong  bool   // it was longer than MaxCaptureBytes and is not kept
	Overrun  bool   // messages before this one were lost: the reader was too slow
}

type sysexTap struct {
	ch      chan SysExCapture
	overrun bool
}

// sysexAsm puts the chunks of a SysEx back together for the taps. Only the
// goroutine that feeds the port touches it.
type sysexAsm struct {
	on   bool
	over bool
	n    int
	buf  []byte
	prog time.Time
}

type tapSet struct {
	mu   sync.Mutex
	taps map[*sysexTap]struct{}
	n    atomic.Int32
	asm  sysexAsm
}

// TapSysEx listens in on the SysEx messages a port receives. It does not
// consume anything: the messages are routed as usual. The channel is closed when
// the port goes away or cancel is called.
func (r *Router) TapSysEx(id string) (<-chan SysExCapture, func(), error) {
	r.mu.RLock()
	ps := r.ports[id]
	r.mu.RUnlock()
	if ps == nil {
		return nil, nil, ErrUnknownPort
	}
	if !ps.info.Input {
		return nil, nil, ErrNotInput
	}
	t := &sysexTap{ch: make(chan SysExCapture, 64)}
	ts := &ps.tap
	ts.mu.Lock()
	if ts.taps == nil {
		ts.taps = map[*sysexTap]struct{}{}
	}
	ts.taps[t] = struct{}{}
	ts.n.Store(int32(len(ts.taps)))
	ts.mu.Unlock()
	return t.ch, func() { ts.remove(t) }, nil
}

func (ts *tapSet) remove(t *sysexTap) {
	ts.mu.Lock()
	if _, ok := ts.taps[t]; ok {
		delete(ts.taps, t)
		close(t.ch)
		ts.n.Store(int32(len(ts.taps)))
	}
	ts.mu.Unlock()
}

// closeAll ends every tap (the port is going away).
func (ts *tapSet) closeAll() {
	ts.mu.Lock()
	for t := range ts.taps {
		close(t.ch)
	}
	ts.taps = nil
	ts.n.Store(0)
	ts.mu.Unlock()
}

func (ts *tapSet) deliver(c SysExCapture) {
	ts.mu.Lock()
	for t := range ts.taps {
		out := c
		if c.Data != nil || c.TooLong {
			out.Overrun = t.overrun
		}
		select {
		case t.ch <- out:
			if c.Data != nil || c.TooLong {
				t.overrun = false
			}
		default:
			if c.Data != nil || c.TooLong {
				t.overrun = true // a progress report is not worth keeping, a message is
			}
		}
	}
	ts.mu.Unlock()
}

// event takes one SysEx chunk of the port's input.
func (ts *tapSet) event(ev midi.Event) {
	a := &ts.asm
	if ev.First {
		*a = sysexAsm{on: true}
	}
	if !a.on {
		return // the tap started in the middle of a message
	}
	a.n += len(ev.Data)
	if !a.over {
		if len(a.buf)+len(ev.Data) > MaxCaptureBytes {
			a.over, a.buf = true, nil
		} else {
			a.buf = append(a.buf, ev.Data...)
		}
	}
	if ev.Last {
		c := SysExCapture{Bytes: a.n, TooLong: a.over}
		if !a.over {
			c.Data = a.buf
			c.Complete = len(a.buf) > 0 && a.buf[len(a.buf)-1] == 0xF7
		}
		*a = sysexAsm{}
		ts.deliver(c)
		return
	}
	if now := time.Now(); now.Sub(a.prog) > 150*time.Millisecond {
		a.prog = now
		ts.deliver(SysExCapture{Bytes: a.n})
	}
}
