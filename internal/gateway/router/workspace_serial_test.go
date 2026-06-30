package router

import (
	"context"
	"testing"

	"selfmind/internal/kernel"
)

func ctxWith(ws bool, mode kernel.ToolMode, hasStrategy bool) context.Context {
	ctx := context.Background()
	if ws {
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{ID: "wsA", Root: "/tmp/wsA"})
	}
	if hasStrategy {
		ctx = kernel.WithTaskStrategy(ctx, kernel.TaskStrategy{ToolMode: mode})
	}
	return ctx
}

func TestWorkspaceSerialKeyWriteVsRead(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no workspace", ctxWith(false, kernel.ToolModeFull, true), ""},
		{"write turn serializes", ctxWith(true, kernel.ToolModeLocalWrite, true), "wsA"},
		{"full turn serializes", ctxWith(true, kernel.ToolModeFull, true), "wsA"},
		{"read-only turn runs concurrent", ctxWith(true, kernel.ToolModeLocalRead, true), ""},
		{"no-tools turn runs concurrent", ctxWith(true, kernel.ToolModeNone, true), ""},
		{"web-only turn runs concurrent", ctxWith(true, kernel.ToolModeWeb, true), ""},
		// No strategy pinned: conservatively serialize (an agent turn could write).
		{"unknown strategy serializes", ctxWith(true, "", false), "wsA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceSerialKey(tc.ctx); got != tc.want {
				t.Fatalf("workspaceSerialKey = %q, want %q", got, tc.want)
			}
		})
	}
}
