package cli

import (
	"strings"
	"testing"
)

func exploreRead(path string) ChatMessage {
	return ChatMessage{Role: "tool", ToolName: "read_file", ToolArgs: `{"path":"` + path + `"}`, Content: "ok", Duration: 0.1}
}

func TestRenderExploreGroupAllReadsCommaJoined(t *testing.T) {
	out := stripANSI(renderExploreGroup([]ChatMessage{
		exploreRead("README.md"), exploreRead("AGENTS.md"), exploreRead("go.mod"),
	}, 70))
	if !strings.Contains(out, "• Explored") {
		t.Fatalf("missing Explored header: %q", out)
	}
	if !strings.Contains(out, "└ Read README.md, AGENTS.md, go.mod") {
		t.Fatalf("reads should be comma-joined on one Read line: %q", out)
	}
}

func TestRenderExploreGroupMixedVerbsPerLine(t *testing.T) {
	out := stripANSI(renderExploreGroup([]ChatMessage{
		exploreRead("README.md"),
		{Role: "tool", ToolName: "list_files", ToolArgs: `{"path":".github"}`, Content: "ok", Duration: 0.1},
	}, 70))
	if !strings.Contains(out, "Read README.md") || !strings.Contains(out, "List .github") {
		t.Fatalf("mixed group should list each verb: %q", out)
	}
}

func TestRenderExploreGroupRunningHeader(t *testing.T) {
	out := stripANSI(renderExploreGroup([]ChatMessage{
		{Role: "tool", ToolName: "read_file", ToolArgs: `{"path":"main.go"}`, IsRunning: true},
	}, 70))
	if !strings.Contains(out, "◦ Exploring") {
		t.Fatalf("running group should show ◦ Exploring: %q", out)
	}
}

func TestIsExploreCell(t *testing.T) {
	if !isExploreCell(ChatMessage{Role: "tool", ToolName: "read_file"}) {
		t.Fatal("read_file should be an explore cell")
	}
	if isExploreCell(ChatMessage{Role: "tool", ToolName: "write_file"}) {
		t.Fatal("write_file must NOT group as explore")
	}
	if isExploreCell(ChatMessage{Role: "tool", ToolName: "terminal"}) {
		t.Fatal("terminal must NOT group as explore")
	}
	if isExploreCell(ChatMessage{Role: "assistant", ToolName: "read_file"}) {
		t.Fatal("only tool-role messages group")
	}
}
