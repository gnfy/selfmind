package kernel

import (
	"context"
	"strings"
)

type workspaceContextKey struct{}

type WorkspaceContext struct {
	ID   string
	Root string
}

func WithWorkspaceContext(ctx context.Context, workspace WorkspaceContext) context.Context {
	workspace.Root = strings.TrimSpace(workspace.Root)
	workspace.ID = strings.TrimSpace(workspace.ID)
	if workspace.Root == "" {
		return ctx
	}
	return context.WithValue(ctx, workspaceContextKey{}, workspace)
}

func WorkspaceContextFromContext(ctx context.Context) (WorkspaceContext, bool) {
	if ctx == nil {
		return WorkspaceContext{}, false
	}
	workspace, ok := ctx.Value(workspaceContextKey{}).(WorkspaceContext)
	return workspace, ok && strings.TrimSpace(workspace.Root) != ""
}
