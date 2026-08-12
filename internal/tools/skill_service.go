package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"selfmind/internal/executionenv"
)

const (
	SkillScopeUser      = "user"
	SkillScopeWorkspace = "workspace"
	SkillScopeExternal  = "external"
)

// SkillRoot describes one directory that can contain skill packages.
type SkillRoot struct {
	Path     string
	Scope    string
	Source   string
	Writable bool
	Priority int
}

func selfmindBaseDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get home dir: %w", err)
		}
	}
	return filepath.Join(home, ".selfmind"), nil
}

func userSkillsDirForTenant(tenantID string) (string, error) {
	baseDir, err := selfmindBaseDir()
	if err != nil {
		return "", err
	}
	return SkillsDirForTenant(baseDir, fallbackTenant(tenantID)), nil
}

func userTenantDirForTenant(tenantID string) (string, error) {
	skillsDir, err := userSkillsDirForTenant(tenantID)
	if err != nil {
		return "", err
	}
	return filepath.Dir(skillsDir), nil
}

// SkillRootsForTenant returns all roots visible to a tenant, ordered by lookup
// priority. Workspace roots are read first; the user root is always available
// and remains the default write target.
func SkillRootsForTenant(tenantID string, invocation ...map[string]interface{}) ([]SkillRoot, error) {
	var roots []SkillRoot
	addExistingRoot := func(path, scope, source string, writable bool, priority int) {
		if strings.TrimSpace(path) == "" {
			return
		}
		clean := filepath.Clean(os.ExpandEnv(path))
		if st, err := os.Stat(clean); err == nil && st.IsDir() {
			roots = append(roots, SkillRoot{
				Path:     clean,
				Scope:    scope,
				Source:   source,
				Writable: writable,
				Priority: priority,
			})
		}
	}

	workspaceStart := ""
	workspaceSkillsAllowed := true
	args := map[string]interface{}{"_tenant_id": tenantID}
	if len(invocation) > 0 && invocation[0] != nil {
		args = invocation[0]
	}
	if scope, ok := currentExecutionScopeAny(args); ok {
		workspaceStart = strings.TrimSpace(scope.WorkspaceRoot)
		workspaceSkillsAllowed = scope.TrustLevel != executionenv.TrustUntrusted
	} else if cwd, err := os.Getwd(); err == nil {
		// Outside an active run (for example a local TUI slash command), the
		// caller's cwd is authoritative. During a daemon-owned run the
		// ExecutionScope above always wins, so daemon cwd can never select a
		// workspace skill root.
		workspaceStart = cwd
	}
	if workspaceSkillsAllowed && workspaceStart != "" {
		priority := 10
		for _, dir := range skillRootAncestors(workspaceStart) {
			addExistingRoot(filepath.Join(dir, ".selfmind", "skills"), SkillScopeWorkspace, "workspace", true, priority)
			priority += 10
			addExistingRoot(filepath.Join(dir, ".agents", "skills"), SkillScopeWorkspace, "codex-compatible", false, priority)
			priority += 10
			addExistingRoot(filepath.Join(dir, "skills"), SkillScopeWorkspace, "workspace", false, priority)
			priority += 10
		}
	}
	for _, path := range splitSkillRootEnv(os.Getenv("SELFMIND_SKILLS_ROOTS")) {
		addExistingRoot(path, SkillScopeExternal, "env", false, 40)
	}
	if path := strings.TrimSpace(os.Getenv("SELFMIND_SKILLS_DIR")); path != "" {
		addExistingRoot(path, SkillScopeExternal, "env", true, 45)
	}

	userDir, err := userSkillsDirForTenant(tenantID)
	if err != nil {
		return nil, err
	}
	roots = append(roots, SkillRoot{
		Path:     userDir,
		Scope:    SkillScopeUser,
		Source:   SkillSourceManual,
		Writable: true,
		Priority: 100,
	})
	// One-release compatibility window: skills previously written under the
	// person partition remain readable, but the control-tenant root wins on
	// name conflicts and all new writes continue to target the control tenant.
	if scope, ok := InvocationScopeFromArgs(args); ok {
		personID := strings.TrimSpace(scope.PersonID)
		if personID != "" && personID != fallbackTenant(tenantID) {
			if legacyDir, legacyErr := userSkillsDirForTenant(personID); legacyErr == nil {
				addExistingRoot(legacyDir, SkillScopeUser, "legacy-person", false, 110)
			}
		}
	}

	return dedupeSkillRoots(roots), nil
}

