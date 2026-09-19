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
	// to SelfMind's product runtime. The marker is local to one directory-form
	// Skill; it never hides an entire root.
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

// SkillStorageMiddleware injects one immutable root for this invocation. Keep
// the invocation envelope shared so execution facts written by the tool reach
// outer evidence and recovery middleware. Restore only this injected key.
func SkillStorageMiddleware(storage *SkillStorage) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			if storage == nil {
				return next(args)
			}
			if args == nil {
				args = map[string]interface{}{}
			}
			previous, existed := args[skillStorageArg]
			args[skillStorageArg] = storage
			defer func() {
				if existed {
					args[skillStorageArg] = previous
				} else {
					delete(args, skillStorageArg)
				}
			}()
			return next(args)
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
			workspaceDirs = skillRootSearchDirs(workspaceStart)
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
			// The other cross-vendor convention. Repositories commonly keep one
			// real directory and symlink the other, so a package found through
			// both is deduplicated by resolved path, not by root.
			addExistingRoot(filepath.Join(dir, ".claude", "skills"), SkillScopeWorkspace, "claude-compatible", SkillProvenanceFirstParty, false, priority)
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

// skillRootSearchDirs returns the directories under a workspace root that may
// carry their own Skill directories: the root itself plus a bounded descent.
//
// A workspace is often a container of several repositories, each with its own
// `.agents/skills`; a workspace that IS one repository is the depth-0 case of
// the same rule. Expressing both as one bounded descent avoids inferring a
// "layout mode", which would be a scenario branch in generic discovery code.
//
// The descent never follows symlinks and never enters a skill container
// directory: the children of `.agents/skills` are packages, not further search
// roots.
func skillRootSearchDirs(start string) []string {
	start = filepath.Clean(start)
	dirs := []string{start}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth >= skillRootSearchMaxDepth || len(dirs) >= skillRootSearchMaxDirs {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			// entry.IsDir() is false for a symlink, which is what we want here:
			// a linked directory is reachable through its real parent.
			if !entry.IsDir() {
				continue
			}
			// Dot directories are never descended. The convention directories
			// are themselves dot directories and are reached by name, not by
			// descent, so this costs nothing and removes the need for a skip
			// list that would have to keep pace with every tool's cache dir.
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if _, skip := skillRootSearchSkipDirs[entry.Name()]; skip {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if len(dirs) >= skillRootSearchMaxDirs {
				return
			}
			child := filepath.Join(dir, name)
			dirs = append(dirs, child)
			walk(child, depth+1)
		}
	}
	walk(start, 0)
	return dirs
}

// skillRootSearchMaxDepth covers `<workspace>/<repo>/<area>` — deep enough for a
// repository that files its Skills under a subdirectory, shallow enough that
// discovery stays a bounded read.
const (
	skillRootSearchMaxDepth = 3
	skillRootSearchMaxDirs  = 512
)

// skillRootSearchSkipDirs are the non-dot directories never descended into:
// dependency and build trees that cannot own a repository's Skills, plus the
// plain `skills` convention directory, whose children are packages rather than
// search roots. Dot directories are excluded by rule, not by name.
var skillRootSearchSkipDirs = map[string]struct{}{
	"node_modules": {}, "dist": {}, "build": {}, "vendor": {}, "target": {},
	"venv": {}, "__pycache__": {}, "coverage": {}, "skills": {},
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

// skillConventionDirs are the per-repository directories a Skill package can be
// published under. They are siblings of one repository directory, and the same
// Skill commonly exists under two of them: a canonical body under one and a
// thin compatibility entrypoint for another coding agent under the other.
var skillConventionDirs = [][]string{
	{".selfmind", "skills"}, {".agents", "skills"}, {".claude", "skills"}, {"skills"},
}

// isDeveloperAgentOnlySkill reports whether the repository that owns this Skill
// has withheld it from the product runtime.
//
// The marker is a statement about a Skill NAME made by its repository, not
// about one directory. Only the canonical body usually carries it, so checking
// the discovered directory alone exposes the compatibility entrypoint — and
// through it the very instructions the marker withheld. That gap only became
// reachable when discovery began enumerating a second convention, which is why
// the check is answered per repository rather than per directory.
func isDeveloperAgentOnlySkill(path string) bool {
	if developerOnlyMarkerPresent(path) {
		return true
	}
	base, name, ok := skillConventionBase(path)
	if !ok {
		return false
	}
	for _, convention := range skillConventionDirs {
		parts := append([]string{base}, convention...)
		sibling := filepath.Join(append(parts, name)...)
		if sibling == filepath.Clean(path) {
			continue
		}
		if developerOnlyMarkerPresent(sibling) {
			return true
		}
	}
	return false
}

func developerOnlyMarkerPresent(path string) bool {
	st, err := os.Stat(filepath.Join(path, developerAgentOnlySkillMarker))
	return err == nil && !st.IsDir()
}

// skillConventionBase splits a package path into the repository directory that
// owns it and the package name, or reports that the path is not published under
// a known convention (an external or managed root, which has no siblings).
func skillConventionBase(path string) (string, string, bool) {
	clean := filepath.Clean(path)
	name := filepath.Base(clean)
	if name == "." || name == string(filepath.Separator) {
		return "", "", false
	}
	container := filepath.Dir(clean)
	for _, convention := range skillConventionDirs {
		base := container
		matched := true
		for i := len(convention) - 1; i >= 0; i-- {
			if filepath.Base(base) != convention[i] {
				matched = false
				break
			}
			base = filepath.Dir(base)
		}
		if matched {
			return base, name, true
		}
	}
	return "", "", false
}

// firstInvocationArgs returns the invocation map a variadic Skill helper was
// called with, so a guard can read the trusted dispatcher metadata on it.
func firstInvocationArgs(invocation []map[string]interface{}) map[string]interface{} {
	if len(invocation) > 0 && invocation[0] != nil {
		return invocation[0]
	}
	return nil
}

func ensureWritableSkill(info SkillInfo, action string) error {
	if info.Writable {
		return nil
	}
	return fmt.Errorf("skill %q is from a read-only %s root (%s); copy it to a writable skill root before %s", info.Name, emptyDefault(info.Scope, "unknown"), info.Path, action)
}

// ensureSkillEditAuthorized guards rewriting a Skill's own body.
//
// Root writability answers "may this runtime write here on its own", which is
// the right question for automatic curation and the wrong one for a person
// applying a reviewed change to their own repository's Skill. The escape hatch
// the read-only error suggests — copy it to a writable root — produces a second
// Skill answering the same bare name, which `matchSkillsByName` then refuses as
// ambiguous: the workaround breaks the Skill it was meant to preserve.
//
// A pin still refuses: it is the person saying this content is not to be
// rewritten. So does every other mutation (delete, archive, support files):
// those are not needed to apply a repair and are not opened here.
func ensureSkillEditAuthorized(info SkillInfo, action string, args map[string]interface{}) error {
	if info.Writable {
		return nil
	}
	if info.Pinned {
		return fmt.Errorf("skill %q is pinned; unpin it before %s", info.Name, action)
	}
	if scope, ok := InvocationScopeFromArgs(args); ok && strings.TrimSpace(scope.SkillMutationMode) == kernel.SkillMutationDirect {
		return nil
	}
	return fmt.Errorf("skill %q lives in %s and is yours to change; apply the reviewed version with /skills promote instead of %s", info.Name, info.Path, action)
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
