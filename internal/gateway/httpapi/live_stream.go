package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
)

// liveStreamHub carries short-lived assistant text deltas from the daemon to
// attached interactive clients. Deltas deliberately never enter control.db:
// the synchronous message response remains the correctness source of truth,
// while this hub only improves perceived latency in the CLI.
type liveStreamHub struct {
	mu     sync.RWMutex
	nextID uint64
	subs   map[string]map[uint64]chan llm.StreamEvent
}

func newLiveStreamHub() *liveStreamHub {
	return &liveStreamHub{subs: make(map[string]map[uint64]chan llm.StreamEvent)}
}

func (h *liveStreamHub) subscribe(personID string) (<-chan llm.StreamEvent, func()) {
	ch := make(chan llm.StreamEvent, 256)
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	if h.subs[personID] == nil {
		h.subs[personID] = make(map[uint64]chan llm.StreamEvent)
	}
	h.subs[personID][id] = ch
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if group := h.subs[personID]; group != nil {
				delete(group, id)
				if len(group) == 0 {
					delete(h.subs, personID)
				}
			}
			h.mu.Unlock()
		})
	}
}

func (h *liveStreamHub) publish(personID string, event llm.StreamEvent) {
	if h == nil || personID == "" || event.EventType != "stream" || event.Content == "" {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs[personID] {
		select {
		case ch <- event:
		default:
			// Streaming is best-effort. A slow terminal receives the complete
			// final response when POST /v1/message returns, so never stall the
			// agent hot path on UI backpressure.
		}
	}
}

func (d *Server) liveStreams() *liveStreamHub {
	d.liveStreamOnce.Do(func() { d.liveStream = newLiveStreamHub() })
	return d.liveStream
}

// handleRunStream exposes person-scoped ephemeral assistant deltas. Tool,
// approval, plan, and lifecycle events remain on the durable task event path.
func (d *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if presenceClaimed(r) {
		d.touchPresence(r.Context(), identity)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := d.liveStreams().subscribe(identity.PersonID)
	defer unsubscribe()
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case event := <-events:
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: assistant.delta\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
