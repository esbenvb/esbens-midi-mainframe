package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"emm/internal/config"
	"emm/internal/netcfg"
)

// When the flush behind the network manager's Save fails, the manager rolls
// the change back. The store must roll back with it: otherwise the settings
// the manager refused are still pending and get written by the next flush.
func TestNetworkSaveFailureRestoresStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	card := filepath.Join(dir, "card")
	if err := os.Mkdir(card, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(card, "state.json")
	store, err := config.Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // leave no retry timer behind
		os.Chmod(card, 0o755)
		_ = store.Flush()
	})
	save := saveNetwork(store)

	first := netcfg.Saved{WifiSSID: "home", WifiPassword: "first-password"}
	if err := save(first); err != nil {
		t.Fatalf("save on a healthy card: %v", err)
	}
	want, _ := json.Marshal(first)

	// The card stops accepting writes: the next Save must fail and leave no trace.
	if err := os.Chmod(card, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := save(netcfg.Saved{WifiSSID: "away", WifiPassword: "second-password"}); err == nil {
		t.Fatal("Save must report the failed flush")
	}
	if got := store.Snapshot().Network; string(got) != string(want) {
		t.Fatalf("store kept the refused settings: %s (want %s)", got, want)
	}

	// The card recovers; what reaches the disk is the state the manager kept.
	if err := os.Chmod(card, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := config.Open(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Network; string(got) != string(want) {
		t.Fatalf("disk holds %s, want %s", got, want)
	}
}

// With nothing saved before, a failed Save leaves the network settings unset.
func TestNetworkSaveFailureWithNothingSavedBefore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	card := filepath.Join(t.TempDir(), "card")
	if err := os.Mkdir(card, 0o555); err != nil {
		t.Fatal(err)
	}
	store, _ := config.Open(filepath.Join(card, "state.json"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		os.Chmod(card, 0o755)
		_ = store.Flush()
	})
	if err := saveNetwork(store)(netcfg.Saved{WifiSSID: "x", WifiPassword: "password1"}); err == nil {
		t.Fatal("Save must report the failed flush")
	}
	if got := store.Snapshot().Network; len(got) != 0 {
		t.Fatalf("store kept refused settings: %s", got)
	}
}
