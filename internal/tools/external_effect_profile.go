package tools

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

const effectScriptRulePrefix = "rule:effect_script:v1:"

// EffectScriptProfile is an explicit local-owner assertion about the complete
// target set of one exact, unchanged script invocation. It does not approve
// execution or settle the effect; ordinary approval and observation still run.
type EffectScriptProfile struct {
	WorkspaceID      string
	ScriptPath       string
	Argv             []string
	TargetKeys       []string
	AllowNetwork     bool
	AllowCredentials bool
}

type effectScriptRule struct {
	WorkspaceID string   `json:"w"`
	Relative    string   `json:"r"`
	Digest      string   `json:"d"`
	ArgsHash    string   `json:"a"`
	Targets     []string `json:"t"`
	Network     bool     `json:"n"`
	Credentials bool     `json:"c"`
}

func BuildEffectScriptRule(profile EffectScriptProfile, workspaceRoot string) (ApprovalRuleCandidate, error) {
	if strings.TrimSpace(profile.WorkspaceID) == "" || len(profile.Argv) > 32 ||
		len(profile.TargetKeys) == 0 || len(profile.TargetKeys) > 16 {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script needs a workspace, exact argv and 1-16 targets")
	}
	for _, arg := range profile.Argv {
		if len(arg) > 512 || strings.ContainsAny(arg, "\x00\r\n") {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect script argv is invalid")
		}
	}
	targets := append([]string(nil), profile.TargetKeys...)
	slices.Sort(targets)
	if slices.Contains(targets, control.UnknownExternalTarget) {
		return ApprovalRuleCandidate{}, fmt.Errorf("unknown is not an exact effect target")
	}
	for i, target := range targets {
		if !control.ValidExternalTargetKey(target) || i > 0 && target == targets[i-1] {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect targets must be distinct canonical keys")
		}
	}
	_, resolved, rel, digest, err := observationScriptMaterial(workspaceRoot, profile.ScriptPath)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	if path, _, ok := directObservationScriptInvocation([]string{profile.ScriptPath}); !ok || path != profile.ScriptPath {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script must be invoked directly with a script path")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode()&0o111 == 0 {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script must be executable")
	}
	rule := effectScriptRule{WorkspaceID: profile.WorkspaceID, Relative: rel, Digest: digest,
		ArgsHash: effectArgsHash(profile.Argv), Targets: targets,
		Network: profile.AllowNetwork, Credentials: profile.AllowCredentials}
	encoded, err := json.Marshal(rule)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	return ApprovalRuleCandidate{Kind: "effect_script",
		Key:   effectScriptRulePrefix + base64.RawURLEncoding.EncodeToString(encoded),
		Label: fmt.Sprintf("unchanged effect script %s for %s", rel, strings.Join(targets, ", "))}, nil
}

func effectArgsHash(args []string) string {
	encoded, _ := json.Marshal(append([]string{}, args...))
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

// registeredEffectScriptTargets returns only operator-asserted targets. A
// missing, revoked, stale, ambiguous or unreadable profile falls back to the
// person-wide unknown lane; model text never narrows the claim.
func registeredEffectScriptTargets(args map[string]interface{}, store *control.Store) ([]string, bool) {
	if store == nil || !isExecTool(stringArg(args, "_tool_name")) {
		return nil, false
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || scope.TrustLevel != executionenv.TrustTrusted || !scope.StandingGrants.Allowed ||
		scope.WorkspaceID == "" || scope.WorkspaceRoot == "" {
		return nil, false
	}
	command, ok := directStaticEffectScriptCommand(strings.TrimSpace(execCommandPayload(stringArg(args, "_tool_name"), args)))
	if !ok {
		return nil, false
	}
	scriptPath, scriptArgs, ok := directObservationScriptInvocation(command)
	if !ok || scriptPath != command[0] || !strings.Contains(scriptPath, "/") {
		return nil, false
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
	if err != nil || resolvedScript == "" || !scopeAllowsPath(scope, filepath.Clean(absScript)) {
		return nil, false
	}
	ctx := contextFromArgs(args)
	grants, err := store.ListApprovalGrants(ctx, scope.TenantID, scope.PersonID, false)
	if err != nil {
		return nil, false
	}
	credentials, _ := args[credentialReadArgKey].(bool)
	want := effectScriptRule{WorkspaceID: scope.WorkspaceID, Relative: rel, Digest: digest,
		ArgsHash: effectArgsHash(scriptArgs), Network: networkSharedArg(args), Credentials: credentials}
	var matched []string
	for _, grant := range grants {
		if !strings.HasPrefix(grant.PatternKey, effectScriptRulePrefix) {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(grant.PatternKey, effectScriptRulePrefix))
		if err != nil || len(raw) > 8192 {
			continue
		}
		var rule effectScriptRule
		if json.Unmarshal(raw, &rule) != nil || rule.WorkspaceID != want.WorkspaceID ||
			rule.Relative != want.Relative || rule.Digest != want.Digest || rule.ArgsHash != want.ArgsHash ||
			rule.Network != want.Network || rule.Credentials != want.Credentials ||
			len(rule.Targets) == 0 || len(rule.Targets) > 16 {
			continue
		}
		valid := true
		for i, target := range rule.Targets {
			if !control.ValidExternalTargetKey(target) || target == control.UnknownExternalTarget ||
				i > 0 && rule.Targets[i-1] >= target {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		approved, err := store.IsApprovalGranted(ctx, scope.TenantID, scope.PersonID, scope.WorkspaceID,
			grant.PatternKey, scope.StandingGrants.NotAfter)
		if err != nil || !approved {
			continue
		}
		if matched != nil {
			return nil, false
		}
		matched = rule.Targets
	}
	return matched, matched != nil
}

// The effect contract accepts only one literal script invocation. Shell
// constructs, assignments, pipes, redirections and expansion cannot hide an
// additional target behind a reviewed argv shape.
func directStaticEffectScriptCommand(payload string) ([]string, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(payload), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown || len(stmt.Redirs) != 0 {
		return nil, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) == 0 {
		return nil, false
	}
	argv := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := staticObservationWord(word)
		if !ok {
			return nil, false
		}
		argv = append(argv, value)
	}
	return argv, true
}