func activeSkillWorkspaceUntrusted(tenantID string, invocation ...map[string]interface{}) bool {
	args := map[string]interface{}{"_tenant_id": tenantID}
	if len(invocation) > 0 && invocation[0] != nil {
		args = invocation[0]
	}
	scope, ok := currentExecutionScopeAny(args)
	return ok && scope.TrustLevel == executionenv.TrustUntrusted
}

func skillRootAncestors(start string) []string {
	start = filepath.Clean(start)
	var dirs []string
	for len(dirs) < 8 {
		dirs = append(dirs, start)
		parent := filepath.Dir(start)
		if parent == start {
			break
		}
		start = parent
	}
	return dirs
}

func WritableSkillRootForTenant(tenantID string, invocation ...map[string]interface{}) (SkillRoot, error) {
	roots, err := SkillRootsForTenant(tenantID, invocation...)
	if err != nil {
		return SkillRoot{}, err
	}
	preferWorkspace := strings.EqualFold(os.Getenv("SELFMIND_SKILLS_WRITE_SCOPE"), SkillScopeWorkspace)
	if preferWorkspace {
		for _, root := range roots {
			if root.Writable && root.Scope == SkillScopeWorkspace {
				if err := os.MkdirAll(root.Path, 0755); err != nil {
					return SkillRoot{}, fmt.Errorf("create writable skills dir: %w", err)
				}
				return root, nil
			}
		}
	}
	for _, root := range roots {
		if root.Writable && root.Scope == SkillScopeUser {
			if err := os.MkdirAll(root.Path, 0755); err != nil {
				return SkillRoot{}, fmt.Errorf("create writable skills dir: %w", err)
			}
			return root, nil
		}
	}
	for _, root := range roots {
		if root.Writable {
			if err := os.MkdirAll(root.Path, 0755); err != nil {
				return SkillRoot{}, fmt.Errorf("create writable skills dir: %w", err)
			}
			return root, nil
		}
	}
	return SkillRoot{}, fmt.Errorf("no writable skill root configured")
}

func splitSkillRootEnv(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := filepath.SplitList(raw)
	if len(fields) <= 1 {
		fields = strings.Split(raw, ",")
	}
	var out []string
	for _, field := range fields {
		if v := strings.TrimSpace(field); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func dedupeSkillRoots(roots []SkillRoot) []SkillRoot {
	sort.SliceStable(roots, func(i, j int) bool {
		return roots[i].Priority < roots[j].Priority
	})
	seen := map[string]bool{}
	var out []SkillRoot
	for _, root := range roots {
		abs, err := filepath.Abs(root.Path)
		if err != nil {
			abs = root.Path
		}
		key := strings.ToLower(filepath.Clean(abs))
		if seen[key] {
			continue
		}
		seen[key] = true
		root.Path = filepath.Clean(abs)
		out = append(out, root)
	}
	return out
}

func ensureWritableSkill(info SkillInfo, action string) error {
	if info.Writable {
		return nil
	}
	return fmt.Errorf("skill %q is from a read-only %s root (%s); copy it to a writable skill root before %s", info.Name, emptyDefault(info.Scope, "unknown"), info.Path, action)
}

func truncateMetadata(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 20 {
		for max > 0 && !utf8.ValidString(s[:max]) {
			max--
		}
		return s[:max]
	}
	for max > 16 && !utf8.ValidString(s[:max-16]) {
		max--
	}
	return s[:max-16] + "...(truncated)"
}
