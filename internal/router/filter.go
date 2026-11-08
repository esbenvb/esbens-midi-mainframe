package router

import (
	"fmt"

	"emm/internal/midi"
)

// compiledFilter is a Filter reduced to bit masks for the hot path.
type compiledFilter struct {
	block     uint32 // bit per midi.Category
	chanMask  uint16 // allowed channels; 0xFFFF when unrestricted
	mapCh     int    // 0 = off, else forced channel 0..15
	hasMapCh  bool
	transpose int
}

// Validate checks a Filter without applying it.
func (f Filter) Validate() error {
	_, err := f.compile()
	return err
}

// Normalize returns the filter with canonical (sorted, de-duplicated) lists,
// so equal filters compare and serialise equally.
func (f Filter) Normalize() Filter {
	out := Filter{MapChannel: f.MapChannel, Transpose: f.Transpose}
	seen := map[midi.Category]bool{}
	for c := midi.Category(0); c < midi.NumCategories; c++ {
		for _, n := range f.Block {
			if cc, ok := midi.ParseCategory(n); ok && cc == c && !seen[c] {
				seen[c] = true
				out.Block = append(out.Block, c.String())
			}
		}
	}
	allowed := [17]bool{}
	for _, ch := range f.Channels {
		if ch >= 1 && ch <= 16 {
			allowed[ch] = true
		}
	}
	n := 0
	for ch := 1; ch <= 16; ch++ {
		if allowed[ch] {
			out.Channels = append(out.Channels, ch)
			n++
		}
	}
	if n == 16 { // everything allowed == no restriction
		out.Channels = nil
	}
	return out
}

func (f Filter) compile() (compiledFilter, error) {
	cf := compiledFilter{chanMask: 0xFFFF, transpose: f.Transpose}
	for _, n := range f.Block {
		c, ok := midi.ParseCategory(n)
		if !ok {
			return cf, fmt.Errorf("unknown category %q", n)
		}
		cf.block |= 1 << uint(c)
	}
	if len(f.Channels) > 0 {
		cf.chanMask = 0
		for _, ch := range f.Channels {
			if ch < 1 || ch > 16 {
				return cf, fmt.Errorf("channel %d out of range 1..16", ch)
			}
			cf.chanMask |= 1 << uint(ch-1)
		}
	}
	if f.MapChannel != 0 {
		if f.MapChannel < 1 || f.MapChannel > 16 {
			return cf, fmt.Errorf("map_channel %d out of range 1..16", f.MapChannel)
		}
		cf.mapCh, cf.hasMapCh = f.MapChannel-1, true
	}
	if f.Transpose < -48 || f.Transpose > 48 {
		return cf, fmt.Errorf("transpose %d out of range -48..48", f.Transpose)
	}
	return cf, nil
}

func (cf *compiledFilter) blocksSysEx() bool { return cf.block&(1<<uint(midi.CatSysEx)) != 0 }

// notesDiffer reports whether o treats a note-off differently from cf: blocks
// it, moves it to another channel or another note. When it does, notes that
// were let through under cf may never be released under o.
func (cf *compiledFilter) notesDiffer(o *compiledFilter) bool {
	const noteBit = 1 << uint(midi.CatNote)
	return cf.block&noteBit != o.block&noteBit ||
		cf.chanMask != o.chanMask ||
		cf.hasMapCh != o.hasMapCh || cf.mapCh != o.mapCh ||
		cf.transpose != o.transpose
}

// apply returns the (possibly rewritten) event and whether it passes.
// The returned event never aliases ev.Data when it was modified.
func (cf *compiledFilter) apply(ev midi.Event) (midi.Event, bool) {
	if cf.block&(1<<uint(ev.Category())) != 0 {
		return ev, false
	}
	if ev.SysEx || len(ev.Data) == 0 {
		return ev, true
	}
	ch, isChan := ev.Channel()
	if !isChan {
		return ev, true
	}
	if cf.chanMask&(1<<uint(ch)) == 0 {
		return ev, false
	}
	if !cf.hasMapCh && cf.transpose == 0 {
		return ev, true
	}
	d := append([]byte(nil), ev.Data...)
	if cf.hasMapCh {
		d[0] = d[0]&0xF0 | byte(cf.mapCh)
	}
	if cf.transpose != 0 {
		switch d[0] & 0xF0 {
		case 0x80, 0x90, 0xA0:
			n := int(d[1]) + cf.transpose
			if n < 0 || n > 127 {
				return ev, false
			}
			d[1] = byte(n)
		}
	}
	return midi.Event{Data: d}, true
}

// noteTracker remembers which notes a connection currently holds on its
// destination, so they can be released if the connection or source vanishes.
// It follows notes only: a sustain pedal (CC 64) that is down when the
// connection goes away stays down on the destination until a pedal-up arrives
// from somewhere or Panic is used.
type noteTracker [16][128 / 8]byte

func (t *noteTracker) observe(ev midi.Event) {
	if ev.SysEx || len(ev.Data) < 3 {
		return
	}
	ch, ok := ev.Channel()
	if !ok {
		return
	}
	n := ev.Data[1] & 0x7F
	switch ev.Data[0] & 0xF0 {
	case 0x90:
		if ev.Data[2] != 0 {
			t[ch][n>>3] |= 1 << (n & 7)
			return
		}
		fallthrough
	case 0x80:
		t[ch][n>>3] &^= 1 << (n & 7)
	}
}

// release returns note-off messages for every held note and clears the tracker.
func (t *noteTracker) release() [][]byte {
	var out [][]byte
	for ch := 0; ch < 16; ch++ {
		for n := 0; n < 128; n++ {
			if t[ch][n>>3]&(1<<(uint(n)&7)) != 0 {
				out = append(out, []byte{0x80 | byte(ch), byte(n), 0})
			}
		}
		t[ch] = [128 / 8]byte{}
	}
	return out
}
