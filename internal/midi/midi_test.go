package midi

import (
	"bytes"
	"reflect"
	"testing"
)

func feed(p *Parser, chunks ...[]byte) []Event {
	var out []Event
	for _, c := range chunks {
		p.Feed(c, func(e Event) { out = append(out, e) })
	}
	return out
}

func datas(evs []Event) [][]byte {
	var out [][]byte
	for _, e := range evs {
		out = append(out, e.Data)
	}
	return out
}

func TestParserBasicMessages(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{0x90, 60, 100, 0x80, 60, 0, 0xC1, 5, 0xE0, 0, 64, 0xF8, 0xF2, 1, 2, 0xF6}))
	want := [][]byte{
		{0x90, 60, 100}, {0x80, 60, 0}, {0xC1, 5}, {0xE0, 0, 64}, {0xF8}, {0xF2, 1, 2}, {0xF6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserRunningStatus(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{0x90, 60, 100, 62, 90, 64, 80}))
	want := [][]byte{{0x90, 60, 100}, {0x90, 62, 90}, {0x90, 64, 80}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserRunningStatusSurvivesRealtime(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{0x90, 60, 0xF8, 100, 62, 0xF8, 90}))
	want := [][]byte{{0xF8}, {0x90, 60, 100}, {0xF8}, {0x90, 62, 90}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserSplitAcrossFeeds(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{0x90}, []byte{60}, []byte{100, 0xB0}, []byte{7}, []byte{127}))
	want := [][]byte{{0x90, 60, 100}, {0xB0, 7, 127}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserCommonCancelsRunningStatus(t *testing.T) {
	p := NewParser()
	// After F6 (tune request) running status is cancelled, so lone data bytes are stray.
	got := datas(feed(p, []byte{0x90, 60, 100, 0xF6, 62, 90}))
	want := [][]byte{{0x90, 60, 100}, {0xF6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserStatusInterruptsPartialMessage(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{0x90, 60, 0xB0, 7, 99}))
	want := [][]byte{{0xB0, 7, 99}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParserStrayBytes(t *testing.T) {
	p := NewParser()
	got := datas(feed(p, []byte{1, 2, 3, 0xF7, 0x90, 60, 100}))
	want := [][]byte{{0x90, 60, 100}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestSysExComplete(t *testing.T) {
	p := NewParser()
	evs := feed(p, []byte{0xF0, 0x7E, 0x7F, 0x06, 0x01, 0xF7})
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	if !e.SysEx || !e.First || !e.Last || !bytes.Equal(e.Data, []byte{0xF0, 0x7E, 0x7F, 0x06, 0x01, 0xF7}) {
		t.Fatalf("bad event %+v", e)
	}
	if e.Class() != ClassSysEx || e.Category() != CatSysEx {
		t.Fatal("classification")
	}
}

func TestSysExChunking(t *testing.T) {
	p := NewParser()
	p.MaxChunk = 8
	msg := []byte{0xF0}
	for i := 0; i < 30; i++ {
		msg = append(msg, byte(i))
	}
	msg = append(msg, 0xF7)
	evs := feed(p, msg)
	var joined []byte
	for i, e := range evs {
		if !e.SysEx {
			t.Fatal("not sysex")
		}
		if i == 0 && !e.First {
			t.Fatal("first flag")
		}
		if i != 0 && e.First {
			t.Fatal("unexpected first flag")
		}
		if i == len(evs)-1 && !e.Last {
			t.Fatal("last flag")
		}
		if i != len(evs)-1 && e.Last {
			t.Fatal("unexpected last flag")
		}
		if len(e.Data) > 8 {
			t.Fatalf("chunk too big: %d", len(e.Data))
		}
		joined = append(joined, e.Data...)
	}
	if !bytes.Equal(joined, msg) {
		t.Fatalf("reassembly mismatch")
	}
}

func TestSysExAcrossFeedsFlushesImmediately(t *testing.T) {
	p := NewParser()
	a := feed(p, []byte{0xF0, 1, 2, 3})
	if len(a) != 1 || a[0].Last || !a[0].First {
		t.Fatalf("first feed: %+v", a)
	}
	b := feed(p, []byte{4, 5, 0xF7})
	if len(b) != 1 || !b[0].Last || b[0].First {
		t.Fatalf("second feed: %+v", b)
	}
	if !bytes.Equal(b[0].Data, []byte{4, 5, 0xF7}) {
		t.Fatalf("second chunk data %x", b[0].Data)
	}
}

func TestSysExRealtimeInterleaved(t *testing.T) {
	p := NewParser()
	evs := feed(p, []byte{0xF0, 1, 2, 0xF8, 3, 0xF7})
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(evs), evs)
	}
	if !evs[0].SysEx || evs[0].Last || !bytes.Equal(evs[0].Data, []byte{0xF0, 1, 2}) {
		t.Fatalf("e0 %+v", evs[0])
	}
	if evs[1].SysEx || !bytes.Equal(evs[1].Data, []byte{0xF8}) {
		t.Fatalf("e1 %+v", evs[1])
	}
	if !evs[2].SysEx || !evs[2].Last || !bytes.Equal(evs[2].Data, []byte{3, 0xF7}) {
		t.Fatalf("e2 %+v", evs[2])
	}
}

func TestSysExAbortedByStatus(t *testing.T) {
	p := NewParser()
	evs := feed(p, []byte{0xF0, 1, 2, 0x90, 60, 100})
	// chunk (flushed at end of data before status), then abort marker, then note
	last := evs[len(evs)-1]
	if last.SysEx || !bytes.Equal(last.Data, []byte{0x90, 60, 100}) {
		t.Fatalf("note not delivered: %+v", evs)
	}
	var sawLast bool
	for _, e := range evs {
		if e.SysEx && e.Last {
			sawLast = true
			if len(e.Data) > 0 && e.Data[len(e.Data)-1] == 0xF7 {
				t.Fatal("aborted sysex must not gain an F7")
			}
		}
	}
	if !sawLast {
		t.Fatal("aborted sysex never terminated")
	}
}

func TestSysExRestartWithoutTerminator(t *testing.T) {
	p := NewParser()
	evs := feed(p, []byte{0xF0, 1, 0xF0, 2, 0xF7})
	var lasts int
	for _, e := range evs {
		if e.Last {
			lasts++
		}
	}
	if lasts != 2 {
		t.Fatalf("want 2 terminated sysexes, got %d (%+v)", lasts, evs)
	}
}

func TestCategoryAndChannel(t *testing.T) {
	cases := []struct {
		d   []byte
		cat Category
		ch  int
		ok  bool
	}{
		{[]byte{0x93, 60, 1}, CatNote, 3, true},
		{[]byte{0x83, 60, 1}, CatNote, 3, true},
		{[]byte{0xA0, 60, 1}, CatPolyAT, 0, true},
		{[]byte{0xBF, 1, 1}, CatCC, 15, true},
		{[]byte{0xC0, 1}, CatProgram, 0, true},
		{[]byte{0xD0, 1}, CatChanAT, 0, true},
		{[]byte{0xE0, 1, 1}, CatBend, 0, true},
		{[]byte{0xF8}, CatClock, 0, false},
		{[]byte{0xFA}, CatTransport, 0, false},
		{[]byte{0xFE}, CatActiveSense, 0, false},
		{[]byte{0xFF}, CatReset, 0, false},
		{[]byte{0xF2, 0, 0}, CatCommon, 0, false},
	}
	for _, c := range cases {
		e := Event{Data: c.d}
		if e.Category() != c.cat {
			t.Errorf("%x: category %v want %v", c.d, e.Category(), c.cat)
		}
		ch, ok := e.Channel()
		if ch != c.ch || ok != c.ok {
			t.Errorf("%x: channel %d,%v want %d,%v", c.d, ch, ok, c.ch, c.ok)
		}
	}
}

func TestCategoryNamesRoundTrip(t *testing.T) {
	for _, n := range CategoryNames() {
		c, ok := ParseCategory(n)
		if !ok || c.String() != n {
			t.Errorf("round trip failed for %q", n)
		}
	}
	if _, ok := ParseCategory("nope"); ok {
		t.Error("ParseCategory accepted junk")
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(Event{Data: []byte{0x90, 60, 100}}); got != "Note On ch1 C4 vel 100" {
		t.Error(got)
	}
	if got := Describe(Event{Data: []byte{0x90, 60, 0}}); got != "Note Off ch1 C4" {
		t.Error(got)
	}
	if got := Describe(Event{Data: []byte{0xE0, 0, 64}}); got != "Pitch Bend ch1 0" {
		t.Error(got)
	}
	if got := Describe(Event{Data: []byte{0xF8}}); got != "Clock" {
		t.Error(got)
	}
}
