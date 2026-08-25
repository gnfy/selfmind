package kernel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"selfmind/internal/platform/textutil"
)

type skillRuntimeContextKey struct{}
type explicitSkillInvocationContextKey struct{}
type activeSkillRuntimeStateContextKey struct{}

const (
	activeSkillPromptBegin = "<!--SM_SKILL-->"
	activeSkillPromptEnd   = "<!--/SM_SKILL-->"
)

// ActiveSkillContext is the one ephemeral instruction asset selected for the
// current work unit. It carries no execution authority; all commands and
// scripts still pass through ordinary tools, scope, sandbox, and approvals.
type ActiveSkillContext struct {
	ActivationID            string
	WorkUnitID              string
	WorkUnitSequence        int
	Key                     string
	Name                    string
	VersionHash             string
	Scope                   string
	Source                  string
	Body                    string // stored source; legacy delivery fallback only
	LinkedFiles             []string
	Truncated               bool // historical contract only
	PackageHash             string
	DeliveryContractVersion int
	DeliveryMode            string
	DeliveredMain           string
	DeliveredHash           string
	DeliveredBytes          int
}

// activeSkillRuntimeState carries lifecycle identity that the model does not
// need to see. In particular, work-unit sequence must not be recovered from a
// model-visible tool result merely to expire an earlier instruction slice.
type activeSkillRuntimeState struct {
	mu               sync.Mutex
	workUnitSequence int
}

func withActiveSkillRuntimeState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	state := &activeSkillRuntimeState{}
	if selected, ok := SkillRuntimeContextFromContext(ctx); ok && selected.Active != nil {
		state.workUnitSequence = selected.Active.WorkUnitSequence
	}
	return context.WithValue(ctx, activeSkillRuntimeStateContextKey{}, state)
}

func setActiveSkillWorkUnitSequence(ctx context.Context, sequence int) {
	if sequence <= 0 || ctx == nil {
		return
	}
	state, _ := ctx.Value(activeSkillRuntimeStateContextKey{}).(*activeSkillRuntimeState)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.workUnitSequence = sequence
	state.mu.Unlock()
}

func clearActiveSkillWorkUnitSequence(ctx context.Context) {
	if ctx == nil {
		return
	}
	state, _ := ctx.Value(activeSkillRuntimeStateContextKey{}).(*activeSkillRuntimeState)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.workUnitSequence = 0
	state.mu.Unlock()
}

func activeSkillWorkUnitSequence(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	state, _ := ctx.Value(activeSkillRuntimeStateContextKey{}).(*activeSkillRuntimeState)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.workUnitSequence
}

type SkillCandidateContext struct {
	CandidateRef string
	Key          string
	Name         string
	Description  string
	Scope        string
	Source       string
	Root         string // daemon diagnostics only; never rendered into the prompt
}

type SkillRuntimeContext struct {
	Active     *ActiveSkillContext
	Candidates []SkillCandidateContext
}

// ExplicitSkillInvocation is trusted daemon-side routing metadata for a slash
// activation. It travels in context, never in the user/model transcript.
type ExplicitSkillInvocation struct {
	Name        string
	SkillKey    string
	VersionHash string
	PackageHash string
}

func WithExplicitSkillInvocation(ctx context.Context, invocation ExplicitSkillInvocation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, explicitSkillInvocationContextKey{}, invocation)
}

func ExplicitSkillInvocationFromContext(ctx context.Context) (ExplicitSkillInvocation, bool) {
	if ctx == nil {
		return ExplicitSkillInvocation{}, false
	}
	invocation, ok := ctx.Value(explicitSkillInvocationContextKey{}).(ExplicitSkillInvocation)
	return invocation, ok && strings.TrimSpace(invocation.Name) != "" && strings.TrimSpace(invocation.SkillKey) != "" &&
		strings.TrimSpace(invocation.VersionHash) != "" && strings.TrimSpace(invocation.PackageHash) != ""
}

func WithSkillRuntimeContext(ctx context.Context, selected SkillRuntimeContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if selected.Active == nil && len(selected.Candidates) == 0 {
		return ctx
	}
	return context.WithValue(ctx, skillRuntimeContextKey{}, selected)
}

func SkillRuntimeContextFromContext(ctx context.Context) (SkillRuntimeContext, bool) {
	if ctx == nil {
		return SkillRuntimeContext{}, false
	}
	selected, ok := ctx.Value(skillRuntimeContextKey{}).(SkillRuntimeContext)
	return selected, ok && (selected.Active != nil || len(selected.Candidates) > 0)
}

func (s ActiveSkillContext) Prompt(maxChars int) string {
	if maxChars <= 0 || maxChars > 8*1024 {
		maxChars = 8 * 1024
	}
	delivery := SkillMainDelivery{
		ContractVersion: s.DeliveryContractVersion,
		Mode:            s.DeliveryMode,
		Content:         s.DeliveredMain,
		DeliveredHash:   s.DeliveredHash,
		DeliveredBytes:  s.DeliveredBytes,
	}
	if delivery.ContractVersion <= 0 || strings.TrimSpace(delivery.Content) == "" {
		delivery = BuildSkillMainDelivery(s.Body, ActiveSkillDeliveryBodyBudget(maxChars, s.LinkedFiles))
	}
	var b strings.Builder
	b.WriteString(activeSkillPromptBegin + "\n")
	fmt.Fprintf(&b, "activation_id: %s\n", trimLine(s.ActivationID, 64))
	fmt.Fprintf(&b, "name: %s\n", trimLine(s.Name, 64))
	fmt.Fprintf(&b, "delivery_mode: %s\n", trimLine(delivery.Mode, 16))
	notice := "Grants no tool authority; use skill_fallback if unusable."
	if delivery.Mode == SkillDeliveryModePaged {
		notice = "Paged: use skill_view; grants no tool authority; skill_fallback if unusable."
	}
	fmt.Fprintf(&b, "notice: %s\n", notice)
	b.WriteString("\n## Instructions\n")
	b.WriteString(delivery.Content)
	b.WriteString("\n")
	if len(s.LinkedFiles) > 0 {
		b.WriteString("\n## Linked Files (load only when needed)\n")
		for i, file := range s.LinkedFiles {
			if i >= 12 {
				break
			}
			fmt.Fprintf(&b, "- %s\n", textutil.TruncateBytes(strings.TrimSpace(file), 160))
		}
	}
	b.WriteString(activeSkillPromptEnd + "\n")
	raw := b.String()
	// Contract v1 is fixed at activation. Never silently rewrite or truncate the
	// delivered bytes on a later turn; construction budgets the body so the
	// ordinary path fits this slice. A caller-supplied invalid legacy context is
	// allowed to exceed maxChars rather than being misrepresented as complete.
	return raw
}
