package kernel

import (
	"context"
	"fmt"
)

// Project evidence is refreshed once per turn, before selected Run state in
// the system prompt. Reordering improves prefix reuse without caching stale
// instructions, changing message roles, or elevating repository authority.
func (a *Agent) projectPromptSections(ctx context.Context) []string {
	var out []string
	// 0.1 Inject project context files (.selfmind.md, AGENTS.md, etc.). Internal
	// background review is governed by its own role prompt and bounded data,
	// not by untrusted repository instructions.
	if a.contextScanner != nil && a.foregroundPromptProfile() {
		// Size the project-context budget to the live model window (may have
		// been changed by SetContextWindow). The project layer has its OWN
		// budget, independent of the person-memory layer below.
		if a.contextEngine != nil {
			a.contextScanner.SetContextWindowTokens(a.contextEngine.maxTokens)
		}
		var ctxFiles []ContextFile
		if workspace, ok := WorkspaceContextFromContext(ctx); ok {
			ctxFiles, _ = a.contextScanner.ScanRoots(workspace.ContextRoots())
		}
		if len(ctxFiles) > 0 {
			ctxPrompt := a.contextScanner.BuildContextPrompt(ctxFiles)
			if ctxPrompt != "" {
				out = append(out, ctxPrompt)
			}
		}
	}
	// Add a compact, deterministic build-system profile for coding work. This
	// is evidence derived from manifests and lockfiles only; prompt assembly
	// never executes repository code. It is limited to foreground and delegated
	// coding profiles; background review keeps its separate bounded role contract.
	if workspace, ok := WorkspaceContextFromContext(ctx); ok && a.workspacePromptProfile() {
		remainingProfiles := maxProfileProjects
		for _, root := range workspace.ContextRoots() {
			if remainingProfiles <= 0 {
				break
			}
			profile := DetectProjectProfile(root)
			if len(profile.Projects) > remainingProfiles {
				profile.Projects = profile.Projects[:remainingProfiles]
			}
			remainingProfiles -= len(profile.Projects)
			if profilePrompt := profile.Prompt(); profilePrompt != "" {
				profilePrompt = fmt.Sprintf("# BOUND PROJECT ROOT\nroot: %s\n\n%s", root, profilePrompt)
				out = append(out, profilePrompt)
			}
		}
	}

	return out
}
