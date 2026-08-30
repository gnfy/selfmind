package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

const (
	SkillScopeUser      = "user"
	SkillScopeWorkspace = "workspace"
	SkillScopeExternal  = "external"
)

// Skill provenance is the authorship tier, independent of scope. Scope says who
// owns the location; provenance says whether SelfMind or this repository
// authored what lives there. External assets are untrusted data below operator,
// user, and safety policy, and are never rewritten automatically.
const (
	SkillProvenanceFirstParty = "first-party"
	SkillProvenanceExternal   = "external"

	// developerAgentOnlySkillMarker lets a repository keep Agent Skills for
	// coding assistants under .agents/skills without exposing those instructions
	// to SelfMind's product runtime. The marker is intentionally local to one
	// directory-form Skill; it never hides an entire root.
	developerAgentOnlySkillMarker = ".selfmind-developer-only"
)

// SkillRoot describes one directory that can contain skill packages.
type SkillRoot struct {
	Path       string
	Scope      string
	Source     string
	Provenance string
	Writable   bool
	Priority   int
}

// SkillStorage is the immutable filesystem root for user/control-tenant Skill
// assets and their adjacent learning audit. App wiring resolves it once from
// configuration and injects it into tool calls. Keeping it per dispatcher
// avoids process-global environment mutation when eval runtimes coexist with a
// real gateway in the same process.
type SkillStorage struct {
	baseDir string
}

const skillStorageArg = "_skill_storage"

func NewSkillStorage(baseDir string) (*SkillStorage, error) {
	baseDir = strings.TrimSpace(os.ExpandEnv(baseDir))
	if baseDir == "" {
		return nil, fmt.Errorf("skill storage base dir is required")
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve skill storage base dir: %w", err)
	}
	return &SkillStorage{baseDir: filepath.Clean(abs)}, nil
}

func (s *SkillStorage) BaseDir() string {
	if s == nil {
		return ""
	}
	return s.baseDir
}

// WithSkillStorage returns a shallow copy carrying an app-owned storage root.
// The opaque value is injected after model argument validation and cannot be
// manufactured by model JSON.
func WithSkillStorage(args map[string]interface{}, storage *SkillStorage) map[string]interface{} {
	out := make(map[string]interface{}, len(args)+1)
	for key, value := range args {
		out[key] = value
	}
	if storage != nil {
		out[skillStorageArg] = storage
	}
	return out
}

// SkillStorageMiddleware injects one immutable root into a dispatcher without
// mutating the caller's argument map or a process-global setting.
func SkillStorageMiddleware(storage *SkillStorage) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			return next(WithSkillStorage(args, storage))
		}
	}
}

func skillStorageFromInvocation(invocation ...map[string]interface{}) *SkillStorage {
	if len(invocation) == 0 || invocation[0] == nil {
		return nil
	}
	storage, _ := invocation[0][skillStorageArg].(*SkillStorage)
	return storage
}

