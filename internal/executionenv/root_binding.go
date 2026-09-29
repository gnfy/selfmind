package executionenv

import "strings"

const (
	RootRolePrimary    = "primary"
	RootRoleAdditional = "additional"
	RootRoleAttachment = "attachment"
	RootRoleView       = "execution_view"

	RootAccessRead  = "read"
	RootAccessWrite = "write"

	RootSourceWorkspace     = "workspace"
	RootSourceCLIAddDir     = "cli_add_dir"
	RootSourceAttachment    = "attachment"
	RootSourceExecutionView = "execution_view"
)

// RootBinding is one physical directory bound to a run. Workspace roots and
// CLI --add-dir roots may both be writable, but that is only an upper bound:
// the run's approval mode, sandbox policy, and safety floor still decide what
// a concrete tool call may do. Source is durable so recovery can reproduce a
// run without turning a historical task or transcript into filesystem
// authority.
type RootBinding struct {
	Path        string `json:"path"`
	Role        string `json:"role"`
	AccessCap   string `json:"access_cap"`
	Source      string `json:"source"`
	ContextRoot bool   `json:"context_root,omitempty"`
	// GitBaseline is captured before a write-capable Run changes a clean
	// checkout. It is not authority to bypass the physical-root lock.
	GitBaseline *GitBaseline `json:"git_baseline,omitempty"`
}

func (b RootBinding) Writable() bool {
	return strings.EqualFold(strings.TrimSpace(b.AccessCap), RootAccessWrite)
}

func CloneRootBindings(in []RootBinding) []RootBinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]RootBinding, len(in))
	copy(out, in)
	for i := range out {
		if in[i].GitBaseline != nil {
			baseline := *in[i].GitBaseline
			out[i].GitBaseline = &baseline
		}
	}
	return out
}

func EqualRootBindings(a, b []RootBinding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		if left.Path != right.Path || left.Role != right.Role || left.AccessCap != right.AccessCap || left.Source != right.Source || left.ContextRoot != right.ContextRoot {
			return false
		}
		if (left.GitBaseline == nil) != (right.GitBaseline == nil) {
			return false
		}
		if left.GitBaseline != nil && *left.GitBaseline != *right.GitBaseline {
			return false
		}
	}
	return true
}

func RootPaths(bindings []RootBinding) []string {
	seen := make(map[string]struct{}, len(bindings))
	out := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		path := strings.TrimSpace(binding.Path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func ContextRootPaths(bindings []RootBinding) []string {
	seen := make(map[string]struct{}, len(bindings))
	out := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		path := strings.TrimSpace(binding.Path)
		if path == "" || !binding.ContextRoot {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}
