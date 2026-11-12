package midi

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	good := [][]byte{
		{0x90, 60, 100},
		{0x80, 60, 0},
		{0xB0, 7, 127},
		{0xC5, 12},
		{0xD0, 64},
		{0xE0, 0, 64},
		{0xF2, 1, 2},
		{0xF3, 4},
		{0xF6},
		{0xFA},
		{0xF0, 0x7D, 1, 2, 3, 0xF7},
		{0xF0, 0xF7},
		{0x90, 60, 100, 0x80, 60, 0, 0xF8, 0xC0, 3},
	}
	for _, d := range good {
		if err := Validate(d); err != nil {
			t.Errorf("% X: unexpected error %v", d, err)
		}
	}
	bad := map[string][]byte{
		"empty":             {},
		"data first":        {60, 100},
		"short note":        {0x90, 60},
		"short program":     {0xC0},
		"high data byte":    {0x90, 60, 0x90},
		"status in data":    {0xB0, 0x80, 1},
		"open sysex":        {0xF0, 1, 2},
		"sysex cut by cc":   {0xF0, 1, 0xB0, 1, 2, 0xF7},
		"stray F7":          {0xF7},
		"undefined F4":      {0xF4},
		"undefined F9":      {0xF9},
		"song pos short":    {0xF2, 1},
		"sysex then data":   {0xF0, 1, 0xF7, 5},
		"note then partial": {0x90, 60, 100, 0x90, 60},
	}
	for name, d := range bad {
		if err := Validate(d); err == nil {
			t.Errorf("%s: % X was accepted", name, d)
		}
	}
}

func TestSplitSysEx(t *testing.T) {
	msgs, err := SplitSysEx([]byte{0xF0, 1, 2, 0xF7, 0xF0, 0x7D, 0xF7})
	if err != nil || len(msgs) != 2 || len(msgs[0]) != 4 || len(msgs[1]) != 3 {
		t.Fatalf("got %v, %v", msgs, err)
	}
	for name, d := range map[string][]byte{
		"empty":       {},
		"not sysex":   {0x90, 60, 100},
		"cut off":     {0xF0, 1, 2},
		"trailing":    {0xF0, 1, 0xF7, 0x00},
		"realtime in": {0xF0, 1, 0xF8, 2, 0xF7},
	} {
		if _, err := SplitSysEx(d); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := SplitSysEx([]byte{0x90}); err == nil || !strings.Contains(err.Error(), "F0") {
		t.Errorf("error should mention F0: %v", err)
	}
}
