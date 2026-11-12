package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"emm/internal/midi"
	"emm/internal/router"
)

// The console and the SysEx page talk to wired devices only (USB, and the
// simulated ones in development): network and Bluetooth ports are fine for
// playing but not for dumps and sends that must arrive complete.

const (
	maxConsoleBytes = 16384    // one console send
	maxSysexUpload  = 16 << 20 // one uploaded .syx
	defaultSysexGap = 20       // ms between the messages of an upload
	maxSysexGap     = 5000
)

// wiredPort finds the port named in the URL and checks that it is a wired
// device. On failure it has already answered.
func (s *Server) wiredPort(w http.ResponseWriter, r *http.Request) (router.PortInfo, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "invalid port id")
		return router.PortInfo{}, false
	}
	info, ok := s.d.Router.PortInfo(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such port: is the device plugged in?")
		return router.PortInfo{}, false
	}
	if !info.Kind.IsDevice() {
		writeErr(w, http.StatusForbidden, "only wired devices (USB) can be used here, not network or Bluetooth ports")
		return router.PortInfo{}, false
	}
	return info, true
}

func routerStatus(err error) int {
	switch {
	case errors.Is(err, router.ErrUnknownPort):
		return http.StatusNotFound
	case errors.Is(err, router.ErrQueueFull):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// POST /api/ports/{id}/send {"data":[144,60,100]}: complete MIDI messages, sent
// to a wired device as they are.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	info, ok := s.wiredPort(w, r)
	if !ok {
		return
	}
	if !info.Output {
		writeErr(w, http.StatusBadRequest, "this port cannot receive MIDI")
		return
	}
	var req struct {
		Data []int `json:"data"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Data) > maxConsoleBytes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("too much for one send (%d bytes at most): use the SysEx page for dumps", maxConsoleBytes))
		return
	}
	data := make([]byte, len(req.Data))
	for i, v := range req.Data {
		if v < 0 || v > 255 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("byte %d is %d: bytes are 0-255", i+1, v))
			return
		}
		data[i] = byte(v)
	}
	if err := midi.Validate(data); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.d.Router.Inject(info.ID, data); err != nil {
		writeErr(w, routerStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": len(data)})
}

// POST /api/ports/{id}/sysex[?gap_ms=N] with the raw bytes of a .syx file as the
// body: sends its messages one after the other, waiting for the device to take
// each (and gap_ms after it), and answers when the last one is out. Cancelling
// the request stops after the message in progress.
func (s *Server) handleSysexSend(w http.ResponseWriter, r *http.Request) {
	info, ok := s.wiredPort(w, r)
	if !ok {
		return
	}
	if !info.Output {
		writeErr(w, http.StatusBadRequest, "this port cannot receive MIDI")
		return
	}
	gap := defaultSysexGap
	if v := r.URL.Query().Get("gap_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxSysexGap {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("gap_ms must be 0-%d", maxSysexGap))
			return
		}
		gap = n
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSysexUpload)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is larger than %d MB", maxSysexUpload>>20))
		} else {
			writeErr(w, http.StatusBadRequest, "could not read the upload: "+err.Error())
		}
		return
	}
	msgs, err := midi.SplitSysEx(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, busy := s.sysexBusy.LoadOrStore(info.ID, struct{}{}); busy {
		writeErr(w, http.StatusConflict, "another SysEx transfer to this device is still running")
		return
	}
	defer s.sysexBusy.Delete(info.ID)

	for i, m := range msgs {
		if err := s.d.Router.SendSysEx(r.Context(), info.ID, m); err != nil {
			if r.Context().Err() != nil {
				return // the browser went away or cancelled
			}
			writeErr(w, routerStatus(err), fmt.Sprintf("stopped at message %d of %d: %v", i+1, len(msgs), err))
			return
		}
		if gap > 0 && i < len(msgs)-1 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Duration(gap) * time.Millisecond):
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"messages": len(msgs), "bytes": len(body)})
}

// GET /api/ports/{id}/sysex/listen: a server-sent event stream of the SysEx
// messages a wired device sends. Events:
//
//	ready     {} once listening has begun
//	progress  {"bytes":N} while a message is still arriving
//	sysex     {"data":"<base64>","bytes":N,"complete":true,"overrun":false} a finished message
//	toolong   {"bytes":N} a message over the size limit, not kept
//	closed    {} the device went away
//
// It only listens: whatever the device sends is still routed as usual.
func (s *Server) handleSysexListen(w http.ResponseWriter, r *http.Request) {
	info, ok := s.wiredPort(w, r)
	if !ok {
		return
	}
	if !info.Input {
		writeErr(w, http.StatusBadRequest, "this port does not send MIDI")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, cancel, err := s.d.Router.TapSysEx(info.ID)
	if err != nil {
		writeErr(w, routerStatus(err), err.Error())
		return
	}
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if sseWrite(w, event, b) != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send("ready", struct{}{}) {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c, open := <-ch:
			if !open {
				send("closed", struct{}{})
				return
			}
			var ok bool
			switch {
			case c.TooLong:
				ok = send("toolong", map[string]any{"bytes": c.Bytes, "overrun": c.Overrun})
			case c.Data != nil:
				ok = send("sysex", map[string]any{
					"data": base64.StdEncoding.EncodeToString(c.Data), "bytes": len(c.Data),
					"complete": c.Complete, "overrun": c.Overrun,
				})
			default:
				ok = send("progress", map[string]int{"bytes": c.Bytes})
			}
			if !ok {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// isSysexUpload: the one call that carries raw bytes instead of JSON.
func isSysexUpload(r *http.Request) bool {
	p := r.URL.Path
	return r.Method == http.MethodPost && strings.HasPrefix(p, "/api/ports/") && strings.HasSuffix(p, "/sysex")
}
