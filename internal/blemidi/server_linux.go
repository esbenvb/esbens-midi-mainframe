//go:build linux

package blemidi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"emm/internal/router"
)

// Server makes the Pi a Bluetooth LE MIDI peripheral through BlueZ.
//
// UNVERIFIED: nothing in this file has run against a real BlueZ or a real
// central; see the package documentation.
type Server struct {
	opts Options
	host router.Host
	log  *slog.Logger

	mu     sync.Mutex
	status Status
	name   string
	rename chan struct{}
}

// NewServer returns a Server. Nothing happens until Run.
func NewServer(o Options, host router.Host) *Server {
	o = o.withDefaults()
	return &Server{
		opts: o, host: host, log: o.Logger.With("component", "blemidi"),
		name: o.Name, status: Status{Name: o.Name},
		rename: make(chan struct{}, 1),
	}
}

// Status returns the current state of the Bluetooth service.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// SetName changes the advertised name; the advertisement is re-registered
// (and the router port renamed) if the service is up.
func (s *Server) SetName(name string) {
	if name == "" {
		name = "EMM"
	}
	s.mu.Lock()
	if s.name == name {
		s.mu.Unlock()
		return
	}
	s.name = name
	s.status.Name = name
	s.mu.Unlock()
	select {
	case s.rename <- struct{}{}:
	default:
	}
	if s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

func (s *Server) setStatus(f func(*Status)) {
	s.mu.Lock()
	before := s.status
	f(&s.status)
	after := s.status
	s.mu.Unlock()
	if before != after && s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

func (s *Server) currentName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// Run registers the MIDI service and advertisement with BlueZ and keeps them
// registered until ctx is cancelled. If BlueZ or the adapter is missing it
// logs the reason once and retries with exponential backoff (1 s to 30 s). The
// router port "ble:0" exists while the service is registered. Run returns nil
// when ctx ends.
func (s *Server) Run(ctx context.Context) error {
	backoff := time.Second
	lastErr := ""
	for ctx.Err() == nil {
		began := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			break
		}
		msg := "bluetooth session ended"
		if err != nil {
			msg = err.Error()
		}
		if msg != lastErr {
			s.log.Warn("bluetooth MIDI unavailable; will retry", "reason", msg)
			lastErr = msg
		}
		s.setStatus(func(st *Status) {
			st.Available, st.Advertising, st.Connected, st.Error = false, false, 0, msg
		})
		if time.Since(began) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
	s.setStatus(func(st *Status) {
		st.Available, st.Advertising, st.Connected = false, false, 0
	})
	return nil
}

// ---- one registration session ------------------------------------------------

func callCtx(ctx context.Context, obj dbus.BusObject, method string, args ...interface{}) error {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return obj.CallWithContext(c, method, 0, args...).Err
}

func isDBusError(err error, name string) bool {
	var de dbus.Error
	if errors.As(err, &de) {
		return de.Name == name
	}
	var pe *dbus.Error
	if errors.As(err, &pe) && pe != nil {
		return pe.Name == name
	}
	return err != nil && strings.Contains(err.Error(), name)
}

func (s *Server) session(ctx context.Context) error {
	var tree *gattTree
	onChange := func() {
		if tree != nil {
			c := tree.connectedCount()
			s.setStatus(func(st *Status) { st.Connected = c })
		}
	}
	name := s.currentName()
	tree = newGattTree(s.log, name, s.opts.Encrypt, onChange)
	defer tree.inbound.close()

	// The interceptor sees incoming messages strictly in order; it is how
	// WriteValue payloads keep their order (godbus runs handlers concurrently).
	conn, err := dbus.ConnectSystemBus(dbus.WithIncomingInterceptor(tree.intercept))
	if err != nil {
		return fmt.Errorf("system D-Bus not available: %w", err)
	}
	defer conn.Close()
	tree.bus = conn

	adapter := dbus.ObjectPath("/org/bluez/" + s.opts.Adapter)
	adapterObj := conn.Object(bluezName, adapter)
	if err := callCtx(ctx, adapterObj, ifaceProps+".GetAll", ifaceAdapter); err != nil {
		return fmt.Errorf("bluetooth adapter %s not available (is bluetoothd running and the adapter present?): %w",
			s.opts.Adapter, err)
	}
	setProp := func(prop string, v interface{}) error {
		return callCtx(ctx, adapterObj, ifaceProps+".Set", ifaceAdapter, prop, dbus.MakeVariant(v))
	}
	if err := setProp("Powered", true); err != nil {
		return fmt.Errorf("cannot power on adapter %s (blocked by rfkill?): %w", s.opts.Adapter, err)
	}
	if err := setProp("Pairable", true); err != nil {
		s.log.Warn("cannot make adapter pairable", "err", err)
	}
	if err := setProp("Alias", name); err != nil {
		s.log.Debug("cannot set adapter alias", "err", err)
	}

	ag := &agent{log: s.log}
	if err := tree.exportAll(conn, ag); err != nil {
		return fmt.Errorf("export D-Bus objects: %w", err)
	}

	// Pairing agent: "Just Works" without a screen.
	mgr := conn.Object(bluezName, "/org/bluez")
	if err := callCtx(ctx, mgr, ifaceAgentMgr+".RegisterAgent", agentPath, "NoInputNoOutput"); err != nil &&
		!isDBusError(err, "org.bluez.Error.AlreadyExists") {
		s.log.Warn("cannot register pairing agent; centrals that require pairing may fail", "err", err)
	} else if err := callCtx(ctx, mgr, ifaceAgentMgr+".RequestDefaultAgent", agentPath); err != nil {
		s.log.Warn("cannot become the default pairing agent", "err", err)
	}

	if err := callCtx(ctx, adapterObj, ifaceGattMgr+".RegisterApplication", appPath, map[string]dbus.Variant{}); err != nil {
		return fmt.Errorf("register GATT application: %w", err)
	}
	registerAdv := func() error {
		return callCtx(ctx, adapterObj, ifaceAdvMgr+".RegisterAdvertisement", advPath, map[string]dbus.Variant{})
	}
	if err := registerAdv(); err != nil {
		return fmt.Errorf("register LE advertisement: %w", err)
	}

	// Watch for centrals going away and for bluetoothd restarting.
	sigCh := make(chan *dbus.Signal, 64)
	conn.Signal(sigCh)
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface(ifaceProps),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchPathNamespace(adapter),
	); err != nil {
		return fmt.Errorf("subscribe to device changes: %w", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, bluezName),
	); err != nil {
		return fmt.Errorf("subscribe to bluetoothd restarts: %w", err)
	}

	br := newBridge(s.host, s.log, tree, s.opts.PacketInterval)
	defer br.close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go br.sendLoop(sctx)
	go br.recvLoop(sctx)

	br.addPort(name)
	defer s.host.RemovePort(PortID)
	s.setStatus(func(st *Status) {
		st.Available, st.Advertising, st.Error, st.Name = true, true, "", name
	})
	s.log.Info("bluetooth MIDI ready", "name", name, "adapter", s.opts.Adapter)

	for {
		select {
		case <-ctx.Done():
			// Best-effort tidy-up; closing the connection removes everything anyway.
			c, cc := context.WithTimeout(context.Background(), 2*time.Second)
			_ = callCtx(c, adapterObj, ifaceAdvMgr+".UnregisterAdvertisement", advPath)
			_ = callCtx(c, adapterObj, ifaceGattMgr+".UnregisterApplication", appPath)
			_ = callCtx(c, mgr, ifaceAgentMgr+".UnregisterAgent", agentPath)
			cc()
			return nil

		case sig, ok := <-sigCh:
			if !ok {
				return errors.New("D-Bus connection closed")
			}
			if err := tree.handleSignal(sig); err != nil {
				return err
			}

		case <-tree.advReleased:
			s.log.Info("BlueZ released the advertisement; registering it again")
			if err := registerAdv(); err != nil {
				return fmt.Errorf("re-register LE advertisement: %w", err)
			}

		case <-s.rename:
			nm := s.currentName()
			s.log.Info("renaming Bluetooth MIDI device", "name", nm)
			tree.setName(nm)
			_ = callCtx(ctx, adapterObj, ifaceAdvMgr+".UnregisterAdvertisement", advPath)
			if err := registerAdv(); err != nil {
				return fmt.Errorf("re-register LE advertisement: %w", err)
			}
			_ = setProp("Alias", nm)
			s.host.RemovePort(PortID) // connections are keyed by ID and survive this
			br.addPort(nm)
			s.setStatus(func(st *Status) { st.Name = nm })
		}
	}
}
