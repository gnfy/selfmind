package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
)

func TestEventToStreamMapping(t *testing.T) {
	cases := []struct {
		name    string
		ev      control.Event
		wantTyp string
		check   func(t *testing.T, se llm.StreamEvent)
	}{
		{
			name:    "tool started",
			ev:      control.Event{ID: "1", Type: "tool.started", Payload: mustJSON(map[string]any{"tool": "read_file", "args": "x.go", "tool_call_id": "c1"})},
			wantTyp: "tool.started",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.ToolName != "read_file" || se.ToolArgs != "x.go" || se.ToolCallID != "c1" {
					t.Fatalf("bad tool.started mapping: %+v", se)
				}
			},
		},
		{
			name:    "tool completed with duration and error",
			ev:      control.Event{ID: "2", Type: "tool.completed", Payload: mustJSON(map[string]any{"tool": "run", "result": "ok", "duration_seconds": 1.5, "error": "boom"})},
			wantTyp: "tool.completed",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.ToolResult != "ok" || se.DurationSeconds != 1.5 || se.Err == nil {
					t.Fatalf("bad tool.completed mapping: %+v", se)
				}
			},
		},
		{
			name:    "thinking",
			ev:      control.Event{ID: "3", Type: "agent.thinking", Payload: mustJSON(map[string]any{"message": "considering"})},
			wantTyp: "agent.thinking",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.Content != "considering" {
					t.Fatalf("bad thinking mapping: %+v", se)
				}
			},
		},
		{
			// Live token ticking (client-mode status bar): the daemon records
			// cumulative run usage as token.updated task events; the client must
			// forward them with a typed Usage snapshot or the TUI shows 0 tokens
			// for the whole run.
			name:    "token updated carries usage",
			ev:      control.Event{ID: "5", Type: "token.updated", Payload: mustJSON(map[string]any{"input_tokens": 1200, "output_tokens": 34})},
			wantTyp: "token.updated",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.Usage == nil || se.Usage.InputTokens != 1200 || se.Usage.OutputTokens != 34 {
					t.Fatalf("bad token.updated mapping: %+v", se)
				}
			},
		},
		{
			name:    "learning classified maps to learning.review",
			ev:      control.Event{ID: "4", Type: "learning.memory.saved", Payload: mustJSON(map[string]any{"message": "saved a fact"})},
			wantTyp: "learning.review",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.Content != "saved a fact" {
					t.Fatalf("bad learning mapping: %+v", se)
				}
			},
		},
		{
			name: "clarify requested",
			ev: control.Event{ID: "6", Type: "clarify.requested", Payload: mustJSON(map[string]any{
				"clarify_id": "clar_123",
				"question":   "Which file should I edit?",
				"choices":    []string{"a.go", "b.go"},
			})},
			wantTyp: "clarify.requested",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.Content != "Which file should I edit?" {
					t.Fatalf("bad clarify question mapping: %+v", se)
				}
				if id, _ := se.Payload["clarify_id"].(string); id != "clar_123" {
					t.Fatalf("clarify_id not carried: %+v", se.Payload)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se, ok := eventToStream(tc.ev)
			if !ok {
				t.Fatalf("expected ok mapping")
			}
			if se.EventType != tc.wantTyp {
				t.Fatalf("EventType = %q, want %q", se.EventType, tc.wantTyp)
			}
			tc.check(t, se)
		})
	}

	// Bookkeeping types are skipped (the live UI does not render them).
	for _, typ := range []string{"run.outcome", "turn.completed", "stream"} {
		if _, ok := eventToStream(control.Event{ID: "x", Type: typ}); ok {
			t.Fatalf("type %q should be skipped", typ)
		}
	}
}

// TestEventToStreamForwardsPlan verifies plan.updated is forwarded (not skipped)
// with the full structured plan payload, so client-mode TUIs can render the live
// checklist instead of dropping the plan step list.
func TestEventToStreamForwardsPlan(t *testing.T) {
	ev := control.Event{
		ID:   "p1",
		Type: "plan.updated",
		Payload: mustJSON(map[string]any{
			"explanation": "starting work",
			"plan": []map[string]any{
				{"step": "read spec", "status": "completed"},
				{"step": "write code", "status": "in_progress"},
				{"step": "run tests", "status": "pending"},
			},
		}),
	}
	se, ok := eventToStream(ev)
	if !ok {
		t.Fatalf("plan.updated should be forwarded")
	}
	if se.EventType != "plan.updated" {
		t.Fatalf("EventType = %q, want plan.updated", se.EventType)
	}
	steps, ok := se.Payload["plan"].([]interface{})
	if !ok || len(steps) != 3 {
		t.Fatalf("plan payload not forwarded intact: %+v", se.Payload)
	}
	if se.Payload["explanation"] != "starting work" {
		t.Fatalf("explanation not forwarded: %+v", se.Payload)
	}
}

