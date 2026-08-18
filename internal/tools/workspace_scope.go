package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const ExecutionProfileWatchFinalization = "watch-finalization"

// ExecutionScope describes the workspace boundary for one active person/tenant.
// The daemon installs a scope before running an agent turn so file and shell
// tools default to the task workspace and cannot wander into another user's
// project directory.
type ExecutionScope struct {
	TenantID      string
	PersonID      string
	WorkspaceID   string
	WorkspaceRoot string
	AllowedRoots  []string
	TaskID        string
	RunID         string
	Channel       string
	TrustLevel    string
	LeaseID       string
	// EnvironmentSnapshotID and EnvironmentGeneration mirror the lease's
	// environment binding so a tool call can resolve its child environment
	// without a control-plane lookup.
	EnvironmentSnapshotID  string
	EnvironmentGeneration  int64
	EnvironmentFingerprint string
	PrincipalFingerprint   string
	CredentialSourceHash   string
	// SandboxPolicy carries the execution policy WITH THE REQUEST. The policy
	// used to exist only as process-wide state installed at startup, which meant
	// one daemon could only ever have one policy — wrong the moment execution
	// serves several workspaces with different trust levels. Nil keeps the
	// process default.
	SandboxPolicy *ExecSandboxPolicy
	Capabilities  []string
	// ExecutionProfile is an internal contract for system-originated work. It
	// must never be populated from an external request.
	ExecutionProfile string
	Approval         ToolApprovalHandler
	// Clarify lets gateway/IM execution contexts answer the clarify tool
	// without a blocking interactive prompt (which only the local TUI has).
	Clarify ClarifyHandler
	// ApprovalMode is the codex-style approval policy for this turn (read-only /
	// auto-edit / full-auto / smart / on-request). Empty means on-request.
	// When ModeGetter is set it is only the run-start snapshot/fallback.
	ApprovalMode ApprovalMode
	// ModeGetter, when set, is consulted at EACH approval decision instead of
	// the static ApprovalMode snapshot, so a /mode change from any endpoint
	// (e.g. IM `/mode smart` while a CLI run is executing) takes effect on the
	// in-flight run's NEXT ask. The gateway installs a closure that re-resolves
	// with run-start precedence: an explicit per-request mode still wins, else
	// the person's CURRENT persisted /mode preference, else on-request. Nil (or
	// an empty result) falls back to ApprovalMode.
	ModeGetter func() ApprovalMode
	// Grants backs class-level approval memory: the approval middleware consults
	// it to skip a human ask for an already-approved class and records durable
	// task/person grants. Run-scoped grants use the in-memory set below.
	Grants ApprovalGrantStore
	// CapabilityStore backs time-bounded execution capabilities such as
	// network:shared. Grants are scoped to this person and workspace and never
	// contain command text or credential bytes.
	CapabilityStore ExecutionCapabilityStore
	// ResumeAuthorizations atomically consumes a one-shot authorization created
	// when an answerable parked approval is approved. It is checked only after
	// the hard floor and current explicit-deny policy.
	ResumeAuthorizations ApprovalResumeAuthorizationStore
	// Judge backs the smart-mode LLM triage step (H2): when set, a dangerous
	// (non-hardline) op that survived the class-grant check is triaged by a
	// cheap model before the human ask (APPROVE auto-runs + grants the class for
	// the task, DENY blocks, ESCALATE/errors fall through to the human ask). Nil
	// = no triage, so smart mode behaves as on-request (human ask). The
	// gateway/app installs a judge backed by a cheap role model, kept OFF the
	// run's main provider.
	Judge ApprovalJudge
	// TriageIntent supplies the person's own words for this run so the judge can
	// rule on AUTHORIZATION, not only on risk: the same command is a different
	// decision when the person asked for it. The gateway installs it (it owns the
	// work spine); it must return bounded, redacted text and is treated as
	// untrusted data by the triage prompt. Nil means authorization is unknown.
	TriageIntent func() string
	// IntentSnapshot is the source-aware replacement for TriageIntent. Keeping
	// both lets older embedders install a bounded string while the daemon supplies
	// structured evidence. When present, this field wins.
	IntentSnapshot func() RunIntentSnapshot
	// runGrants remembers an explicit human approval only for this live run.
	// It is intentionally in-memory and dies with SetExecutionScope cleanup:
	// useful for repeated verification calls without minting durable authority.
	runGrants *runApprovalGrantSet
}

type runApprovalGrantSet struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

func newRunApprovalGrantSet() *runApprovalGrantSet {
	return &runApprovalGrantSet{keys: make(map[string]struct{})}
}