func selfmindBaseDir(invocation ...map[string]interface{}) (string, error) {
	if storage := skillStorageFromInvocation(invocation...); storage != nil && storage.BaseDir() != "" {
		return storage.BaseDir(), nil
	}
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

// skillUserHomeDir resolves the person's home directory for the cross-vendor
// root. It is deliberately independent of the SelfMind asset base, which a
// storage override may relocate.
func skillUserHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func userSkillsDirForTenant(tenantID string, invocation ...map[string]interface{}) (string, error) {
	baseDir, err := selfmindBaseDir(invocation...)
	if err != nil {
		return "", err
	}
	return SkillsDirForTenant(baseDir, fallbackTenant(tenantID)), nil
}

func userTenantDirForTenant(tenantID string, invocation ...map[string]interface{}) (string, error) {
	skillsDir, err := userSkillsDirForTenant(tenantID, invocation...)
	if err != nil {
		return "", err
	}
	return filepath.Dir(skillsDir), nil
}

func managedWorkspaceSkillsDirForTenant(tenantID, workspaceID string, invocation ...map[string]interface{}) (string, error) {
	baseDir, err := selfmindBaseDir(invocation...)
	if err != nil {
		return "", err
	}
	return ManagedWorkspaceSkillsDir(baseDir, fallbackTenant(tenantID), workspaceID), nil
}

// SkillRootsForTenant returns all roots visible to a tenant, ordered by lookup
// priority. Repository-authored workspace roots win, the control-managed
// logical-workspace root follows them, and the user root remains visible as the
// cross-workspace fallback. Write selection is a separate trusted-scope step in
// ResolveWritableSkillRootForTenant.
func SkillRootsForTenant(tenantID string, invocation ...map[string]interface{}) ([]SkillRoot, error) {
	var roots []SkillRoot
	addExistingRoot := func(path, scope, source, provenance string, writable bool, priority int) {
		if strings.TrimSpace(path) == "" {
			return
		}
		clean := filepath.Clean(os.ExpandEnv(path))
		if st, err := os.Stat(clean); err == nil && st.IsDir() {
			roots = append(roots, SkillRoot{
				Path:       clean,
				Provenance: provenance,
				Scope:      scope,
				Source:     source,
				Writable:   writable,
				Priority:   priority,
			})
		}
	}

	workspaceStart := ""
	workspaceSkillsAllowed := true
	workspaceDirs := []string(nil)
	args := map[string]interface{}{"_tenant_id": tenantID}
	if len(invocation) > 0 && invocation[0] != nil {
		args = invocation[0]
	}
	if invocationScope, ok := InvocationScopeFromArgs(args); ok && strings.TrimSpace(invocationScope.WorkspaceID) != "" {
		managedRoot, err := managedWorkspaceSkillsDirForTenant(tenantID, invocationScope.WorkspaceID, invocation...)
		if err != nil {
			return nil, err
		}
		// Repository-authored workspace Skills keep precedence. The managed root
		// follows them but precedes external and user-global assets.
		roots = append(roots, SkillRoot{
			Path: managedRoot, Scope: SkillScopeWorkspace, Source: SkillSourceAgentCreated,
			Provenance: SkillProvenanceFirstParty, Writable: true, Priority: 35,
		})
	}
	if scope, ok := currentExecutionScopeAny(args); ok {
		workspaceStart = strings.TrimSpace(scope.WorkspaceRoot)
		workspaceSkillsAllowed = scope.TrustLevel != executionenv.TrustUntrusted
		// A typed run scope already identifies the logical workspace boundary.
		// Do not walk above it into ~/.selfmind/skills and relabel user assets as
		// workspace Skills. Ancestor discovery is only for direct local callers
		// whose cwd may be a subdirectory of the project.
		if workspaceStart != "" {
			workspaceDirs = []string{workspaceStart}
		}
	} else if cwd, err := os.Getwd(); err == nil {
		// Outside an active run (for example a local TUI slash command), the
		// caller's cwd is authoritative. During a daemon-owned run the
		// ExecutionScope above always wins, so daemon cwd can never select a
		// workspace skill root.
		workspaceStart = cwd
		workspaceDirs = skillRootAncestors(workspaceStart)
	}
	if workspaceSkillsAllowed && workspaceStart != "" {
		priority := 10
		for _, dir := range workspaceDirs {
			addExistingRoot(filepath.Join(dir, ".selfmind", "skills"), SkillScopeWorkspace, "workspace", SkillProvenanceFirstParty, true, priority)
			priority += 10
			addExistingRoot(filepath.Join(dir, ".agents", "skills"), SkillScopeWorkspace, "codex-compatible", SkillProvenanceFirstParty, false, priority)
			priority += 10
			addExistingRoot(filepath.Join(dir, "skills"), SkillScopeWorkspace, "workspace", SkillProvenanceFirstParty, false, priority)
			priority += 10
		}
	}
	for _, path := range splitSkillRootEnv(os.Getenv("SELFMIND_SKILLS_ROOTS")) {
		addExistingRoot(path, SkillScopeExternal, "env", SkillProvenanceExternal, false, 40)
	}
	if path := strings.TrimSpace(os.Getenv("SELFMIND_SKILLS_DIR")); path != "" {
		addExistingRoot(path, SkillScopeExternal, "env", SkillProvenanceExternal, true, 45)
	}

	userDir, err := userSkillsDirForTenant(tenantID, invocation...)
	if err != nil {
		return nil, err
	}
	roots = append(roots, SkillRoot{
		Path:       userDir,
		Scope:      SkillScopeUser,
		Source:     SkillSourceManual,
		Provenance: SkillProvenanceFirstParty,
		Writable:   true,
		Priority:   100,
	})
	// Cross-vendor Agent Skills convention. A Skill the person already keeps
	// for another agent works here with no further action, while the writable
	// user root above keeps precedence so an explicit install is never
	// shadowed by this default location.
	if home := skillUserHomeDir(); home != "" {
		addExistingRoot(filepath.Join(home, ".agents", "skills"), SkillScopeUser, "agents-compatible", SkillProvenanceExternal, false, 105)
	}
	// One-release compatibility window: skills previously written under the
	// person partition remain readable, but the control-tenant root wins on
	// name conflicts and all new writes continue to target the control tenant.
	if scope, ok := InvocationScopeFromArgs(args); ok {
		personID := strings.TrimSpace(scope.PersonID)
		if personID != "" && personID != fallbackTenant(tenantID) {
			if legacyDir, legacyErr := userSkillsDirForTenant(personID, invocation...); legacyErr == nil {
				addExistingRoot(legacyDir, SkillScopeUser, "legacy-person", SkillProvenanceFirstParty, false, 110)
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

// ResolveWritableSkillRootForTenant selects the write target without touching
// the filesystem. Read paths use this resolver and treat a missing directory
// as an empty store.
func ResolveWritableSkillRootForTenant(tenantID string, invocation ...map[string]interface{}) (SkillRoot, error) {
	roots, err := SkillRootsForTenant(tenantID, invocation...)
	if err != nil {
		return SkillRoot{}, err
	}
	args := map[string]interface{}{}
	if len(invocation) > 0 && invocation[0] != nil {
		args = invocation[0]
	}
	publicationScope := ""
	if scope, ok := InvocationScopeFromArgs(args); ok {
		publicationScope = strings.ToLower(strings.TrimSpace(scope.SkillPublicationScope))
		if publicationScope == kernel.SkillPublicationWorkspace && strings.TrimSpace(scope.WorkspaceID) == "" {
			return SkillRoot{}, fmt.Errorf("workspace Skill publication requires workspace identity")
		}
	}
	preferWorkspace := publicationScope == kernel.SkillPublicationWorkspace ||
		strings.EqualFold(os.Getenv("SELFMIND_SKILLS_WRITE_SCOPE"), SkillScopeWorkspace)
	if preferWorkspace {
		for _, root := range roots {
			if root.Writable && root.Scope == SkillScopeWorkspace &&
				(publicationScope != kernel.SkillPublicationWorkspace || root.Source == SkillSourceAgentCreated) {
				return root, nil
			}
		}
		if publicationScope == kernel.SkillPublicationWorkspace {
			return SkillRoot{}, fmt.Errorf("managed workspace skill root is unavailable")
		}
	}
	for _, root := range roots {
		if root.Writable && root.Scope == SkillScopeUser {
			return root, nil
		}
	}
	for _, root := range roots {
		if root.Writable {
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

func isDeveloperAgentOnlySkill(path string) bool {
	st, err := os.Stat(filepath.Join(path, developerAgentOnlySkillMarker))
	return err == nil && !st.IsDir()
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
