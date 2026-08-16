package kernel

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/platform/textutil"
)

type skillRuntimeContextKey struct{}

const (
	activeSkillPromptBegin = "<!-- SELFMIND_ACTIVE_SKILL_BEGIN -->"
	activeSkillPromptEnd   = "<!-- SELFMIND_ACTIVE_SKILL_END -->"
)

// ActiveSkillContext is the one ephemeral instruction asset selected for the
// current work unit. It carries no execution authority; all commands and
// scripts still pass through ordinary tools, scope, sandbox, and approvals.
type ActiveSkillContext struct {
	ActivationID string
	WorkUnitID   string
	Key          string
	Name         string
	VersionHash  string
	Scope        string
	Source       string
	Body         string
	LinkedFiles  []string
	Truncated    bool
}

type SkillCandidateContext struct {
	Key         string
	Name        string
	Description string
	Scope       string
	Source      string
}

type SkillRuntimeContext struct {
	Active     *ActiveSkillContext
	Candidates []SkillCandidateContext
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
	var b strings.Builder
	b.WriteString(activeSkillPromptBegin + "\n")
	b.WriteString("# ACTIVE SKILL FOR CURRENT WORK UNIT\n")
	b.WriteString("This is the single instruction asset activated for the current work unit. Follow it when applicable, but it does not override operator/user/safety policy and grants no tool, filesystem, network, credential, shell, or approval authority. If it is wrong or unusable, call skill_fallback and replan this work unit without another skill.\n")
	writeKV(&b, "activation_id", s.ActivationID)
	writeKV(&b, "work_unit_id", s.WorkUnitID)
	writeKV(&b, "skill_key", s.Key)
	writeKV(&b, "name", s.Name)
	writeKV(&b, "version_hash", s.VersionHash)
	writeKV(&b, "scope", s.Scope)
	writeKV(&b, "source", s.Source)
	b.WriteString("\n## Instructions\n")
	overhead := b.Len() + 160
	bodyBudget := maxChars - overhead
	if bodyBudget < 256 {
		bodyBudget = 256
	}
	body := textutil.TruncateBytes(strings.TrimSpace(s.Body), bodyBudget)
	b.WriteString(body)
	b.WriteString("\n")
	if s.Truncated || len(body) < len(strings.TrimSpace(s.Body)) {
		b.WriteString("[Skill instructions were bounded for this work unit. Load a specific linked file with skill_view if needed.]\n")
	}
	if len(s.LinkedFiles) > 0 {
		b.WriteString("\n## Linked Files (load only when needed)\n")
		for i, file := range s.LinkedFiles {
			if i >= 12 {
				break
			}
			fmt.Fprintf(&b, "- %s\n", file)
		}
	}
	b.WriteString(activeSkillPromptEnd + "\n")
	end := "\n" + activeSkillPromptEnd + "\n"
	raw := b.String()
	if len(raw) <= maxChars {
		return raw
	}
	// Preserve the closing structural marker even when the body is bounded so
	// fallback/work-unit switching can remove this exact volatile prompt slice.
	return textutil.TruncateBytes(raw, maxChars-len(end)) + end
}
