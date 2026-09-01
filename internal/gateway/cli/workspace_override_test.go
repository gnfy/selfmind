package cli

// Session workspace override: a successful `/workspace <n|id>` this session
// must pin the resolved workspace on every subsequent request (explicit
// WorkspaceID wins over cwd derivation server-side) and the status bar must
// show where the next message actually runs. Without this the switch was a
// no-op for later CLI turns (observed live: file ops in the selected
// workspace tripped out-of-root approvals because the turn silently ran in
// the launch dir).

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

const switchReply = "Current workspace: game (ws_123)\n/mnt/d/wwwroot/ai/game"

func TestWorkspaceSelectPinsSessionOverride(t *testing.T) {
	c := NewController("", "", nil, "")
	m := c.model

	var captured []api.MessageRequest
	c.SetMessageProcessor(func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		captured = append(captured, req)
		return api.MessageResponse{Content: switchReply}, 200
	})

	// Fresh session: no override rides the request and the bar shows the cwd.
	if req := m.controlMessageRequest("/status"); req.WorkspaceID != "" {
		t.Fatalf("fresh session must not send a workspace override, got %q", req.WorkspaceID)
	}
	if line := stripANSI(m.statusLine()); !strings.Contains(line, currentWorkingDir()) {
		t.Fatalf("fresh session status line should show cwd: %q", line)
	}

	msg := m.handleWorkspaceSelect([]string{"2"})()
	switched, ok := msg.(MsgWorkspaceSwitched)
	if !ok {
		t.Fatalf("successful switch should yield MsgWorkspaceSwitched, got %T", msg)
	}
	if switched.ID != "ws_123" || switched.Name != "game" || switched.Path != "/mnt/d/wwwroot/ai/game" {
		t.Fatalf("bad parse of switch reply: %+v", switched)
	}
	if len(captured) != 1 || captured[0].Content != "/workspace 2" {
		t.Fatalf("switch must relay the raw command to the gateway: %+v", captured)
	}
	if _, cmd := m.Update(switched); cmd != nil {
		// MsgWorkspaceSwitched re-renders through MsgAgentDone; drain the cmd.
		_ = cmd
	}
	if m.workspaceOverrideID != "ws_123" {
		t.Fatalf("override not pinned: %q", m.workspaceOverrideID)
	}
	// The reply text still renders in the transcript like any control reply.
	found := false
	for _, message := range m.messages {
		if strings.Contains(message.Content, "Current workspace: game") {
			found = true
		}
	}
	if !found {
		t.Fatalf("switch reply should render in the transcript: %+v", m.messages)
	}

	// Subsequent control requests carry the override (covers /new inheriting it).
	if req := m.controlMessageRequest("/new fix the loader"); req.WorkspaceID != "ws_123" {
		t.Fatalf("control request must carry the override, got %q", req.WorkspaceID)
	}

	// Subsequent agent turns carry the override too.
	if msg := m.runAgent(context.Background(), "hello")(); msg == nil {
		t.Fatal("runAgent returned no message")
	}
	last := captured[len(captured)-1]
	if last.Content != "hello" || last.WorkspaceID != "ws_123" {
		t.Fatalf("agent turn must carry the override workspace: %+v", last)
	}
	if last.ClientCWD == "" {
		t.Fatalf("ClientCWD should still ride the request: %+v", last)
	}

	// Status bar reflects the override, not the launch cwd.
	if line := stripANSI(m.statusLine()); !strings.Contains(line, "game:/mnt/d/wwwroot/ai/game") {
		t.Fatalf("status line should show the override workspace: %q", line)
	}
}

func TestWorkspaceSelectFailureSetsNoOverride(t *testing.T) {
	c := NewController("", "", nil, "")
	m := c.model
	c.SetMessageProcessor(func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		return api.MessageResponse{Content: "No workspace matching 9. Run /workspaces to see the list."}, 200
	})

	msg := m.handleWorkspaceSelect([]string{"9"})()
	if _, ok := msg.(MsgWorkspaceSwitched); ok {
		t.Fatalf("failure reply must not produce a switch: %+v", msg)
	}
	done, ok := msg.(MsgAgentDone)
	if !ok || !strings.Contains(done.Response, "No workspace matching") {
		t.Fatalf("failure reply should render as a plain control reply: %+v", msg)
	}
	if m.workspaceOverrideID != "" {
		t.Fatalf("failed switch must not pin an override: %q", m.workspaceOverrideID)
	}
	if req := m.runAgentRequestProbe(); req.WorkspaceID != "" {
		t.Fatalf("no-override session must keep WorkspaceID empty, got %q", req.WorkspaceID)
	}
}

// runAgentRequestProbe captures the MessageRequest runAgent would send now.
func (m *uiModel) runAgentRequestProbe() api.MessageRequest {
	var captured api.MessageRequest
	prev := m.messageProcessor
	m.messageProcessor = func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		captured = req
		return api.MessageResponse{Content: "ok"}, 200
	}
	_ = m.runAgent(context.Background(), "probe")()
	m.messageProcessor = prev
	return captured
}

func TestParseWorkspaceSwitchReply(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		ok    bool
		id    string
		wsN   string
		path  string
	}{
		{"canonical", "Current workspace: game (ws_123)\n/mnt/d/g", true, "ws_123", "game", "/mnt/d/g"},
		{"name with parens and spaces", "Current workspace: my proj (v2) (ws_9)\n/tmp/p", true, "ws_9", "my proj (v2)", "/tmp/p"},
		{"missing path line still pins id", "Current workspace: solo (ws_1)", true, "ws_1", "solo", ""},
		{"usage text", "Usage: /workspace <n|workspace_id>", false, "", "", ""},
		{"not found", "No workspace matching 9.", false, "", "", ""},
		{"empty id", "Current workspace: broken ()\n/tmp", false, "", "", ""},
		{"no closing paren", "Current workspace: broken (ws_1\n/tmp", false, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, name, path, ok := parseWorkspaceSwitchReply(tc.reply)
			if ok != tc.ok || id != tc.id || name != tc.wsN || path != tc.path {
				t.Fatalf("parse(%q) = %q %q %q %v, want %q %q %q %v", tc.reply, id, name, path, ok, tc.id, tc.wsN, tc.path, tc.ok)
			}
		})
	}
}
