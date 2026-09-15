package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

type approvalPauseProvider struct {
	workSelectionProvider
	path     string
	lastTool string
}

func (p *approvalPauseProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	for _, m := range req.Messages {
		if m.Role == "tool" && p.lastTool == "" {
			p.lastTool = m.Content
		}
	}
	args, _ := json.Marshal(map[string]string{"path": p.path, "content": "must not execute"})
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{
		{ID: "pending", Function: "write_file", Args: string(args)},
		{ID: "later", Function: "write_file", Args: string(args)},
	}}
	close(out)
	return out, nil
}

func TestUnansweredApprovalPausesProductionRunBeforeNextDispatch(t *testing.T) {
	store := controltest.NewStore(t)
	path := filepath.Join(t.TempDir(), "receipt.txt")
	provider := &approvalPauseProvider{path: path}
	dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
	dispatcher.RegisterTool(tools.NewWriteFileTool())
	dispatcher.InjectMiddleware(tools.SmartApprovalMiddleware(filepath.Dir(path)))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 4, 1, nil)
	server := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default", ApprovalWait: 100 * time.Millisecond, ApprovalWaitUnattended: 100 * time.Millisecond}
	resp, status := server.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Write the receipt", ApprovalMode: "smart"})
	if status != 200 || resp.Run == nil || resp.Outcome == nil || resp.Outcome.Status != "waiting_user" || !resp.Outcome.NeedApprove || provider.calls != 1 {
		t.Fatalf("approval wait did not stop the run: status=%d calls=%d response=%+v outcome=%+v tool=%s", status, provider.calls, resp, resp.Outcome, provider.lastTool)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unapproved call wrote the receipt: %v", err)
	}
	approvals, err := store.ListApprovalRequests(context.Background(), resp.Identity.TenantID, resp.Identity.PersonID, "pending", 10)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("later call created another approval: approvals=%d err=%v", len(approvals), err)
	}
	current, err := store.GetRun(context.Background(), resp.Identity.TenantID, resp.Run.ID)
	if err != nil || current.Status != "waiting_user" {
		t.Fatalf("pause was not durable: run=%+v err=%v", current, err)
	}
}
