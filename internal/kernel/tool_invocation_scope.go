package kernel

import (
	"context"
	"strings"
)

type toolInvocationScopeContextKey struct{}

// ToolInvocationScope separates durable asset ownership from execution
// authority. It contains identifiers only: credentials and environment values
// never cross this boundary.
type ToolInvocationScope struct {
	ControlTenantID   string
	PersonID          string
	WorkspaceID       string
	RunID             string
	LeaseID           string
	ExecutionScopeKey string
}

func WithToolInvocationScope(ctx context.Context, scope ToolInvocationScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(scope.ControlTenantID) == "" && strings.TrimSpace(scope.PersonID) == "" &&
		strings.TrimSpace(scope.RunID) == "" && strings.TrimSpace(scope.LeaseID) == "" {
		return ctx
	}
	return context.WithValue(ctx, toolInvocationScopeContextKey{}, scope)
}

func ToolInvocationScopeFromContext(ctx context.Context) (ToolInvocationScope, bool) {
	if ctx == nil {
		return ToolInvocationScope{}, false
	}
	scope, ok := ctx.Value(toolInvocationScopeContextKey{}).(ToolInvocationScope)
	if !ok {
		return ToolInvocationScope{}, false
	}
	return scope, strings.TrimSpace(scope.ControlTenantID) != "" || strings.TrimSpace(scope.PersonID) != "" ||
		strings.TrimSpace(scope.RunID) != "" || strings.TrimSpace(scope.LeaseID) != ""
}
