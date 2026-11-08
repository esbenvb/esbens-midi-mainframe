package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"emm/internal/router"
)

// hub fans small named signals out to SSE clients. Slow clients lose events
// rather than slowing everyone down; every event is a hint to refetch.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct{ ch chan hubEvent }

type hubEvent struct {
	name string
	data []byte // nil: the handler computes the payload for its client
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan hubEvent, 64)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *hub) publish(name string, v any) {
	var data []byte
	if v != nil {
		data, _ = json.Marshal(v)
	}
	h.mu.Lock()
	for s := range h.subs {
		select {
		case s.ch <- hubEvent{name: name, data: data}:
		default: // client is behind; it will resync on the next topology event
		}
	}
	h.mu.Unlock()
}

func sseWrite(w http.ResponseWriter, event string, data []byte) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// activityPayload is the per-port counter delta sent in "activity" events.
type activityPayload struct {
	RxMsgs      uint64 `json:"rx_msgs"`
	RxBytes     uint64 `json:"rx_bytes"`
	TxMsgs      uint64 `json:"tx_msgs"`
	TxBytes     uint64 `json:"tx_bytes"`
	Dropped     uint64 `json:"dropped"`
	Errors      uint64 `json:"errors"`
	QueuedBytes int    `json:"queued_bytes"`
	LastRx      int64  `json:"last_rx"`
	LastTx      int64  `json:"last_tx"`
}

func toActivity(p router.PortStats) activityPayload {
	return activityPayload{p.RxMsgs, p.RxBytes, p.TxMsgs, p.TxBytes, p.Dropped, p.Errors, p.QueuedBytes, p.LastRx, p.LastTx}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	var mon <-chan router.MonitorEntry
	if r.URL.Query().Get("monitor") == "1" {
		ch, cancel := s.d.Router.SubscribeMonitor()
		defer cancel()
		mon = ch
	}

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

	// Take the activity baseline BEFORE the topology snapshot that carries the
	// per-port stats: a counter that changes in between is then seen by the
	// first tick and sent as a delta. Taking it after (as this once did) made
	// such a change look already delivered, so it never reached the client.
	last := s.d.Router.Stats()
	initial := s.buildState()
	if s.testHookInitialState != nil {
		s.testHookInitialState()
	}
	if !send("topology", initial) {
		return
	}

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var batch []router.MonitorEntry

	flushMonitor := func() bool {
		if len(batch) == 0 {
			return true
		}
		ok := send("monitor", batch)
		batch = batch[:0]
		return ok
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case ev := <-sub.ch:
			var ok bool
			switch ev.name {
			case "topology":
				ok = send("topology", s.buildState())
			case "settings":
				ok = send("settings", s.buildSettings())
			case "network":
				ok = send("network", s.networkBody())
			default:
				ok = sseWrite(w, ev.name, ev.data) == nil
				fl.Flush()
			}
			if !ok {
				return
			}

		case e := <-mon:
			batch = append(batch, e)
			if len(batch) >= 200 && !flushMonitor() {
				return
			}

		case <-tick.C:
			if !flushMonitor() {
				return
			}
			cur := s.d.Router.Stats()
			delta := map[string]activityPayload{}
			for id, c := range cur {
				if p, ok := last[id]; !ok || p != c {
					delta[id] = toActivity(c)
				}
			}
			last = cur
			if len(delta) > 0 && !send("activity", delta) {
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
