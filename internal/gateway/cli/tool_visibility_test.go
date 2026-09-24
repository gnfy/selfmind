package cli

import (
	"strings"
	"testing"
)

func TestToolMessagesExplainLifecycleAndBatchActions(t *testing.T) {
	tests := []struct {
		name string
		msg  ChatMessage
		want []string
		drop string
	}{
		{
			name: "resume work",
			msg: ChatMessage{Role: "tool", ToolName: "work_select",
				ToolArgs: `{"action":"resume","run_id":"run_f0a430bf-82a8-4063-a8f2-3aa18205cd9d"}`,
				Content:  "This turn now continues the selected run."},
			want: []string{"Continued run run_f0a430bf…", "continues the selected run", "work_select"},
		},
		{
			name: "select skill",
			msg: ChatMessage{Role: "tool", ToolName: "skill_select",
				ToolArgs: `{"candidate_ref":"opaque","reason":"release workflow applies"}`,
				Content:  "aws-codebuild-release · activated"},
			want: []string{"Selected Skill aws-codebuild-release", "activated", "reason: release workflow applies", "skill_select"},
			drop: "└ completed",
		},
		{
			name: "read skill section",
			msg: ChatMessage{Role: "tool", ToolName: "skill_view",
				ToolArgs: `{"name":"aws-codebuild-release","section":"Safety boundaries"}`,
				Content:  "aws-codebuild-release · section Safety boundaries · 1400/1400 bytes · complete"},
			want: []string{"Read Skill aws-codebuild-release", "section Safety boundaries", "1400/1400 bytes", "skill_view"},
			drop: "└ completed",
		},
		{
			name: "batch read",
			msg: ChatMessage{Role: "tool", ToolName: "batch_read",
				ToolArgs: `{"operations":[{"tool":"read_file","path":"plan.json"},{"tool":"ls_r","path":".release"}]}`,
				Content:  "2 operation(s) · all succeeded"},
			want: []string{"Completed 2/2 reads", "batch_read"},
			drop: "read_file plan.json",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := stripANSI(renderToolMessage(test.msg, 120))
			for _, want := range test.want {
				if !strings.Contains(out, want) {
					t.Fatalf("rendered tool message %q missing %q", out, want)
				}
			}
			if test.drop != "" && strings.Contains(out, test.drop) {
				t.Fatalf("rendered tool message kept opaque result %q: %q", test.drop, out)
			}
		})
	}
}

func TestToolMessagesStepBelowNarrationAndEvidenceStepsBelowAction(t *testing.T) {
	out := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "read_file",
		ToolArgs: `{"path":"config.yml"}`,
		Content:  "12 lines · 300 B",
	}, 80))
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("tool hierarchy lines = %q", lines)
	}
	if !strings.HasPrefix(lines[0], "  • Read config.yml") {
		t.Fatalf("tool action must occupy the first indented level: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "    └ ") || !strings.Contains(lines[1], "lines") {
		t.Fatalf("tool evidence must occupy the second indented level: %q", lines[1])
	}
}

func TestBatchReadCompletionDoesNotRepeatChildTargets(t *testing.T) {
	out := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "batch_read",
		ToolArgs: `{"operations":[{"tool":"read_file","path":"one.md"},{"tool":"read_file","path":"two.md"}]}`,
		Content:  "2 operation(s) · all succeeded",
	}, 100))
	if !strings.Contains(out, "Completed 2/2 reads") {
		t.Fatalf("batch completion must expose the aggregate: %q", out)
	}
	if strings.Contains(out, "one.md") || strings.Contains(out, "two.md") || strings.Contains(out, "operation(s)") {
		t.Fatalf("batch completion repeated child evidence: %q", out)
	}
}

func TestToolMessageSuppressesOpaqueCompletionAndShowsTypedFailure(t *testing.T) {
	completed := stripANSI(renderToolMessage(ChatMessage{
		Role: "tool", ToolName: "skill_view", ToolArgs: `{"name":"release","section":"Checks"}`, Content: "completed",
	}, 100))
	if strings.Contains(completed, "└ completed") || !strings.Contains(completed, "section Checks") {
		t.Fatalf("opaque completion should fall back to typed evidence: %q", completed)
	}

	failed := stripANSI(renderToolMessage(ChatMessage{
		Role: "tool", ToolName: "terminal", ToolArgs: `{"command":"release preflight"}`,
		Content: "FAIL target: connection refused before dispatch\nNo external effect observed.", IsError: true, Duration: 1.2,
	}, 100))
	for _, want := range []string{"Failed release preflight", "terminal", "connection refused", "No external effect observed"} {
		if !strings.Contains(failed, want) {
			t.Fatalf("failed tool message %q missing %q", failed, want)
		}
	}
}
