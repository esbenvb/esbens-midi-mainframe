package rtpmidi

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestInteropServe is a manual harness, skipped unless EMM_INTEROP_PORT is set.
// It runs an endpoint on that control port for EMM_INTEROP_SECONDS (default
// 20) so that a real implementation (CoreMIDI via a Swift script, rtpMIDI,
// rtpmidid, …) can connect to it. It prints what it receives as "RX <hex>",
// echoes everything back, and once a peer is connected sends a note and a
// 5000-byte SysEx so the peer's receive side is exercised too.
//
//	EMM_INTEROP_PORT=5504 go test -tags netgo -run TestInteropServe -v ./internal/rtpmidi
func TestInteropServe(t *testing.T) {
	ps := os.Getenv("EMM_INTEROP_PORT")
	if ps == "" {
		t.Skip("manual interop harness; set EMM_INTEROP_PORT")
	}
	port, _ := strconv.Atoi(ps)
	secs := 20
	if v, err := strconv.Atoi(os.Getenv("EMM_INTEROP_SECONDS")); err == nil {
		secs = v
	}
	var s *Server
	sent := false
	s = NewServer(Config{Name: "EMM Interop", ControlPort: port, Logger: quietLogger()}, Handler{
		OnMIDI: func(p PeerInfo, d []byte) {
			if len(d) > 16 {
				fmt.Printf("RX from %q: %d bytes, starts % X ... ends % X\n", p.Name, len(d), d[:8], d[len(d)-4:])
			} else {
				fmt.Printf("RX from %q: % X\n", p.Name, d)
			}
			s.Send(d) // echo
		},
		OnPeers: func() {
			ps := s.Peers()
			fmt.Printf("PEERS: %d %+v\n", len(ps), ps)
			if len(ps) > 0 && !sent {
				sent = true
				go func() {
					time.Sleep(1500 * time.Millisecond)
					s.Send([]byte{0x90, 0x3C, 0x64})
					dump := randomSysEx(5000, 9)
					for _, c := range split(dump, 256) {
						s.Send(c)
					}
					fmt.Printf("TX: note + %d-byte SysEx\n", len(dump))
				}()
			}
		},
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, d := s.Ports()
	fmt.Printf("LISTENING control=%d data=%d\n", c, d)
	time.Sleep(time.Duration(secs) * time.Second)
	fmt.Println("DONE")
}