// TestProcessMessageReturnsFinalAnswerAndStreamsEvents stands up a fake gateway
// and verifies (1) the synchronous final answer is returned and (2) live task
// events are replayed into the ctx stream observer.
func TestProcessMessageReturnsFinalAnswerAndStreamsEvents(t *testing.T) {
	events := []control.Event{
		// newest-first, as ListTaskEvents returns.
		{ID: "e2", Type: "tool.completed", Payload: mustJSON(map[string]any{"tool": "read_file", "result": "done", "duration_seconds": 0.2})},
		{ID: "e1", Type: "tool.started", Payload: mustJSON(map[string]any{"tool": "read_file", "args": "main.go"})},
	}
	var eventPolls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tasks/events":
			// The first poll is the baseline probe: events that pre-exist the
			// turn are suppressed, so serve this turn's events only afterwards.
			if atomic.AddInt32(&eventPolls, 1) == 1 {
				writeJSONResp(w, map[string]any{"task": map[string]any{"id": "t1"}, "events": []control.Event{}})
				return
			}
			writeJSONResp(w, map[string]any{"task": map[string]any{"id": "t1"}, "events": events})
		case "/v1/message":
			// Simulate a turn that takes a beat so the poller has time to fire.
			time.Sleep(500 * time.Millisecond)
			writeJSONResp(w, api.MessageResponse{Content: "the final answer"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")

	var mu sync.Mutex
	var got []string
	ctx := httpapi.WithStreamObserver(context.Background(), func(se llm.StreamEvent) {
		mu.Lock()
		got = append(got, se.EventType)
		mu.Unlock()
	})

	resp, status := c.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "tester", Content: "inspect main.go"})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if resp.Content != "the final answer" {
		t.Fatalf("content = %q", resp.Content)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 || got[0] != "tool.started" || got[1] != "tool.completed" {
		t.Fatalf("expected tool.started then tool.completed, got %v", got)
	}
}

func TestProcessMessageSkipsPollingForControlCommands(t *testing.T) {
	var eventsHit int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tasks/events":
			mu.Lock()
			eventsHit++
			mu.Unlock()
			writeJSONResp(w, map[string]any{"events": []control.Event{}})
		case "/v1/message":
			writeJSONResp(w, api.MessageResponse{Content: "status: idle"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	ctx := httpapi.WithStreamObserver(context.Background(), func(se llm.StreamEvent) {})
	resp, status := c.ProcessMessage(ctx, api.MessageRequest{Content: "/status"})
	if status != http.StatusOK || resp.Content != "status: idle" {
		t.Fatalf("unexpected resp %q status %d", resp.Content, status)
	}
	mu.Lock()
	defer mu.Unlock()
	if eventsHit != 0 {
		t.Fatalf("control command should not poll events, hit %d times", eventsHit)
	}
}

func TestDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dispatch" {
			http.NotFound(w, r)
			return
		}
		var req api.DispatchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Tool == "skill_manage" {
			writeJSONResp(w, api.DispatchResponse{Result: "reloaded 3 skills"})
			return
		}
		writeJSONResp(w, api.DispatchResponse{Error: "tool not allowed via dispatch: " + req.Tool})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	out, err := c.Dispatch("skill_manage", map[string]interface{}{"action": "reload"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out != "reloaded 3 skills" {
		t.Fatalf("result = %q", out)
	}

	// A tool the daemon rejects surfaces as an error.
	if _, err := c.Dispatch("terminal", map[string]interface{}{}); err == nil {
		t.Fatal("expected error for disallowed tool")
	}
}

func TestEventToStreamApprovalRequested(t *testing.T) {
	se, ok := eventToStream(control.Event{
		ID:   "a1",
		Type: "approval.requested",
		Payload: mustJSON(map[string]any{
			"approval_id": "appr-123",
			"tool":        "write_file",
			"reason":      "edits main.go",
			"target":      "main.go",
		}),
	})
	if !ok || se.EventType != "approval.requested" {
		t.Fatalf("expected approval.requested mapping, got %+v ok=%v", se, ok)
	}
	if se.ToolName != "write_file" || se.Content != "edits main.go" {
		t.Fatalf("tool/reason not mapped: %+v", se)
	}
	if id, _ := se.Payload["approval_id"].(string); id != "appr-123" {
		t.Fatalf("approval_id not carried: %+v", se.Payload)
	}
	if target, _ := se.Payload["target"].(string); target != "main.go" {
		t.Fatalf("target not carried: %+v", se.Payload)
	}
}

func TestRespondApproval(t *testing.T) {
	var gotID, gotDecision, gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/approvals/respond" {
			http.NotFound(w, r)
			return
		}
		var req api.ApprovalRespondRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotID, gotDecision, gotScope = req.ApprovalID, req.Decision, req.Scope
		writeJSONResp(w, api.ApprovalRespondResponse{})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.RespondApproval("appr-123", "approved", ""); err != nil {
		t.Fatalf("RespondApproval: %v", err)
	}
	if gotID != "appr-123" || gotDecision != "approved" || gotScope != "" {
		t.Fatalf("server saw id=%q decision=%q scope=%q", gotID, gotDecision, gotScope)
	}
	// The TUI panel's grant scope rides the same request.
	if err := c.RespondApproval("appr-123", "approved", "task"); err != nil {
		t.Fatalf("RespondApproval with scope: %v", err)
	}
	if gotScope != "task" {
		t.Fatalf("scope not threaded through: %q", gotScope)
	}
}

// TestSteerRun covers the client side of mid-turn steering: the request body,
// the endpoint, and the mapping of daemon refusals onto the typed errors the
// TUI renders (409 → ErrNoActiveRun, 429 → ErrSteerBusy).
func TestSteerRun(t *testing.T) {
	var gotText string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/steer" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req api.RunSteerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotText = req.Text
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSONResp(w, api.RunSteerResponse{Accepted: true})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.SteerRun("focus on the failing test"); err != nil {
		t.Fatalf("SteerRun: %v", err)
	}
	if gotText != "focus on the failing test" {
		t.Fatalf("server saw text = %q", gotText)
	}

	status = http.StatusConflict
	if err := c.SteerRun("late guidance"); !errors.Is(err, ErrNoActiveRun) {
		t.Fatalf("409 should map to ErrNoActiveRun, got %v", err)
	}
	status = http.StatusTooManyRequests
	if err := c.SteerRun("rapid guidance"); !errors.Is(err, ErrSteerBusy) {
		t.Fatalf("429 should map to ErrSteerBusy, got %v", err)
	}
	status = http.StatusInternalServerError
	err := c.SteerRun("broken daemon")
	if err == nil || errors.Is(err, ErrNoActiveRun) || errors.Is(err, ErrSteerBusy) {
		t.Fatalf("500 should be a generic error, got %v", err)
	}
}

func TestDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/digest" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("platform") != "cli" {
			t.Errorf("digest must identify as the cli platform, got %q", r.URL.Query().Get("platform"))
		}
		writeJSONResp(w, api.DigestResponse{
			SinceUnix:     1751600000,
			FinishedTasks: []api.DigestTask{{ID: "t1", Title: "Ship it", Status: "completed"}},
			ActiveRun:     &api.DigestActiveRun{TaskID: "t2", Title: "Long migration", ElapsedSeconds: 720},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	digest, err := c.Digest(context.Background())
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if digest.Empty() {
		t.Fatal("digest with content must not read empty")
	}
	if len(digest.FinishedTasks) != 1 || digest.FinishedTasks[0].Title != "Ship it" {
		t.Fatalf("finished tasks = %+v", digest.FinishedTasks)
	}
	if digest.ActiveRun == nil || digest.ActiveRun.ElapsedSeconds != 720 {
		t.Fatalf("active run = %+v", digest.ActiveRun)
	}
	if (&api.DigestResponse{SinceUnix: 1}).Empty() != true {
		t.Fatal("a digest with no sections must read empty")
	}
}

// TestWatchActiveRunStreamsAndDetectsRunEnd: re-attach (G0-d) — the watcher
// suppresses pre-attach history via the baseline probe, forwards fresh live
// events to the observer, and returns the outcome summary when run.finished
// lands.
func TestWatchActiveRunStreamsAndDetectsRunEnd(t *testing.T) {
	history := []control.Event{
		// Pre-attach history (newest-first) that must never be replayed.
		{ID: "old1", Type: "tool.completed", Payload: mustJSON(map[string]any{"tool": "read_file", "result": "stale"})},
	}
	live := []control.Event{
		// Newest-first: the run finishes after one fresh tool event.
		{ID: "e2", Type: "run.finished", Payload: mustJSON(map[string]any{"outcome": map[string]any{"status": "done", "summary": "migration complete"}})},
		{ID: "e1", Type: "tool.started", Payload: mustJSON(map[string]any{"tool": "terminal", "args": "make migrate"})},
		{ID: "old1", Type: "tool.completed", Payload: mustJSON(map[string]any{"tool": "read_file", "result": "stale"})},
	}
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/events" {
			http.NotFound(w, r)
			return
		}
		if atomic.AddInt32(&polls, 1) == 1 {
			writeJSONResp(w, map[string]any{"events": history})
			return
		}
		writeJSONResp(w, map[string]any{"events": live})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary := c.WatchActiveRun(ctx, func(se llm.StreamEvent) {
		mu.Lock()
		got = append(got, se.EventType)
		mu.Unlock()
	})
	if summary != "migration complete" {
		t.Fatalf("summary = %q", summary)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "tool.started" {
		t.Fatalf("observer must see only the fresh renderable event, got %v", got)
	}
}

// TestWatchActiveRunFallsBackToCurrentTaskProbe: a run that finalizes without
// a terminal event (failure paths) still ends the watch via the
// /v1/tasks/current probe.
func TestWatchActiveRunFallsBackToCurrentTaskProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tasks/events":
			writeJSONResp(w, map[string]any{"events": []control.Event{}})
		case "/v1/tasks/current":
			writeJSONResp(w, map[string]any{"task": nil, "active_run": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- c.WatchActiveRun(ctx, func(llm.StreamEvent) {}) }()
	select {
	case summary := <-done:
		if summary != "" {
			t.Fatalf("summary = %q, want empty from probe fallback", summary)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("watcher did not end via the current-task probe")
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
