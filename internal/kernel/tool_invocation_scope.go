package kernel

import (
	"context"
	"strings"
)

type toolInvocationScopeContextKey struct{}

const (
	// SkillMutationDirect allows an explicitly user-authorized management
	// surface to update the active filesystem asset. It remains the
	// compatibility default while foreground ingress is migrated.
	SkillMutationDirect = "direct_active"
	// SkillMutationCandidateOnly is reserved for the durable curator. It does
	// not authorize rewriting the active skill.
	SkillMutationCandidateOnly = "candidate_only"
	// SkillMutationNone makes skill_manage read-only for this invocation.
	SkillMutationNone = "none"
)

// ToolInvocationScope separates durable asset ownership from execution
// authority. It contains identifiers only: credentials and environment values
// never cross this boundary.
type ToolInvocationScope struct {
	ControlTenantID   string
	PersonID          string
	TaskID            string
	WorkspaceID       string
	RunID             string
	LeaseID           string
	ExecutionScopeKey string
	WorkUnitID        string
	ExecutionLane     string
	AttachmentMode    string
	// SkillMutationMode is trusted dispatcher metadata. Model-supplied JSON
	// cannot populate the hidden invocation scope carried to tools.
	SkillMutationMode string
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
