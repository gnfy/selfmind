package tools

import (
	"sort"
	"strings"
	"sync"
)

// skillToolPrefix is the namespace of the hidden per-Skill compatibility
// tools. The Skill loader keeps every Skill address inside it, and nothing
// else registers a name there.
const skillToolPrefix = "skill:"

// SkillToolManager keeps the hidden per-Skill compatibility tools identical
// in every attached dispatcher. Agent workers share one, as they share the
// MCP manager: a Skill change made through any worker, or through management
// dispatch on the primary, reloads the Skill tools of all of them at once.
type SkillToolManager struct {
	mu          sync.RWMutex
	dispatchers []*Dispatcher
}

// NewSkillToolManager attaches every dispatcher to one manager. Each
// dispatcher loaded its own Skill tools when it was built; all of them now
// take the first dispatcher's, so the pool starts identical even if a Skill
// changed on disk while the workers were being built. A dispatcher made later
// with Subset, such as a delegated sub-agent's, is not attached: a reload
// through it changes only that subset, never the workers' dispatchers.
func NewSkillToolManager(dispatchers ...*Dispatcher) *SkillToolManager {
	m := &SkillToolManager{dispatchers: append([]*Dispatcher(nil), dispatchers...)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.dispatchers) == 0 {
		return m
	}
	shared := m.dispatchers[0].registry.skillTools()
	for _, dispatcher := range m.dispatchers {
		dispatcher.registry.replaceSkillTools(shared)
		dispatcher.registry.attachSkillToolManager(m)
	}
	return m
}

// Reload reads the compatibility tool of every enabled Skill once and
// installs that one set in every dispatcher. Reloads are serialized, so a
// slower reload can never install an older set over a newer one.
func (m *SkillToolManager) Reload(tenantID string, invocation ...map[string]interface{}) ([]SkillDefinition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defs, tools, err := loadSkillTools(tenantID, invocation...)
	for _, dispatcher := range m.dispatchers {
		dispatcher.registry.replaceSkillTools(tools)
	}
	return defs, err
}

// HoldCatalog runs fn while no Skill tool reload can be applied, so fn sees
// the same Skill tools in every dispatcher. fn must not reload Skill tools.
func (m *SkillToolManager) HoldCatalog(fn func()) {
	if m == nil {
		fn()
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	fn()
}

func (r *Registry) attachSkillToolManager(m *SkillToolManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skillToolManager = m
}

func (r *Registry) sharedSkillToolManager() *SkillToolManager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skillToolManager
}

// skillTools returns the registry's Skill tools in name order.
func (r *Registry) skillTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name := range r.tools {
		if strings.HasPrefix(name, skillToolPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, r.tools[name])
	}
	return tools
}

// replaceSkillTools swaps the registry's Skill tools for tools in one step,
// so a catalogue read sees the old set or the new one, never a registry
// between the two. A later tool replaces an earlier one of the same name, as
// registering them in order would.
func (r *Registry) replaceSkillTools(tools []Tool) {
	compiled := make([]compiledToolSchema, len(tools))
	for i, tool := range tools {
		compiled[i] = compileToolSchema(tool)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureMaps()
	for name := range r.tools {
		if strings.HasPrefix(name, skillToolPrefix) {
			r.generation++
			r.generations[name] = r.generation
			delete(r.tools, name)
			delete(r.schemas, name)
		}
	}
	for i, tool := range tools {
		r.generation++
		r.tools[tool.Name()] = tool
		r.schemas[tool.Name()] = compiled[i]
		r.generations[tool.Name()] = r.generation
	}
}
