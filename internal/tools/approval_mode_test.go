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
		// on-request only asks on dangerous.
		{ApprovalOnRequest, "write_file", false, false},
		{ApprovalOnRequest, "terminal", false, false},
		{ApprovalOnRequest, "terminal", true, true},
	}
	for _, r := range rows {
		if got := approvalNeeded(r.mode, r.tool, r.dangerous); got != r.want {
			t.Errorf("approvalNeeded(%s,%s,dangerous=%v)=%v want %v", r.mode, r.tool, r.dangerous, got, r.want)
		}
	}
}
