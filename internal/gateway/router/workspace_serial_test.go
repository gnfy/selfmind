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
		{"no workspace", ctxWith(false, kernel.ToolModeFull, true), "unscoped"},
		{"write turn serializes", ctxWith(true, kernel.ToolModeLocalWrite, true), "wsA"},
		{"full turn serializes", ctxWith(true, kernel.ToolModeFull, true), "wsA"},
		{"read hint keeps open tools serialized", ctxWith(true, kernel.ToolModeLocalRead, true), "wsA"},
		{"no-tools turn runs concurrent", ctxWith(true, kernel.ToolModeNone, true), ""},
		{"web hint keeps open tools serialized", ctxWith(true, kernel.ToolModeWeb, true), "wsA"},
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

func TestWorkspaceSerialPathsIncludesEveryBoundContextRoot(t *testing.T) {
	ctx := kernel.WithWorkspaceContext(context.Background(), kernel.WorkspaceContext{
		ID: "wsA", Root: "/tmp/wsA", Roots: []string{"/tmp/wsA", "/tmp/shared"},
	})
	ctx = kernel.WithTaskStrategy(ctx, kernel.TaskStrategy{ToolMode: kernel.ToolModeLocalWrite})
	got := workspaceSerialPaths(ctx)
	if len(got) != 2 || got[0] != "/tmp/wsA" || got[1] != "/tmp/shared" {
		t.Fatalf("serial paths = %#v", got)
	}

	readCtx := kernel.WithTaskStrategy(ctx, kernel.TaskStrategy{ToolMode: kernel.ToolModeNone})
	if got := workspaceSerialPaths(readCtx); len(got) != 0 {
		t.Fatalf("read-only serial paths = %#v", got)
	}
}
