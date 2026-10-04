package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

type runEventSubscriber struct {
	ch  chan api.RunEvent
	gap atomic.Bool
	// session is the subscribing client's session; attached is the one run it
	// explicitly observes. A client that names neither sees only what concerns
	// the person as a whole.
	session  string
	attached string
}

// personWideRunEventTypes reach every session of the person: the run lifecycle
// and the human waits, so a window knows that work runs or waits elsewhere
// without receiving that work's transcript.
var personWideRunEventTypes = map[string]bool{
	"run.started": true, "run.steered": true, "run.finished": true,
	"run.cancelled": true, "run.interrupted": true, "run.failed": true,
	"approval.requested": true, "approval.approved": true, "approval.rejected": true,
	"approval.expired": true, "approval.archived": true, "approval.parked": true,
	"clarify.requested": true, "background.notice": true, "external_watch.completed": true,
}

// wants decides one event's audience, identically for live delivery and for
// replay, so a reconnect cannot show a session more than the live stream did.
func (s *runEventSubscriber) wants(event api.RunEvent) bool {
	if s.attached != "" && event.RunID == s.attached {
		return true
	}
	if personWideRunEventTypes[event.Type] {
		return true
	}
	if event.Channel == "" {
		// A missing audience on a run-scoped detail is not permission to
		// broadcast its transcript to every client of the person.
		return event.RunID == "" && event.TaskID == ""
	}
	return s.session != "" && event.Channel == s.session
}

// view keeps the full event for its originating session or an explicit
// observer. Other sessions need lifecycle and human-wait facts, not the raw
// input, approval arguments, or clarification choices carried by those events.
// Live delivery and durable replay use this same projection.
func (s *runEventSubscriber) view(event api.RunEvent) (api.RunEvent, bool) {
	if !s.wants(event) {
		return api.RunEvent{}, false
	}
	if (s.attached != "" && event.RunID != "" && event.RunID == s.attached) ||
		(s.session != "" && event.Channel != "" && event.Channel == s.session) ||
		!personWideRunEventTypes[event.Type] {
		return event, true
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &source); err != nil {
		source = nil
	}
	summary := map[string]json.RawMessage{}
	copyField := func(name string) {
		if value, ok := source[name]; ok {
			summary[name] = value
		}
	}
	copyShortField := func(name string, limit int) {
		var value string
		if err := json.Unmarshal(source[name], &value); err != nil {
			return
		}
		encoded, _ := json.Marshal(truncate(value, limit))
		summary[name] = encoded
	}
	switch event.Type {
	case "run.started":
		// Task titles can be generated from the opening user message. Keep
		// that channel-local text out of another terminal's status notice.
		copyField("origin")
	case "run.finished", "run.cancelled", "run.interrupted", "run.failed":
		var outcome struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(source["outcome"], &outcome); err == nil && outcome.Status != "" {
			summary["outcome"], _ = json.Marshal(map[string]string{"status": outcome.Status})
		}
	case "approval.requested":
		copyField("approval_id")
		copyShortField("tool", 80)
		// The target may itself be a command or contain credentials. The
		// approval id is enough for an explicit /approvals lookup.
	case "approval.approved", "approval.rejected", "approval.expired", "approval.archived", "approval.parked":
		copyField("approval_id")
	case "clarify.requested":
		copyField("clarify_id")
		// A generated question may quote the private input of this run.
		summary["question"], _ = json.Marshal("A question is waiting")
	case "background.notice":
		copyShortField("message", 160)
		copyField("kind")
	case "external_watch.completed":
		copyField("watch_id")
		copyField("status")
	}
	event.Payload, _ = json.Marshal(summary)
	return event, true
}

// runEventBroker serializes the daemon-local live view. Durable appends are
// observed after commit; assistant deltas share the same per-run live sequence
// but never enter SQLite.
type runEventBroker struct {
	store  *control.Store
	mu     sync.Mutex
	nextID uint64
	subs   map[string]map[uint64]*runEventSubscriber
	seq    map[string]uint64
	// clientDrops counts, per person, the other-session detail events their
	// terminals report having received and dropped: the audience filter's
	// miss count, which should stay zero.
	clientDrops map[string]int64
}

func newRunEventBroker(store *control.Store) *runEventBroker {
	b := &runEventBroker{
		store: store,
		subs:  make(map[string]map[uint64]*runEventSubscriber),
		seq:   make(map[string]uint64),
	}
	if store != nil {
		store.SubscribeEventAppends(b.publishDurable)
	}
	return b
}

func (b *runEventBroker) subscribe(personID, session, attached string) (*runEventSubscriber, func()) {
	sub := &runEventSubscriber{ch: make(chan api.RunEvent, 256), session: session, attached: attached}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	if b.subs[personID] == nil {
		b.subs[personID] = make(map[uint64]*runEventSubscriber)
	}
	b.subs[personID][id] = sub
	b.mu.Unlock()
	var once sync.Once
	return sub, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[personID], id)
			if len(b.subs[personID]) == 0 {
				delete(b.subs, personID)
			}
			b.mu.Unlock()
		})
	}
}

// sessionAttached reports whether a client is subscribed as this session of
// the person right now: a terminal that asked a question and is still open.
func (b *runEventBroker) sessionAttached(personID, session string) bool {
	session = strings.TrimSpace(session)
	if b == nil || session == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs[personID] {
		if sub.session == session {
			return true
		}
	}
	return false
}

// recordClientDrops adds a terminal's report of dropped other-session detail
// events and returns the person's total since the daemon started.
func (b *runEventBroker) recordClientDrops(personID string, n int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clientDrops == nil {
		b.clientDrops = make(map[string]int64)
	}
	b.clientDrops[personID] += n
	return b.clientDrops[personID]
}

