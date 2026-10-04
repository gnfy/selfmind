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
	WorkspaceID        string
	ScriptPath         string
	Argv               []string
	TargetKeys         []string
	AllowNetwork       bool
	AllowCredentials   bool
	ObservationCommand string
}

type effectScriptRule struct {
	WorkspaceID         string   `json:"w"`
	Relative            string   `json:"r"`
	Digest              string   `json:"d"`
	ArgsHash            string   `json:"a"`
	Targets             []string `json:"t"`
	Network             bool     `json:"n"`
	Credentials         bool     `json:"c"`
	ObservationRelative string   `json:"or,omitempty"`
	ObservationDigest   string   `json:"od,omitempty"`
	ObservationArgsHash string   `json:"oa,omitempty"`
}

// EffectObservationBinding is a local-owner assertion that an unchanged,
// separately approved read-only script observes the exact effect targets.
// The watcher must use status_json.v1; a prose or regex match cannot settle a
// remote effect automatically.
type EffectObservationBinding struct {
	RuleKey            string
	ObservationRuleKey string
	ScriptRoot         string
	ScriptPath         string
	ScriptDigest       string
	TargetKeys         []string
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
	if profile.ObservationCommand != "" {
		argv, ok := directStaticEffectScriptCommand(profile.ObservationCommand)
		if !ok || len(argv) == 0 || argv[0] == "" || !strings.Contains(argv[0], "/") {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation must be one exact direct script command")
		}
		path, _, scriptOK := directObservationScriptInvocation(argv)
		if !scriptOK || path != argv[0] {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation must invoke the script directly")
		}
		_, resolved, observationRel, observationDigest, err := observationScriptMaterial(workspaceRoot, path)
		if err != nil {
			return ApprovalRuleCandidate{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil || info.Mode()&0o111 == 0 {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation script must be executable")
		}
		rule.ObservationRelative = observationRel
		rule.ObservationDigest = observationDigest
		rule.ObservationArgsHash = effectArgsHash(argv[1:])
	}
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

// RegisteredEffectObservation validates an exact watcher command against the
// owner's effect profile. The ordinary observation-only approval is checked
// separately by watch_external before its preflight; this binding adds target
// identity, not read-only authority.
func RegisteredEffectObservation(args map[string]interface{}, store *control.Store) (EffectObservationBinding, bool) {
	if store == nil {
		return EffectObservationBinding{}, false
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || scope.TrustLevel != executionenv.TrustTrusted || !scope.StandingGrants.Allowed ||
		scope.WorkspaceID == "" || scope.WorkspaceRoot == "" {
		return EffectObservationBinding{}, false
	}
	argv, ok := directStaticEffectScriptCommand(strings.TrimSpace(stringArg(args, "command")))
	if !ok || len(argv) == 0 {
		return EffectObservationBinding{}, false
	}
	path, scriptArgs, ok := directObservationScriptInvocation(argv)
	if !ok || path != argv[0] {
		return EffectObservationBinding{}, false
	}
	cwd := strings.TrimSpace(stringArg(args, "cwd"))
	if cwd == "" || cwd == "." {
		cwd = scope.WorkspaceRoot
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(scope.WorkspaceRoot, cwd)
	}
	absScript := filepath.Join(cwd, path)
	if filepath.IsAbs(path) {
		absScript = path
	}
	physicalRoot, resolved, rel, digest, err := observationScriptMaterial(scope.WorkspaceRoot, absScript)
	if err != nil || resolved == "" || !scopeAllowsPath(scope, filepath.Clean(absScript)) {
		return EffectObservationBinding{}, false
	}
	grants, err := store.ListApprovalGrants(contextFromArgs(args), scope.TenantID, scope.PersonID, false)
	if err != nil {
		return EffectObservationBinding{}, false
	}
	credentials, _ := args[credentialReadArgKey].(bool)
	observationRuleKey := ""
	for _, key := range observationScriptRuntimeKeys(scope.WorkspaceID, rel, digest, scriptArgs, networkSharedArg(args), credentials) {
		approved, err := store.IsApprovalGranted(contextFromArgs(args), scope.TenantID, scope.PersonID,
			scope.WorkspaceID, key, scope.StandingGrants.NotAfter)
		if err == nil && approved {
			observationRuleKey = key
			break
		}
	}
	if observationRuleKey == "" {
		return EffectObservationBinding{}, false
	}
	var binding EffectObservationBinding
	for _, grant := range grants {
		if !strings.HasPrefix(grant.PatternKey, effectScriptRulePrefix) {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(grant.PatternKey, effectScriptRulePrefix))
		if err != nil || len(raw) > 8192 {
			continue
		}
		var rule effectScriptRule
		if json.Unmarshal(raw, &rule) != nil || rule.WorkspaceID != scope.WorkspaceID ||
			rule.ObservationRelative != rel || rule.ObservationDigest != digest ||
			rule.ObservationArgsHash != effectArgsHash(scriptArgs) ||
			rule.Network != networkSharedArg(args) || rule.Credentials != credentials ||
			len(rule.Targets) == 0 {
			continue
		}
		valid := true
		for i, target := range rule.Targets {
			if !control.ValidExternalTargetKey(target) || target == control.UnknownExternalTarget || i > 0 && rule.Targets[i-1] >= target {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		approved, err := store.IsApprovalGranted(contextFromArgs(args), scope.TenantID, scope.PersonID,
			scope.WorkspaceID, grant.PatternKey, scope.StandingGrants.NotAfter)
		if err != nil || !approved {
			continue
		}
		if binding.RuleKey != "" {
			return EffectObservationBinding{}, false
		}
		binding = EffectObservationBinding{RuleKey: grant.PatternKey, ObservationRuleKey: observationRuleKey,
			ScriptRoot: physicalRoot, ScriptPath: resolved,
			ScriptDigest: digest, TargetKeys: append([]string(nil), rule.Targets...)}
	}
	return binding, binding.RuleKey != ""
}

func ValidateEffectObservationScript(root, path, digest string) bool {
	_, resolved, _, current, err := observationScriptMaterial(root, path)
	physical, pathErr := filepath.EvalSymlinks(path)
	return err == nil && pathErr == nil && resolved == filepath.Clean(physical) && current == digest
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
