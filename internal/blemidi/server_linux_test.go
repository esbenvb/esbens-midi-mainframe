//go:build linux

package blemidi

import "testing"

// Options.OnChange is optional: SetName on a server built without one must not
// panic, and must still record the name and queue the re-advertisement.
func TestSetNameWithoutOnChange(t *testing.T) {
	s := NewServer(Options{Logger: discardLog()}, nil)
	s.SetName("Stage Rig")
	if got := s.Status().Name; got != "Stage Rig" {
		t.Fatalf("status name %q", got)
	}
	if got := s.currentName(); got != "Stage Rig" {
		t.Fatalf("name %q", got)
	}
	select {
	case <-s.rename:
	default:
		t.Fatal("no re-advertisement was requested")
	}
	s.SetName("") // empty falls back to the default and still must not panic
	if got := s.Status().Name; got != "EMM" {
		t.Fatalf("status name %q", got)
	}
}

func TestSetNameCallsOnChange(t *testing.T) {
	calls := 0
	s := NewServer(Options{Logger: discardLog(), OnChange: func() { calls++ }}, nil)
	s.SetName("A")
	s.SetName("A") // unchanged: no notification
	s.SetName("B")
	if calls != 2 {
		t.Fatalf("OnChange called %d times, want 2", calls)
	}
}
