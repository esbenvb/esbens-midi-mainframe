package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The image's system interface number and the one this emmd needs are raised together:
// an emmd that needs more than the image offers is refused by the launcher on the device.
func TestOSAPIMatchesImage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "image", "board", "emm", "os-api"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("image/board/emm/os-api is not a number: %v", err)
	}
	if n != osAPI {
		t.Fatalf("image/board/emm/os-api is %d but emmd's osAPI is %d: raise both together", n, osAPI)
	}
}

func TestOSAPIFlag(t *testing.T) {
	o, err := parseFlags([]string{"--os-api"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.showOSAPI {
		t.Fatal("--os-api not parsed")
	}
}
