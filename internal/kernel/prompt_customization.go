package kernel

import (
	"strings"

	"selfmind/internal/promptassets"
)

type AgentPromptProfile string

const (
	PromptProfileForeground       AgentPromptProfile = "foreground"
	PromptProfileBackgroundReview AgentPromptProfile = "background_review"
	PromptProfileDelegation       AgentPromptProfile = "delegation"
)

func (a *Agent) SetPromptSnapshot(snapshot *promptassets.Snapshot) {
	if a != nil {
		a.promptSnapshot = snapshot
		if a.contextEngine != nil {
			a.contextEngine.SetPromptSnapshot(snapshot)
		}
	}
}

func (a *Agent) SetPromptProfile(profile AgentPromptProfile) {
	if a == nil {
		return
	}
	if profile == PromptProfileBackgroundReview {
		a.promptProfile = profile
		return
	}
	if profile == PromptProfileDelegation {
		a.promptProfile = profile
		return
	}
	a.promptProfile = PromptProfileForeground
}

func (a *Agent) composeForegroundPrompt(base string, sections ...string) string {
	if a == nil || a.promptSnapshot == nil || !a.foregroundPromptProfile() {
		return base
	}
	allowed := make([]string, 0, len(sections))
	for _, section := range sections {
		// Delegated agents keep their specialized role identity and do not emit
		// user-facing progress or durable-learning behavior. They inherit only
		// applicable execution-quality and UI guidance from agent.md.
		if a.promptProfile == PromptProfileDelegation {
			switch section {
			case promptassets.SectionPersona, promptassets.SectionProgressUpdates, promptassets.SectionLearningPreferences:
				continue
			}
		}
		allowed = append(allowed, section)
	}
	return a.promptSnapshot.Compose(promptassets.FileAgent, base, allowed...)
}

func (a *Agent) foregroundPromptProfile() bool {
	return a == nil || a.promptProfile != PromptProfileBackgroundReview
}

func (a *Agent) primaryForegroundPromptProfile() bool {
	return a == nil || a.promptProfile == "" || a.promptProfile == PromptProfileForeground
}

func (a *Agent) workspacePromptProfile() bool {
	return a == nil || a.promptProfile != PromptProfileBackgroundReview
}

// ForegroundPromptDefaults exposes only the static/operator-facing guidance
// layers for `selfmind prompt show agent`. It never renders runtime memory,
// project instructions, tool schemas, or task context.
func ForegroundPromptDefaults(soul string) string {
	persona := strings.TrimSpace(soul)
	if persona == "" {
		persona = "(none; no legacy agent.soul persona is configured. Use agent.md / Persona for personal preferences.)"
	}
	parts := []string{
		"## Persona\n\n" + persona,
		"## Response and Interaction Floor (locked)\n\n" + foregroundDeliveryGuidance(),
		"## Work Quality and Verification Floor (locked)\n\n" + taskExecutionGuidance(),
		"## Workspace Implementation Quality (locked, conditional)\n\n" + workspaceImplementationGuidance(),
		"## Progress Updates\n\n" + progressNarrationGuidance(),
		"## Frontend and UI\n\n" + conditionalUserFacingInterfaceGuidance(userFacingInterfaceQualityGuidance()),
		"## Persistent Learning Floor (locked)\n\n" + selfImprovementGuidance(),
		"## Additional Runtime Layers (locked)\n\nTool-use contract, task strategy, sandbox boundary, selected runtime context, memory/Skill wrappers, project-context trust boundary, and registered tool schemas are assembled by SelfMind and are not editable through the prompt workspace.",
	}
	return strings.Join(parts, "\n\n")
}

func BackgroundReviewPromptDefaults() string {
	return backgroundReviewSoul(true, false) + "\n\n" + buildBackgroundReviewPrompt(nil, true, false)
}

func SummarizerPromptDefaults() string {
	return buildSummaryPrompt("<existing summary when present>", "<bounded conversation turns>")
}
