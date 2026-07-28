package tools

import "testing"

func TestNormalizeApprovalMode(t *testing.T) {
	cases := map[string]ApprovalMode{
		"":           ApprovalOnRequest,
		"on-request": ApprovalOnRequest,
		"read-only":  ApprovalReadOnly,
		"readonly":   ApprovalReadOnly,
		"auto-edit":  ApprovalAutoEdit,
		"auto":       ApprovalAutoEdit,
		"full-auto":  ApprovalFullAuto,
		"yolo":       ApprovalFullAuto,
		"nonsense":   ApprovalOnRequest,
	}
	for in, want := range cases {
		if got := NormalizeApprovalMode(in); got != want {
			t.Errorf("NormalizeApprovalMode(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEffectiveApprovalModeUsesSmartOnlyForEmpty(t *testing.T) {
	if got := EffectiveApprovalMode(""); got != ApprovalSmart {
		t.Fatalf("empty effective mode = %q, want smart", got)
	}
	if got := EffectiveApprovalMode("nonsense"); got != ApprovalOnRequest {
		t.Fatalf("invalid effective mode = %q, want on-request", got)
	}
	if got := EffectiveApprovalMode("read-only"); got != ApprovalReadOnly {
		t.Fatalf("explicit effective mode = %q, want read-only", got)
	}
}

func TestApprovalNeeded(t *testing.T) {
	type row struct {
		mode      ApprovalMode
		tool      string
		dangerous bool
		want      bool
	}
	rows := []row{
		// full-auto never asks, even on dangerous.
		{ApprovalFullAuto, "terminal", true, false},
		{ApprovalFullAuto, "write_file", false, false},
		// read-only asks for any write or exec, plus dangerous reads.
		{ApprovalReadOnly, "write_file", false, true},
		{ApprovalReadOnly, "terminal", false, true},
		{ApprovalReadOnly, "read_file", false, false},
		{ApprovalReadOnly, "read_file", true, true},
		// auto-edit lets in-workspace edits through, asks for commands.
		{ApprovalAutoEdit, "write_file", false, false},
		{ApprovalAutoEdit, "patch", false, false},
		{ApprovalAutoEdit, "write_file", true, true}, // flagged edit (e.g. out of workspace)
		{ApprovalAutoEdit, "terminal", false, true},
		// on-request asks on dangerous OR any arbitrary-code exec tool (running
		// commands/code is inherently approval-worthy, not gated on the heuristic).
		{ApprovalOnRequest, "write_file", false, false},
		{ApprovalOnRequest, "terminal", false, true},
		{ApprovalOnRequest, "terminal", true, true},
		{ApprovalOnRequest, "execute_code", false, true},
		{ApprovalOnRequest, "shell", false, true},
		{ApprovalOnRequest, "read_file", false, false},
		// smart gates on exec tools too (then the LLM judge triages the ask).
		{ApprovalSmart, "terminal", false, true},
		{ApprovalSmart, "execute_code", false, true},
		{ApprovalSmart, "read_file", false, false},
		{ApprovalSmart, "write_file", true, true},
	}
	for _, r := range rows {
		if got := approvalNeeded(r.mode, r.tool, r.dangerous); got != r.want {
			t.Errorf("approvalNeeded(%s,%s,dangerous=%v)=%v want %v", r.mode, r.tool, r.dangerous, got, r.want)
		}
	}
}
