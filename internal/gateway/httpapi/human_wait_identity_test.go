package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
)

// Each approved write is followed by a new model call. Unlike the original
// human-wait fixture, this crosses the resumed-call/new-call identity boundary.
func (p *humanWaitProvider) streamFollowupWrite(req llm.ChatRequest) <-chan llm.StreamEvent {
	out := make(chan llm.StreamEvent, 1)
	writes := 0
	for _, m := range req.Messages {
		if m.Role == "tool" && m.Name == "write_file" {
			if strings.Contains(m.Content, "Error executing") {
				out <- llm.StreamEvent{Content: "The follow-up was not executed."}
				close(out)
				return out
			}
			writes++
		}
	}
	switch writes {
	case 0:
		id := "first-write"
		if p.missingIDs {
			id = ""
		}
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: id, Function: "write_file", Args: `{"path":"receipt.txt","content":"approved effect"}`}}}
	case 1:
		id := "followup-write"
		if p.missingIDs {
			id = ""
		}
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: id, Function: "write_file", Args: `{"path":"next.txt","content":"distinct follow-up"}`}}}
	default:
		out <- llm.StreamEvent{Content: "Both approved operations completed."}
	}
	close(out)
	return out
}

func TestApprovalContinuationThenNewCallKeepsIdentityAcrossRestart(t *testing.T) {
	for _, missingIDs := range []bool{true, false} {
		for _, restart := range []bool{false, true} {
			name := "native"
			if missingIDs {
				name = "generated"
			}
			if restart {
				name += "_restart"
			}
			t.Run(name, func(t *testing.T) {
				daemon, store, provider, owner := newHumanWaitServer(t)
				provider.missingIDs, provider.followupWrite = missingIDs, true
				ws := humanWaitWorkspace(t, store, owner, "identity")
				startHumanWait(t, daemon, ws, "identity")
				first := waitParkedApproval(t, daemon, store, owner)
				if restart {
					daemon, _, provider, _ = newHumanWaitServerWithStore(t, store)
					provider.missingIDs, provider.followupWrite = missingIDs, true
				}
				ctx := context.Background()
				if _, err := daemon.respondApprovalByToken(ctx, owner, first.ID, "approved", "identity", control.ApprovalDecisionInput{}); err != nil {
					t.Fatal(err)
				}
				second := waitParkedApproval(t, daemon, store, owner)
				if second.ID == first.ID || second.RunID == first.RunID {
					t.Fatal("new action reused the prior approval or Run")
				}
				var firstCall, secondCall string
				for i, approval := range []*control.ApprovalRequest{first, second} {
					cp, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, approval.RunID)
					if err != nil || cp == nil {
						t.Fatalf("checkpoint: %+v %v", cp, err)
					}
					if i == 0 {
						firstCall = cp.WaitToolCallID
					} else {
						secondCall = cp.WaitToolCallID
					}
				}
				if firstCall == secondCall {
					t.Fatalf("distinct actions share %q", firstCall)
				}
				if _, err := os.Stat(filepath.Join(ws.LocalPath, "next.txt")); !os.IsNotExist(err) {
					t.Fatalf("unapproved follow-up ran: %v", err)
				}
				if _, err := daemon.respondApprovalByToken(ctx, owner, second.ID, "approved", "identity", control.ApprovalDecisionInput{}); err != nil {
					t.Fatal(err)
				}
				waitUntil(t, 2*time.Second, func() bool {
					data, err := os.ReadFile(filepath.Join(ws.LocalPath, "next.txt"))
					return err == nil && string(data) == "distinct follow-up" && daemon.coordinator().activeCount(owner.PersonID) == 0
				}, "follow-up did not finish")
				for path, want := range map[string]string{"receipt.txt": "approved effect", "next.txt": "distinct follow-up"} {
					data, err := os.ReadFile(filepath.Join(ws.LocalPath, path))
					if err != nil || string(data) != want {
						t.Fatalf("%s: %q %v", path, data, err)
					}
				}
				events, err := store.ListTaskEvents(ctx, first.TaskID, 200)
				if err != nil {
					t.Fatal(err)
				}
				invoked := make(map[string]int)
				for _, event := range events {
					if strings.Contains(string(event.Payload), "already claimed with different arguments") {
						t.Fatal("continuation collided with the restored action")
					}
					if event.Type == "tool.completed" {
						var result struct {
							Invoked bool   `json:"invoked"`
							CallID  string `json:"tool_call_id"`
						}
						if err := json.Unmarshal(event.Payload, &result); err != nil {
							t.Fatal(err)
						}
						if result.Invoked {
							invoked[result.CallID]++
						}
					}
				}
				if len(invoked) != 2 || invoked[firstCall] != 1 || invoked[secondCall] != 1 {
					t.Fatalf("approved operations did not execute exactly once: %+v", invoked)
				}
			})
		}
	}
}
