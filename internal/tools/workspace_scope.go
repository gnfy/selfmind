package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

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
	Approval      ToolApprovalHandler
}

var executionScopes sync.Map // tenantID used by the agent -> ExecutionScope

// SetExecutionScope installs scope for tenantID and returns a cleanup function.
func SetExecutionScope(tenantID string, scope ExecutionScope) func() {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return func() {}
	}
	executionScopes.Store(tenantID, scope)
	return func() {
		executionScopes.Delete(tenantID)
	}
}

func currentExecutionScope(args map[string]interface{}) (ExecutionScope, bool) {
	scope, ok := currentExecutionScopeAny(args)
	return scope, ok && strings.TrimSpace(scope.WorkspaceRoot) != ""
}

func currentExecutionScopeAny(args map[string]interface{}) (ExecutionScope, bool) {
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
			case "terminal":
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
		return "", fmt.Errorf("path %s escapes workspace allowed roots", clean)
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
