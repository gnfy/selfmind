package httpapi

// A run the daemon starts on the person's behalf (a watcher finalization, a
// cron fire) must be identifiable by attached clients, which render it as a
// result line instead of replaying its progress. run.started carries the
// origin; these tests pin both the classification and the wire contract.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func TestRunOriginClassifiesDaemonStartedRuns(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		req  api.MessageRequest
		want string
	}{
		{
			name: "person turn has no origin",
			ctx:  context.Background(),
			req:  api.MessageRequest{Content: "fix the failing test"},
			want: "",
		},
		{
			name: "explicit request origin wins",
			ctx:  kernel.WithTurnSource(context.Background(), "cron"),
			req:  api.MessageRequest{Origin: "backfill", WatchID: "watch_1"},
			want: "backfill",
		},
		{
			name: "watch id implies a watcher finalization",
			ctx:  context.Background(),
			req:  api.MessageRequest{WatchID: "watch_1"},
			want: runOriginWatch,
		},
		{
			name: "turn source is the synchronous-path fallback",
			ctx:  kernel.WithTurnSource(context.Background(), "cron"),
			req:  api.MessageRequest{},
			want: runOriginCron,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runOrigin(tc.ctx, tc.req); got != tc.want {
				t.Fatalf("runOrigin = %q, want %q", got, tc.want)
			}
		})
	}
}

// An async run executes under a fresh context.Background(), so the origin must
// ride the request to reach run.started. This is the contract the TUI reads to
// keep background work out of the transcript.
func TestApprovalOriginAndSourceReachRunStartedEvent(t *testing.T) {
	provider := newSlowLLMProvider("Three builds succeeded overnight.")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "parked approval task", Channel: "cli"})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	provider.releaseNow()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		TenantID:         identity.TenantID,
		Platform:         "cli",
		PlatformUserID:   "local",
		Channel:          "cli",
		Content:          "resume the parked approval",
		TaskID:           task.ID,
		Origin:           runOriginApproval,
		SourceApprovalID: "apr_source",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("approval continuation = status %d resp %+v", status, resp)
	}

	events, err := store.ListTaskEvents(ctx, resp.Task.ID, 50)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	var started *struct {
		Origin           string `json:"origin"`
		WatchID          string `json:"watch_id"`
		SourceApprovalID string `json:"source_approval_id"`
	}
	for _, event := range events {
		if event.Type != "run.started" {
			continue
		}
		var payload struct {
			Origin           string `json:"origin"`
			WatchID          string `json:"watch_id"`
			SourceApprovalID string `json:"source_approval_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("run.started payload: %v", err)
		}
		started = &payload
		break
	}
	if started == nil {
		t.Fatal("no run.started event recorded")
	}
	if started.Origin != runOriginApproval || started.SourceApprovalID != "apr_source" {
		t.Fatalf("run.started provenance = %+v", started)
	}
	if strings.TrimSpace(started.WatchID) != "" {
		t.Fatalf("approval continuation must not claim a watch id: %q", started.WatchID)
	}
}
