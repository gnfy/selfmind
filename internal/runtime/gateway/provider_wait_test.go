package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
)

type waitProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p waitProvider) ChatCompletion(ctx context.Context, _ []llm.Message) (string, error) {
	_, err := p.Chat(ctx, llm.ChatRequest{})
	return "", err
}
func (p waitProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
	return &llm.ChatResponse{}, nil
}
func (p waitProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("not used")
}

func TestProviderWaitIsAttributedToExactRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "person")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "repo", LocalPath: filepath.Join(t.TempDir(), "repo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: workspace.ID, Title: "wait", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread, "cli", "wait for provider")
	if err != nil {
		t.Fatal(err)
	}
	gate := llm.NewRequestGate(1)
	installProviderWaitObserver(gate, store)
	p := waitProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	first := gate.Wrap(p, "route-a")
	second := gate.Wrap(p, "route-a")
	done := make(chan struct{})
	go func() {
		_, _ = first.Chat(ctx, llm.ChatRequest{})
		close(done)
	}()
	<-p.entered
	waitCtx, cancel := context.WithTimeout(llm.WithModelContext(ctx, llm.ModelContext{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
	}), 30*time.Millisecond)
	defer cancel()
	_, err = second.Chat(waitCtx, llm.ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
	close(p.release)
	<-done
	events, err := store.ListTaskEvents(ctx, thread.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "model.provider_wait" {
			continue
		}
		var payload struct {
			RouteID    string `json:"route_id"`
			Reason     string `json:"reason"`
			Canceled   bool   `json:"canceled"`
			DurationMS int64  `json:"duration_ms"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if event.RunID != run.ID || event.Visibility != "internal" || payload.RouteID != "route-a" || payload.Reason != "capacity" || !payload.Canceled || payload.DurationMS <= 0 {
			t.Fatalf("provider wait event = %+v payload = %+v", event, payload)
		}
		return
	}
	t.Fatal("missing provider wait event")
}