func (b *runEventBroker) clientDropCount(personID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clientDrops[personID]
}

func (b *runEventBroker) publishDurable(event control.Event) {
	if event.PersonID == "" {
		return
	}
	b.publish(b.withSavedAnswer(context.Background(), durableRunEvent(event)))
}

func (b *runEventBroker) publishAssistant(task *control.Task, run *control.Run, channel string, event llm.StreamEvent) {
	if task == nil || event.Content == "" {
		return
	}
	payload, _ := json.Marshal(event)
	runID := ""
	if run != nil {
		runID = run.ID
	}
	b.publish(api.RunEvent{
		TenantID: task.TenantID, PersonID: task.PersonID,
		TaskID: task.ID, RunID: runID, Channel: channel, Type: "assistant.delta",
		Durability: api.EventEphemeral, CreatedAt: time.Now(), Payload: payload,
	})
}

func (b *runEventBroker) publish(event api.RunEvent) {
	if b == nil || event.PersonID == "" || event.Type == "" {
		return
	}
	b.mu.Lock()
	key := event.RunID
	if key == "" {
		key = "person:" + event.PersonID
	}
	b.seq[key]++
	event.LiveSeq = b.seq[key]
	for _, sub := range b.subs[event.PersonID] {
		view, ok := sub.view(event)
		if !ok {
			continue
		}
		select {
		case sub.ch <- view:
		default:
			sub.gap.Store(true)
		}
	}
	if isTerminalRunEvent(event.Type) && event.RunID != "" {
		delete(b.seq, key)
	}
	b.mu.Unlock()
}

func isTerminalRunEvent(eventType string) bool {
	switch eventType {
	case "run.finished", "run.cancelled", "run.interrupted", "run.failed":
		return true
	default:
		return false
	}
}

func (d *Server) events() *runEventBroker {
	d.eventBrokerOnce.Do(func() { d.eventBroker = newRunEventBroker(d.Control) })
	return d.eventBroker
}

// handleRunStream is retained as a compatibility route alias for one release.
// New clients use /v1/events/stream and decode the RunEvent envelope.
func (d *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	d.handleEventsStream(w, r)
}

func (d *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
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

	query := r.URL.Query()
	sub, unsubscribe := d.events().subscribe(identity.PersonID, strings.TrimSpace(query.Get("session")), strings.TrimSpace(query.Get("run")))
	defer unsubscribe()
	cursor, explicitCursor := requestedEventCursor(r)
	if !explicitCursor {
		cursor, _ = d.Control.LatestPersonEventCursor(r.Context(), identity.TenantID, identity.PersonID)
	} else if !d.replayPersonEvents(r.Context(), w, flusher, identity, sub, &cursor) {
		return
	}
	writeRunEventSSE(w, api.RunEvent{
		Cursor: cursor, TenantID: identity.TenantID, PersonID: identity.PersonID,
		Type: "ready", Durability: api.EventEphemeral, CreatedAt: time.Now(),
	})
	flusher.Flush()
	if strings.EqualFold(r.URL.Query().Get("once"), "true") {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if sub.gap.Swap(false) {
				writeRunEventSSE(w, api.RunEvent{Type: "stream.gap", Durability: api.EventEphemeral, PersonID: identity.PersonID, CreatedAt: time.Now()})
				if !d.replayPersonEvents(r.Context(), w, flusher, identity, sub, &cursor) {
					return
				}
			}
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case event := <-sub.ch:
			if sub.gap.Swap(false) {
				writeRunEventSSE(w, api.RunEvent{Type: "stream.gap", Durability: api.EventEphemeral, PersonID: identity.PersonID, CreatedAt: time.Now()})
				if !d.replayPersonEvents(r.Context(), w, flusher, identity, sub, &cursor) {
					return
				}
			}
			if event.Durability == api.EventDurable {
				if event.Cursor <= cursor {
					continue
				}
				cursor = event.Cursor
			}
			writeRunEventSSE(w, event)
			flusher.Flush()
		}
	}
}

func requestedEventCursor(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("cursor"))
	}
	if raw == "" {
		return 0, false
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	return cursor, err == nil && cursor >= 0
}

func (d *Server) replayPersonEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, identity *control.IdentityContext, sub *runEventSubscriber, cursor *int64) bool {
	for {
		events, err := d.Control.ListPersonEventsAfter(ctx, identity.TenantID, identity.PersonID, *cursor, 200)
		if err != nil {
			return false
		}
		for _, event := range events {
			if replayed, ok := sub.view(durableRunEvent(event)); ok {
				// Hydrate only after audience projection: foreign lifecycle facts
				// must not acquire the originating session's final answer.
				if (sub.session != "" && replayed.Channel == sub.session) || (sub.attached != "" && replayed.RunID == sub.attached) {
					replayed = d.events().withSavedAnswer(ctx, replayed)
				}
				writeRunEventSSE(w, replayed)
			}
			*cursor = event.Cursor
		}
		flusher.Flush()
		if len(events) < 200 {
			return true
		}
	}
}

func durableRunEvent(event control.Event) api.RunEvent {
	return api.RunEvent{
		EventID: event.ID, Cursor: event.Cursor, TenantID: event.TenantID,
		PersonID: event.PersonID, TaskID: event.TaskID, RunID: event.RunID,
		Channel: event.Channel, Type: event.Type, Durability: api.EventDurable,
		CreatedAt: event.CreatedAt, Payload: event.Payload,
	}
}

func writeRunEventSSE(w http.ResponseWriter, event api.RunEvent) {
	data, _ := json.Marshal(event)
	if event.Durability == api.EventDurable && event.Cursor > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", event.Cursor)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
}
