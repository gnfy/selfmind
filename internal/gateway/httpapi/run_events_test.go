package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
)

func TestAggregateGatewayResponseRequiresProseAfterLastTool(t *testing.T) {
	stream := make(chan llm.StreamEvent, 5)
	stream <- llm.StreamEvent{EventType: "stream", Content: "I will inspect the repository."}
	stream <- llm.StreamEvent{EventType: "tool.started", ToolName: "read_file"}
	stream <- llm.StreamEvent{EventType: "tool.completed", ToolName: "read_file", ToolResult: "ok"}
	stream <- llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
		"status": "completed", "completion_reason": "completed",
	}}
	close(stream)

	server := &Server{}
	content, _, _, hasFinal, err := server.coordinator().aggregateGatewayResponse(
		context.Background(), "cli", nil, nil,
		&router.HandleResponse{Stream: stream, IsStreaming: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hasFinal {
		t.Fatal("progress prose before a tool call must not count as a final response")
	}
	if strings.Contains(content, "inspect the repository") {
		t.Fatalf("progress prose leaked into the materialized final response: %q", content)
	}
}

func TestAggregateGatewayResponseKeepsProseAfterLastTool(t *testing.T) {
	stream := make(chan llm.StreamEvent, 4)
	stream <- llm.StreamEvent{EventType: "tool.started", ToolName: "read_file"}
	stream <- llm.StreamEvent{EventType: "tool.completed", ToolName: "read_file", ToolResult: "ok"}
	stream <- llm.StreamEvent{EventType: "stream", Content: "The repository is healthy."}
	close(stream)

	server := &Server{}
	content, _, _, hasFinal, err := server.coordinator().aggregateGatewayResponse(
		context.Background(), "cli", nil, nil,
		&router.HandleResponse{Stream: stream, IsStreaming: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinal || content != "The repository is healthy." {
		t.Fatalf("content=%q hasFinal=%v", content, hasFinal)
	}
}

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

func TestRecordToolOutputIsLiveOnly(t *testing.T) {
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
		if event.Type == "tool.output" {
			t.Fatalf("tool.output must remain live-only, got durable event %+v", event)
		}
	}
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
	steering, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli", Platform: "cli",
		Content: "retry with token sk-secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store, DefaultTenantID: "default"}
	server.coordinator().recordStreamEvent(ctx, "cli", task, run, llm.StreamEvent{
		EventType: "agent.steering",
		Payload: map[string]interface{}{
			"steering_id":  steering.ID,
			"content_hash": steering.ContentHash,
			"input_length": len([]rune(steering.Content)),
		},
	})

	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "run.steering_consumed" {
			continue
		}
		if strings.Contains(string(event.Payload), "sk-secret-value") || !strings.Contains(string(event.Payload), "Mid-turn guidance was applied") || !strings.Contains(string(event.Payload), steering.ID) {
			t.Fatalf("consumed payload must be useful and redacted: %s", event.Payload)
		}
		if leftovers, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10); err != nil || len(leftovers) != 0 {
			t.Fatalf("mailbox not consumed: %+v err=%v", leftovers, err)
		}
		return
	}
	t.Fatal("run.steering_consumed event was not recorded")
}
