package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"selfmind/internal/kernel/llm"
)

type toolActivationContextKey struct{}

// toolActivationState holds the deferred tools discovered in the CURRENT work
// unit. Within a unit the set only grows, so the provider tools block changes
// at most when new capability is discovered and never oscillates between
// iterations. Crossing into a new top-level work unit resets it, mirroring how
// an Active Skill expires at the same boundary — a capability discovered for
// one unit of work is not evidence for the next.
//
// The set is durable by reconstruction rather than by its own storage:
// activation is recorded in tool_search results, which the loop checkpoint
// replays verbatim, so seedToolActivationFromMessages rebuilds it after a park
// or a daemon restart.
type toolActivationState struct {
	mu sync.RWMutex
	// active is the set discovered in the current work unit.
	active map[string]struct{}
	// unitSequence is the in-progress top-level work-unit sequence the set
	// belongs to, taken from update_plan projections. It is tracked here rather
	// than reusing the Active Skill's sequence, which only exists once a Skill
	// has been selected — activation must reset per unit even with no Skill.
	unitSequence int
}

func withToolActivationState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := toolActivationStateFromContext(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, toolActivationContextKey{}, &toolActivationState{active: make(map[string]struct{})})
}

func toolActivationStateFromContext(ctx context.Context) (*toolActivationState, bool) {
	if ctx == nil {
		return nil, false
	}
	state, ok := ctx.Value(toolActivationContextKey{}).(*toolActivationState)
	return state, ok && state != nil
}

func deferredToolActive(ctx context.Context, name string) bool {
	state, ok := toolActivationStateFromContext(ctx)
	if !ok {
		return false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	_, ok = state.active[strings.TrimSpace(name)]
	return ok
}

func activateDeferredTools(ctx context.Context, names []string) []string {
	state, ok := toolActivationStateFromContext(ctx)
	if !ok {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	var added []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := state.active[name]; exists {
			continue
		}
		state.active[name] = struct{}{}
		added = append(added, name)
	}
	return added
}

func activatedToolCount(ctx context.Context) int {
	state, ok := toolActivationStateFromContext(ctx)
	if !ok {
		return 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return len(state.active)
}

// applyWorkUnitBoundary resets the activation set when a successful update_plan
// moves the run to a different top-level work unit. A capability discovered for
// one unit of work is not evidence for the next, and leaving the set in place
// carried activations across the boundary in the same change that lost them
// across a resume. Returns the number of entries cleared.
func applyWorkUnitBoundary(ctx context.Context, sequence int) int {
	if sequence <= 0 {
		return 0
	}
	state, ok := toolActivationStateFromContext(ctx)
	if !ok {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.unitSequence == sequence {
		return 0
	}
	cleared := 0
	if state.unitSequence != 0 {
		cleared = len(state.active)
		state.active = make(map[string]struct{})
	}
	state.unitSequence = sequence
	return cleared
}

// inProgressWorkUnitSequence reads the current top-level work-unit sequence out
// of an update_plan projection.
func inProgressWorkUnitSequence(toolName, raw string) int {
	if strings.TrimSpace(toolName) != "update_plan" {
		return 0
	}
	var projected struct {
		WorkUnits []struct {
			Sequence   int    `json:"sequence"`
			PlanStatus string `json:"plan_status"`
		} `json:"work_units"`
	}
	if json.Unmarshal([]byte(raw), &projected) != nil {
		return 0
	}
	for _, unit := range projected.WorkUnits {
		if unit.PlanStatus == "in_progress" {
			return unit.Sequence
		}
	}
	return 0
}

// activatedToolNamesFromSearchResult parses the activation names out of one
// tool_search result body.
func activatedToolNamesFromSearchResult(toolName, raw string) []string {
	if strings.TrimSpace(toolName) != "tool_search" {
		return nil
	}
	var results []struct {
		Name      string `json:"name"`
		Activated bool   `json:"activated"`
	}
	if json.Unmarshal([]byte(raw), &results) != nil {
		return nil
	}
	var names []string
	for _, result := range results {
		if result.Activated {
			names = append(names, result.Name)
		}
	}
	return names
}

func activateToolsFromSearchResult(ctx context.Context, toolName, raw string) []string {
	return activateDeferredTools(ctx, activatedToolNamesFromSearchResult(toolName, raw))
}

// seedToolActivationFromMessages rebuilds the activation set from a replayed
// tool ledger. Without it a run that discovered capabilities, parked on an
// approval, and resumed in a fresh RunConversation refused every tool it had
// already activated.
func seedToolActivationFromMessages(ctx context.Context, messages []llm.Message) int {
	var names []string
	sequence := 0
	for _, message := range messages {
		if message.Role != "tool" {
			continue
		}
		if unit := inProgressWorkUnitSequence(message.Name, message.Content); unit > 0 {
			// Replayed results are an ordered ledger. A later work-unit boundary
			// invalidates every activation recorded before it; accumulating all
			// historical tool_search results leaks capabilities across units.
			if sequence != unit {
				names = nil
				sequence = unit
			}
		}
		names = append(names, activatedToolNamesFromSearchResult(message.Name, message.Content)...)
	}
	if state, ok := toolActivationStateFromContext(ctx); ok {
		state.mu.Lock()
		state.active = make(map[string]struct{}, len(names))
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				state.active[name] = struct{}{}
			}
		}
		state.unitSequence = sequence
		restored := len(state.active)
		state.mu.Unlock()
		return restored
	}
	return 0
}

// toolExposureResolver is an optional AgentBackend capability: answer one
// tool's catalogue exposure without materializing every definition. Kernel must
// not import concrete tools, so this is a probe rather than a required method —
// cloned sub-agent dispatchers and test doubles that do not implement it keep
// the definition-scan fallback.
type toolExposureResolver interface {
	ResolveToolExposure(name string) (exposure string, known bool)
}

// resolveToolExposureForDispatch answers the dispatch-time availability
// question. The single-lookup path avoids running a JSON round trip per
// registered tool inside the streaming loop.
func (a *Agent) resolveToolExposureForDispatch(name string) (exposure string, known bool) {
	if resolver, ok := a.backend.(toolExposureResolver); ok {
		if value, found := resolver.ResolveToolExposure(name); found {
			if strings.TrimSpace(value) == "" {
				return "direct", true
			}
			return strings.ToLower(strings.TrimSpace(value)), true
		}
		return "", false
	}
	definitions := a.backend.GetToolDefinitions()
	for _, definition := range definitions {
		if toolDefinitionName(definition) == strings.TrimSpace(name) {
			return toolDefinitionExposure(definition), true
		}
	}
	return "", false
}

func toolDefinitionExposure(def map[string]interface{}) string {
	metadata, _ := def["selfmind"].(map[string]interface{})
	exposure, _ := metadata["exposure"].(string)
	if strings.TrimSpace(exposure) == "" {
		return "direct"
	}
	return strings.ToLower(strings.TrimSpace(exposure))
}

func toolDefinitionAvailable(ctx context.Context, def map[string]interface{}) bool {
	switch toolDefinitionExposure(def) {
	case "hidden":
		return false
	case "deferred":
		return deferredToolActive(ctx, toolDefinitionName(def))
	default:
		return true
	}
}
