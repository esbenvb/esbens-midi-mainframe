//go:build !linux

package blemidi

import (
	"context"
	"log/slog"
	"sync"

	"emm/internal/router"
)

// Server is a stand-in on platforms without BlueZ so the daemon builds
// everywhere (e.g. on a laptop when developing with simulated devices).
//
// Run logs once that Bluetooth MIDI needs Linux, registers no router port,
// and then blocks until ctx is done (it returns nil, not an error, so the
// daemon does not treat a missing feature as a failure). Status stays
// "unavailable" with an explanatory Error.
type Server struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	status Status
}

const unsupportedMsg = "bluetooth MIDI is only available on Linux"

// NewServer returns a stub Server.
func NewServer(o Options, _ router.Host) *Server {
	o = o.withDefaults()
	return &Server{opts: o, log: o.Logger.With("component", "blemidi"),
		status: Status{Name: o.Name, Error: unsupportedMsg}}
}

// Run blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info(unsupportedMsg)
	<-ctx.Done()
	return nil
}

// Status always reports the service as unavailable.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// SetName records the name; there is nothing to re-advertise.
func (s *Server) SetName(name string) {
	if name == "" {
		name = "EMM"
	}
	s.mu.Lock()
	s.status.Name = name
	s.mu.Unlock()
	if s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}