func (s *runApprovalGrantSet) has(key string) bool {
	if s == nil || strings.TrimSpace(key) == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.keys[key]
	s.mu.RUnlock()
	return ok
}

func (s *runApprovalGrantSet) add(key string) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.mu.Lock()
	s.keys[key] = struct{}{}
	s.mu.Unlock()
}

type ExecutionCapabilityStore interface {
	HasExecutionCapability(ctx context.Context, tenantID, personID, workspaceID, capability, resourceFingerprint string) (bool, error)
	GrantExecutionCapability(ctx context.Context, tenantID, personID, workspaceID, capability, resourceFingerprint, grantedBy string, expiresAt time.Time) error
}

// executionScopes is keyed by scope key. Historically the only key was the
// person id (passed as "tenantID" because the agent's storage tenant IS the
// person), which silently assumed one active execution per person. A run-scoped
// key is also registered so a caller that knows its run resolves exactly its own
// scope — the shape a separate execution node needs, where one process serves
// many runs.
var executionScopes sync.Map // scope key -> ExecutionScope

type scopeKeyContextKey struct{}

// WithExecutionScopeKey tags a context with the run-scoped key its tool calls
// should resolve. The gateway installs it alongside the workspace context.
func WithExecutionScopeKey(ctx context.Context, key string) context.Context {
	key = strings.TrimSpace(key)
	if ctx == nil || key == "" {
		return ctx
	}
	return context.WithValue(ctx, scopeKeyContextKey{}, key)
}

func executionScopeKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if key, ok := ctx.Value(scopeKeyContextKey{}).(string); ok {
		return key
	}
	return ""
}

type ExecutionScopeDiagnostic struct {
	Installed        bool
	WorkspaceID      string
	TrustLevel       string
	LeaseID          string
	ExecutionProfile string
	Capabilities     []string
}

// ExecutionScopeDiagnostics returns only non-secret execution metadata for
// /diag. It never exposes commands, credential refs, environment names, or
// approval payloads.
func ExecutionScopeDiagnostics(personID string) ExecutionScopeDiagnostic {
	value, ok := executionScopes.Load(strings.TrimSpace(personID))
	if !ok {
		return ExecutionScopeDiagnostic{}
	}
	scope, ok := value.(ExecutionScope)
	if !ok {
		return ExecutionScopeDiagnostic{}
	}
	return ExecutionScopeDiagnostic{
		Installed:        true,
		WorkspaceID:      scope.WorkspaceID,
		TrustLevel:       scope.TrustLevel,
		LeaseID:          scope.LeaseID,
		ExecutionProfile: scope.ExecutionProfile,
		Capabilities:     append([]string{}, scope.Capabilities...),
	}
}

// SetExecutionScope installs scope under the person key and, when the scope
// carries a run id, under a run-scoped key as well. The returned cleanup removes
// both.
func SetExecutionScope(personKey string, scope ExecutionScope) func() {
	personKey = strings.TrimSpace(personKey)
	runKey := ExecutionScopeKeyForRun(scope.RunID)
	if runKey != "" && scope.runGrants == nil {
		scope.runGrants = newRunApprovalGrantSet()
	}
	if personKey == "" && runKey == "" {
		return func() {}
	}
	if personKey != "" {
		executionScopes.Store(personKey, scope)
	}
	if runKey != "" {
		executionScopes.Store(runKey, scope)
	}
	return func() {
		if personKey != "" {
			executionScopes.Delete(personKey)
		}
		if runKey != "" {
			executionScopes.Delete(runKey)
		}
	}
}

// ExecutionScopeKeyForRun builds the run-scoped key. Empty for a scope with no
// run (a local CLI helper), which then resolves by person as before.
func ExecutionScopeKeyForRun(runID string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	return "run:" + runID
}

func currentExecutionScope(args map[string]interface{}) (ExecutionScope, bool) {
	scope, ok := currentExecutionScopeAny(args)
	return scope, ok && strings.TrimSpace(scope.WorkspaceRoot) != ""
}

func currentExecutionScopeAny(args map[string]interface{}) (ExecutionScope, bool) {
	// Prefer the run-scoped key: it identifies exactly one execution even when
	// the process serves several.
	if key := executionScopeKeyFromContext(contextFromArgs(args)); key != "" {
		if value, ok := executionScopes.Load(key); ok {
			if scope, ok := value.(ExecutionScope); ok {
				return scope, true
			}
		}
	}
	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		return ExecutionScope{}, false
	}
	value, ok := executionScopes.Load(tenantID)
	if !ok {
		return ExecutionScope{}, false
	}
	scope, ok := value.(ExecutionScope)
	return scope, ok
}

