package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel/llm"
)

// TestCLIAsyncResultRoutesToPreferredIM encodes the continuity promise for
// fire-and-forget terminal runs: the final answer must reach the person's
// preferred IM endpoint instead of vanishing (observed live: a rejected
// approval's acknowledgment was invisible, so the rejection looked like a
// no-op). With a single bound account, that account is the preferred one.
func TestCLIAsyncResultRoutesToPreferredIM(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.coordinator().deliverAsyncResult(ctx, identity,
		api.MessageRequest{Platform: "cli", Channel: "cli"},
		api.MessageResponse{
			Content: "The user rejected chmod; nothing else was run.",
			Task:    task,
		})

	if len(recorder.messages) != 1 {
		t.Fatalf("messages = %+v", recorder.messages)
	}
	msg := recorder.messages[0]
	if msg.Platform != "weixin" || msg.PlatformUserID != "wxid_123" {
		t.Fatalf("target = %s/%s", msg.Platform, msg.PlatformUserID)
	}
	if msg.Content != "The user rejected chmod; nothing else was run." {
		t.Fatalf("result content should be the final answer only:\n%s", msg.Content)
	}
}

func TestRecordToolOutputKeepsCorrelationFields(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Output", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "run command")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store, DefaultTenantID: "default"}
	server.coordinator().recordStreamEvent(ctx, "cli", task, run, llm.StreamEvent{
		EventType:  "tool.output",
		ToolName:   "terminal",
		ToolCallID: "call-1",
		Content:    "line one",
	})

	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "tool.output" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["tool"] != "terminal" || payload["tool_call_id"] != "call-1" || payload["message"] != "line one" {
			t.Fatalf("payload = %+v", payload)
		}
		return
	}
	t.Fatal("tool.output event was not recorded")
}

func TestRecordSteeringConsumedIsDurableAndRedacted(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Steer", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "start")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store, DefaultTenantID: "default"}
	server.coordinator().recordStreamEvent(ctx, "cli", task, run, llm.StreamEvent{
		EventType: "agent.steering",
		Payload:   map[string]interface{}{"input": "retry with token sk-secret-value"},
	})

	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "run.steering_consumed" {
			continue
		}
		if strings.Contains(string(event.Payload), "sk-secret-value") || !strings.Contains(string(event.Payload), "Mid-turn guidance was applied") {
			t.Fatalf("consumed payload must be useful and redacted: %s", event.Payload)
		}
		return
	}
	t.Fatal("run.steering_consumed event was not recorded")
}
