package cli

// TUI approval-mode omission: the client must NOT force-send its local
// default mode on every message — that explicit mode would forever shadow the
// person's persisted /mode preference on the gateway (the live mid-run /mode
// defect). A fresh session omits approval_mode entirely; only an explicit TUI
// /mode this session pins the mode on outgoing requests.

import (
	"context"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestClientRequestOmitsModeUntilExplicitlySet(t *testing.T) {
	c := NewController("", "", nil, "default")
	m := c.model
	if m.approvalMode != "" {
		t.Fatalf("fresh session must leave the approval mode unset, got %q", m.approvalMode)
	}

	var captured []api.MessageRequest
	c.SetMessageProcessor(func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		captured = append(captured, req)
		return api.MessageResponse{Content: "ok"}, 200
	})

	if msg := m.runAgent(context.Background(), "hello")(); msg == nil {
		t.Fatal("runAgent returned no message")
	}
	if len(captured) != 1 || captured[0].ApprovalMode != "" {
		t.Fatalf("request without a session /mode must omit approval_mode: %+v", captured)
	}

	// An explicit /mode this session pins the mode on later requests.
	m.handleMode([]string{"smart"})
	if m.approvalMode != "smart" {
		t.Fatalf("/mode smart should set the session mode, got %q", m.approvalMode)
	}
	if msg := m.runAgent(context.Background(), "again")(); msg == nil {
		t.Fatal("runAgent returned no message")
	}
	if len(captured) != 2 || captured[1].ApprovalMode != "smart" {
		t.Fatalf("explicit session mode must ride the request: %+v", captured)
	}
}

func TestClientSteeringCarriesOnlyExplicitSessionMode(t *testing.T) {
	for _, tc := range []struct{ name, explicit, saved, want string }{
		{"saved preference", "", "full-auto", ""},
		{"session override", "read-only", "full-auto", "read-only"},
		{"explicit smart", "smart", "read-only", "smart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewController("", "", nil, "default").model
			m.clientMode, m.thinking = true, true
			m.daemonRunID, m.channel = "run-owned", "session-owned"
			m.approvalMode, m.persistedApprovalMode = tc.explicit, tc.saved
			called := false
			m.steerFn = func(runID, channel, text, mode string) error {
				called = true
				if runID != "run-owned" || channel != "session-owned" || mode != tc.want {
					t.Fatalf("steering request: run=%q channel=%q mode=%q, want mode=%q", runID, channel, mode, tc.want)
				}
				return nil
			}
			m.injectMidRunGuidance("also check the changed path")
			if !called {
				t.Fatal("steering was not sent")
			}
		})
	}
}
