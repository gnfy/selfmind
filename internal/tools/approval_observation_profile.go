package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/executionenv"
)

const (
	ApprovalRuleKindObservationScript = "observation_script"
	maxObservationScriptBytes         = 2 << 20
)

// ObservationScriptProfile is a local-owner assertion that one unchanged
// workspace script is observation-only for a bounded argv shape. It contains
// no credential values and is persisted through the existing revocable
// approval-grant ledger.
type ObservationScriptProfile struct {
	WorkspaceID      string
	ScriptPath       string
	ArgvPrefix       []string
	AllowTrailing    bool
	AllowNetwork     bool
	AllowCredentials bool
}

// BuildObservationScriptRule validates and fingerprints a profile. Callers
// must still enforce that only an authenticated local CLI can persist it.
func BuildObservationScriptRule(profile ObservationScriptProfile, workspaceRoot string) (ApprovalRuleCandidate, error) {
	_, _, rel, digest, err := observationScriptMaterial(workspaceRoot, profile.ScriptPath)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	if len(profile.ArgvPrefix) == 0 && profile.AllowTrailing {
		// This is intentionally allowed only when the local operator spelled out
		// --all-args. The unchanged content hash is still the authority boundary.
	}
	key := observationScriptRuleKey(profile.WorkspaceID, rel, digest, profile.ArgvPrefix,
		profile.AllowTrailing, profile.AllowNetwork, profile.AllowCredentials)
	return ApprovalRuleCandidate{
		Kind:  ApprovalRuleKindObservationScript,
		Key:   key,
		Label: fmt.Sprintf("unchanged observation script %s", filepath.ToSlash(rel)),
	}, nil
}

func observationOnlyExec(toolName string, args map[string]interface{}) bool {
	return deterministicObservationExec(toolName, args) || approvedObservationScript(toolName, args)
}

func approvedObservationScript(toolName string, args map[string]interface{}) bool {
	if !isExecTool(toolName) || strings.EqualFold(toolName, "execute_code") {
		return false
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || scope.Grants == nil || scope.TrustLevel != executionenv.TrustTrusted || strings.TrimSpace(scope.WorkspaceID) == "" {
		return false
	}
	commands, ok := parseObservationCommands(strings.TrimSpace(execCommandPayload(toolName, args)))
	if !ok || len(commands) != 1 {
		return false
	}
	scriptPath, scriptArgs, ok := directObservationScriptInvocation(commands[0])
	if !ok {
		return false
	}
	cwd := strings.TrimSpace(stringArg(args, "cwd"))
	if cwd == "" || cwd == "." {
		cwd = scope.WorkspaceRoot
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(scope.WorkspaceRoot, cwd)
	}
	absScript := filepath.Join(cwd, scriptPath)
	if filepath.IsAbs(scriptPath) {
		absScript = scriptPath
	}
	_, resolvedScript, rel, digest, err := observationScriptMaterial(scope.WorkspaceRoot, absScript)
	if err != nil || !scopeAllowsPath(scope, filepath.Clean(absScript)) {
		return false
	}
	if resolvedScript == "" {
		return false
	}
	network := networkSharedArg(args)
	credentials, _ := args[credentialReadArgKey].(bool)
	ctx := contextFromArgs(args)
	keys := observationScriptRuntimeKeys(scope.WorkspaceID, rel, digest, scriptArgs, network, credentials)
	for _, key := range keys {
		if scope.runGrants != nil && scope.runGrants.has(key) {
			return true
		}
		granted, _ := scope.Grants.IsApprovalGranted(ctx, scope.TenantID, scope.PersonID, scope.TaskID, key)
		if granted {
			return true
		}
	}
	return false
}

func directObservationScriptInvocation(argv []string) (string, []string, bool) {
	if len(argv) == 0 {
		return "", nil, false
	}
	program := strings.ToLower(filepath.Base(argv[0]))
	if len(argv) >= 2 {
		ext := strings.ToLower(filepath.Ext(argv[1]))
		valid := (ext == ".py" && (program == "python" || program == "python3")) ||
			(ext == ".sh" && (program == "sh" || program == "bash")) ||
			((ext == ".js" || ext == ".mjs" || ext == ".cjs") && (program == "node" || program == "nodejs"))
		if valid {
			return argv[1], append([]string{}, argv[2:]...), true
		}
	}
	switch strings.ToLower(filepath.Ext(argv[0])) {
	case ".py", ".sh", ".js", ".mjs", ".cjs":
		return argv[0], append([]string{}, argv[1:]...), true
	default:
		return "", nil, false
	}
}

func observationScriptMaterial(root, rawPath string) (string, string, string, string, error) {
	root = strings.TrimSpace(root)
	rawPath = strings.TrimSpace(rawPath)
	if root == "" || rawPath == "" {
		return "", "", "", "", fmt.Errorf("workspace root and script path are required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve workspace links: %w", err)
	}
	script := rawPath
	if !filepath.IsAbs(script) {
		script = filepath.Join(resolvedRoot, script)
	}
	script, err = filepath.Abs(script)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve script path: %w", err)
	}
	resolvedScript, err := filepath.EvalSymlinks(script)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve script links: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedScript)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", "", "", fmt.Errorf("script must be inside the workspace")
	}
	info, err := os.Stat(resolvedScript)
	if err != nil {
		return "", "", "", "", fmt.Errorf("inspect script: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxObservationScriptBytes {
		return "", "", "", "", fmt.Errorf("script must be a regular file no larger than %d bytes", maxObservationScriptBytes)
	}
	data, err := os.ReadFile(resolvedScript)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read script: %w", err)
	}
	sum := sha256.Sum256(data)
	return filepath.Clean(resolvedRoot), filepath.Clean(resolvedScript), filepath.ToSlash(rel), fmt.Sprintf("%x", sum[:]), nil
}

func observationScriptRuntimeKeys(workspaceID, rel, digest string, args []string, network, credentials bool) []string {
	keys := []string{observationScriptRuleKey(workspaceID, rel, digest, args, false, network, credentials)}
	for i := 0; i <= len(args); i++ {
		keys = append(keys, observationScriptRuleKey(workspaceID, rel, digest, args[:i], true, network, credentials))
	}
	return keys
}

func observationScriptRuleKey(workspaceID, rel, digest string, prefix []string, trailing, network, credentials bool) string {
	prefixSum := sha256.Sum256([]byte(strings.Join(prefix, "\x00")))
	pathSum := sha256.Sum256([]byte(filepath.ToSlash(filepath.Clean(rel))))
	return fmt.Sprintf("rule:%s:%s:%x:%s:%x:t%t:n%t:c%t",
		ApprovalRuleKindObservationScript, strings.TrimSpace(workspaceID), pathSum[:8], digest[:16], prefixSum[:8], trailing, network, credentials)
}
