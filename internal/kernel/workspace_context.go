package kernel

import (
	"context"
	"strings"
)

type workspaceContextKey struct{}

type WorkspaceContext struct {
	ID    string
	Root  string
	Roots []string
}

func WithWorkspaceContext(ctx context.Context, workspace WorkspaceContext) context.Context {
	workspace.Root = strings.TrimSpace(workspace.Root)
	workspace.ID = strings.TrimSpace(workspace.ID)
	if workspace.Root == "" {
		return ctx
	}
	seen := map[string]struct{}{workspace.Root: {}}
	roots := []string{workspace.Root}
	for _, root := range workspace.Roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	workspace.Roots = roots
	return context.WithValue(ctx, workspaceContextKey{}, workspace)
}

func WorkspaceContextFromContext(ctx context.Context) (WorkspaceContext, bool) {
	if ctx == nil {
		return WorkspaceContext{}, false
	}
	workspace, ok := ctx.Value(workspaceContextKey{}).(WorkspaceContext)
	return workspace, ok && strings.TrimSpace(workspace.Root) != ""
}

func (workspace WorkspaceContext) ContextRoots() []string {
	if len(workspace.Roots) > 0 {
		return append([]string{}, workspace.Roots...)
	}
	if strings.TrimSpace(workspace.Root) == "" {
		return nil
	}
	return []string{workspace.Root}
}