// WorkspaceScopeMiddleware normalizes path/cwd arguments into the active
// workspace and rejects attempts to escape its allowed roots.
func WorkspaceScopeMiddleware() Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			scope, ok := currentExecutionScope(args)
			if !ok {
				return next(args)
			}

			toolName, _ := args["_tool_name"].(string)
			switch toolName {
			case "terminal", "verify", "watch_external":
				cwd, _ := args["cwd"].(string)
				scoped, err := resolveScopedPath(scope, cwd)
				if err != nil {
					return "", err
				}
				args["cwd"] = scoped
			case "read_file", "write_file", "search_files", "ls_r":
				rawPath, _ := args["path"].(string)
				scoped, err := resolveScopedPath(scope, rawPath)
				if err != nil {
					return "", err
				}
				args["path"] = scoped
			case "vision_analyze":
				// The tool's local branch is a filesystem read and must obey
				// the scope like read_file (it used to os.ReadFile any path,
				// bypassing AllowedRoots entirely). http(s) URLs stay with the
				// tool's own SSRF/egress handling.
				rawURL, _ := args["image_url"].(string)
				if isLocalImageRef(rawURL) {
					scoped, err := resolveScopedPath(scope, strings.TrimPrefix(strings.TrimSpace(rawURL), "file://"))
					if err != nil {
						return "", err
					}
					args["image_url"] = scoped
				}
			case "patch":
				patchContent, _ := args["patch"].(string)
				scoped, err := scopePatchContent(scope, patchContent)
				if err != nil {
					return "", err
				}
				args["patch"] = scoped
			}

			return next(args)
		}
	}
}

// isLocalImageRef mirrors vision_analyze's own local-vs-remote split: anything
// without a URL scheme (or with file://) is a local filesystem read; http(s)
// URLs are remote fetches.
func isLocalImageRef(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if strings.HasPrefix(raw, "file://") {
		return true
	}
	return !strings.Contains(raw, "://")
}

func resolveScopedPath(scope ExecutionScope, raw string) (string, error) {
	root, err := filepath.Abs(scope.WorkspaceRoot)
	if err != nil {
		return "", fmt.Errorf("workspace root is invalid: %w", err)
	}
	root = filepath.Clean(root)

	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "." {
		raw = root
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(root, clean)
	}
	clean, err = filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("path is invalid: %w", err)
	}
	clean = filepath.Clean(clean)

	if !scopeAllowsPath(scope, clean) {
		// The "path ... escapes workspace allowed roots" prefix is stable:
		// failure classifiers match it via strings.Contains("escapes
		// workspace"), so guidance may only be APPENDED. The trailing hint
		// names the actual way out — the scope follows the bound workspace,
		// not the terminal's cwd, and without it users read this error as
		// "resume didn't work" (observed live).
		return "", fmt.Errorf("path %s escapes workspace allowed roots. Use /resume <task> to work in that task's workspace, or /workspace <n> to switch", clean)
	}
	return clean, nil
}

func scopeAllowsPath(scope ExecutionScope, target string) bool {
	roots := scope.AllowedRoots
	if len(roots) == 0 {
		roots = []string{scope.WorkspaceRoot}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if isWithin(filepath.Clean(absRoot), filepath.Clean(target)) {
			return true
		}
	}
	return false
}

func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func scopePatchContent(scope ExecutionScope, patchContent string) (string, error) {
	if strings.TrimSpace(patchContent) == "" {
		return patchContent, nil
	}
	lines := strings.Split(patchContent, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
			scoped, err := resolveScopedPath(scope, path)
			if err != nil {
				return "", err
			}
			lines[i] = "*** Update File: " + scoped
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			scoped, err := resolveScopedPath(scope, path)
			if err != nil {
				return "", err
			}
			lines[i] = "*** Add File: " + scoped
		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			scoped, err := resolveScopedPath(scope, path)
			if err != nil {
				return "", err
			}
			lines[i] = "*** Delete File: " + scoped
		case strings.HasPrefix(line, "*** Move File: "):
			spec := strings.TrimSpace(strings.TrimPrefix(line, "*** Move File: "))
			from, to, ok := strings.Cut(spec, " -> ")
			if !ok {
				return "", fmt.Errorf("invalid move patch path: %s", spec)
			}
			scopedFrom, err := resolveScopedPath(scope, strings.TrimSpace(from))
			if err != nil {
				return "", err
			}
			scopedTo, err := resolveScopedPath(scope, strings.TrimSpace(to))
			if err != nil {
				return "", err
			}
			lines[i] = "*** Move File: " + scopedFrom + " -> " + scopedTo
		}
	}
	return strings.Join(lines, "\n"), nil
}
