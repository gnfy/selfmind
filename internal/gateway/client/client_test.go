package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
			name:    "learning classified maps to learning.review",
			ev:      control.Event{ID: "4", Type: "learning.memory.saved", Payload: mustJSON(map[string]any{"message": "saved a fact"})},
			wantTyp: "learning.review",
			check: func(t *testing.T, se llm.StreamEvent) {
				if se.Content != "saved a fact" {
					t.Fatalf("bad learning mapping: %+v", se)
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
	for _, typ := range []string{"plan.updated", "run.outcome", "turn.completed", "stream"} {
		if _, ok := eventToStream(control.Event{ID: "x", Type: typ}); ok {
			t.Fatalf("type %q should be skipped", typ)
		}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tasks/events":
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
}

func TestRespondApproval(t *testing.T) {
	var gotID, gotDecision string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/approvals/respond" {
			http.NotFound(w, r)
			return
		}
		var req api.ApprovalRespondRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotID, gotDecision = req.ApprovalID, req.Decision
		writeJSONResp(w, api.ApprovalRespondResponse{})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.RespondApproval("appr-123", "approved"); err != nil {
		t.Fatalf("RespondApproval: %v", err)
	}
	if gotID != "appr-123" || gotDecision != "approved" {
		t.Fatalf("server saw id=%q decision=%q", gotID, gotDecision)
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
