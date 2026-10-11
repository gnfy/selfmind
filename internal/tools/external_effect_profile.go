package tools

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

const effectScriptRulePrefix = "rule:effect_script:v1:"

const (
	maxEffectEnvAssignments = 16
	maxEffectEnvValueBytes  = 512
	maxEffectStdinTextBytes = 4096
)

var effectEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	// Cwd is the workspace directory the script and its observation command
	// run from; empty is the workspace root.
	Cwd string
	// Env is every NAME=VALUE assignment the invocation writes before the
	// script. StdinText (a here-string) or StdinFile (a workspace file
	// redirected with <) is its one standard input. A call that declares
	// other inputs is a different invocation.
	Env       []string
	StdinText *string
	StdinFile string
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
	// A root invocation with no declared inputs encodes none of these, as
	// profiles registered before they existed did: such a profile means the
	// workspace root with no environment assignments or standard input.
	Cwd       string `json:"cw,omitempty"`
	EnvHash   string `json:"e,omitempty"`
	StdinHash string `json:"s,omitempty"`
	// The paths as written, which a script sees as $0 and $PWD, are encoded
	// only where they differ from the physical ones above (a symlink on the
	// way); a rule without them was registered by its physical paths.
	Invoked            string `json:"iv,omitempty"`
	LogicalCwd         string `json:"lc,omitempty"`
	ObservationInvoked string `json:"oi,omitempty"`
}

func (r effectScriptRule) invokedPath() string {
	if r.Invoked != "" {
		return r.Invoked
	}
	return r.Relative
}

func (r effectScriptRule) logicalCwd() string {
	if r.LogicalCwd != "" {
		return r.LogicalCwd
	}
	return r.Cwd
}

func (r effectScriptRule) observationInvokedPath() string {
	if r.ObservationInvoked != "" {
		return r.ObservationInvoked
	}
	return r.ObservationRelative
}

// effectInvocation is one literal script invocation: its argv, the
// environment assignments written before it in sorted NAME=VALUE form, and
// at most one standard input. No part needs expansion, so it is exactly what
// the shell runs.
type effectInvocation struct {
	argv      []string
	env       []string
	stdinText *string
	stdinFile string
}

func (i effectInvocation) declaresInputs() bool {
	return len(i.env) > 0 || i.stdinText != nil || i.stdinFile != ""
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
	physicalRoot, resolved, rel, digest, err := observationScriptMaterial(workspaceRoot, profile.ScriptPath)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	if !shellPathReaches(workspaceRoot, profile.ScriptPath, resolved) {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script path reaches another file through a symlink and \"..\"")
	}
	if path, _, ok := directObservationScriptInvocation([]string{profile.ScriptPath}); !ok || path != profile.ScriptPath {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script must be invoked directly with a script path")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode()&0o111 == 0 {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script must be executable")
	}
	cwd, err := effectProfileCwd(physicalRoot, profile.Cwd)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	invoked, scriptInside := effectLogicalPath(workspaceRoot, profile.ScriptPath)
	logicalCwd, cwdInside := effectLogicalPath(workspaceRoot, profile.Cwd)
	if !scriptInside || !cwdInside {
		return ApprovalRuleCandidate{}, fmt.Errorf("effect script and cwd must be named inside the workspace")
	}
	env, err := canonicalEffectEnv(profile.Env)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	stdin, err := effectProfileStdin(workspaceRoot, profile.StdinText, profile.StdinFile)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	rule := effectScriptRule{WorkspaceID: profile.WorkspaceID, Relative: rel, Digest: digest,
		ArgsHash: effectArgsHash(profile.Argv), Targets: targets,
		Network: profile.AllowNetwork, Credentials: profile.AllowCredentials,
		Cwd: cwd, EnvHash: effectEnvHash(env), StdinHash: effectStdinHash(stdin)}
	if invoked != rel {
		rule.Invoked = invoked
	}
	if logicalCwd != cwd {
		rule.LogicalCwd = logicalCwd
	}
	if profile.ObservationCommand != "" {
		invocation, ok := parseEffectInvocation(profile.ObservationCommand)
		if !ok || invocation.declaresInputs() || invocation.argv[0] == "" || !strings.Contains(invocation.argv[0], "/") {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation must be one exact direct script command without its own environment or input")
		}
		argv := invocation.argv
		path, _, scriptOK := directObservationScriptInvocation(argv)
		if !scriptOK || path != argv[0] {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation must invoke the script directly")
		}
		// The watcher runs the command from the profile's directory.
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspaceRoot, filepath.FromSlash(logicalCwd), path)
		}
		observationInvoked, inside := effectLogicalPath(workspaceRoot, path)
		if !inside {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation script must be named inside the workspace")
		}
		_, resolved, observationRel, observationDigest, err := observationScriptMaterial(workspaceRoot, path)
		if err != nil {
			return ApprovalRuleCandidate{}, err
		}
		if !shellPathReaches(filepath.Join(workspaceRoot, filepath.FromSlash(logicalCwd)), argv[0], resolved) {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation script path reaches another file through a symlink and \"..\"")
		}
		info, err := os.Stat(resolved)
		if err != nil || info.Mode()&0o111 == 0 {
			return ApprovalRuleCandidate{}, fmt.Errorf("effect observation script must be executable")
		}
		rule.ObservationRelative = observationRel
		rule.ObservationDigest = observationDigest
		rule.ObservationArgsHash = effectArgsHash(argv[1:])
		if observationInvoked != observationRel {
			rule.ObservationInvoked = observationInvoked
		}
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		return ApprovalRuleCandidate{}, err
	}
	label := fmt.Sprintf("unchanged effect script %s", invoked)
	if invoked != rel {
		label += " (file " + rel + ")"
	}
	if logicalCwd != "" {
		label += " run from " + logicalCwd
	}
	if len(env) > 0 {
		label += fmt.Sprintf(" with %d declared env assignment(s)", len(env))
	}
	if file, ok := strings.CutPrefix(stdin, "file:"); ok {
		stdinRel, _, _ := strings.Cut(file, "\x00")
		label += " with stdin from " + stdinRel
	} else if stdin != "" {
		label += " with declared stdin text"
	}
	return ApprovalRuleCandidate{Kind: "effect_script",
		Key:   effectScriptRulePrefix + base64.RawURLEncoding.EncodeToString(encoded),
		Label: label + " for " + strings.Join(targets, ", ")}, nil
}

func effectArgsHash(args []string) string {
	encoded, _ := json.Marshal(append([]string{}, args...))
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

// effectEnvHash and effectStdinHash keep declared values out of the stored
// key; an empty declaration encodes nothing.
func effectEnvHash(env []string) string {
	if len(env) == 0 {
		return ""
	}
	return effectArgsHash(env)
}

func effectStdinHash(stdin string) string {
	if stdin == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(stdin))
	return fmt.Sprintf("%x", sum[:])
}

// effectProfileCwd resolves the registered directory to its physical,
// workspace-relative form; the workspace root is "".
func effectProfileCwd(physicalRoot, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "." {
		return "", nil
	}
	dir := raw
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(physicalRoot, dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("effect script cwd must be an existing workspace directory")
	}
	rel, ok := effectWorkspaceDir(physicalRoot, dir)
	if !ok {
		return "", fmt.Errorf("effect script cwd must be inside the workspace")
	}
	return rel, nil
}

// effectLogicalPath names path as written, relative to the workspace root as
// configured and without resolving symlinks: what a script sees as $0 or
// $PWD. A relative path is taken from the root; "" is the root itself.
func effectLogicalPath(root, path string) (string, bool) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

// effectWorkspaceDir names dir by its physical path relative to the
// workspace. A profile binds both this and the path as written.
func effectWorkspaceDir(physicalRoot, dir string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(physicalRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

func canonicalEffectEnv(entries []string) ([]string, error) {
	if len(entries) > maxEffectEnvAssignments {
		return nil, fmt.Errorf("effect script declares at most %d env assignments", maxEffectEnvAssignments)
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !effectEnvName.MatchString(name) || seen[name] ||
			len(value) > maxEffectEnvValueBytes || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("effect script env must be distinct NAME=VALUE assignments")
		}
		seen[name] = true
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out, nil
}

// effectProfileStdin is the declared standard input: "text:" and the
// here-string, or "file:" and the workspace file's path and content digest.
// A registered file path is workspace-relative, like the script path.
func effectProfileStdin(workspaceRoot string, text *string, file string) (string, error) {
	file = strings.TrimSpace(file)
	switch {
	case text != nil && file != "":
		return "", fmt.Errorf("effect script takes one standard input: text or a file")
	case text != nil:
		if len(*text) > maxEffectStdinTextBytes || strings.ContainsRune(*text, 0) {
			return "", fmt.Errorf("effect script stdin text is invalid")
		}
		return "text:" + *text, nil
	case file != "":
		_, resolved, rel, digest, err := observationScriptMaterial(workspaceRoot, file)
		if err != nil {
			return "", fmt.Errorf("effect script stdin file: %w", err)
		}
		if !shellPathReaches(workspaceRoot, file, resolved) {
			return "", fmt.Errorf("effect script stdin file reaches another file through a symlink and \"..\"")
		}
		return "file:" + rel + "\x00" + digest, nil
	default:
		return "", nil
	}
}

// callStdin is the same declaration for a call: a redirected file is read
// relative to the call's directory and must stay inside the run's scope.
func (i effectInvocation) callStdin(scope ExecutionScope, cwd string) (string, bool) {
	switch {
	case i.stdinText != nil:
		return "text:" + *i.stdinText, true
	case i.stdinFile != "":
		path := i.stdinFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		_, resolved, rel, digest, err := observationScriptMaterial(scope.WorkspaceRoot, path)
		if err != nil || !scopeAllowsPath(scope, filepath.Clean(path)) || !shellPathReaches(cwd, i.stdinFile, resolved) {
			return "", false
		}
		return "file:" + rel + "\x00" + digest, true
	default:
		return "", true
	}
}

// effectInvocationCwd is the directory a terminal or watcher call runs from,
// cleaned as text as the workspace scope cleans it before the call runs.
func effectInvocationCwd(scope ExecutionScope, raw string) string {
	cwd := strings.TrimSpace(raw)
	if cwd == "" || cwd == "." {
		return scope.WorkspaceRoot
	}
	if !filepath.IsAbs(cwd) {
		return filepath.Join(scope.WorkspaceRoot, cwd)
	}
	return filepath.Clean(cwd)
}

// registeredEffectScriptTargets returns only operator-asserted targets. A
// missing, revoked, stale, ambiguous or unreadable profile falls back to the
// person-wide unknown lane; model text never narrows the claim.
func registeredEffectScriptTargets(args map[string]interface{}, store *control.Store) ([]string, bool) {
	targets, _, ok := registeredEffectScript(args, store)
	return targets, ok
}

// registeredEffectScript also reports whether the matched profile names an
// observation command: the owner's assertion that the script's effect
// completes after it returns and is settled by that observation.
func registeredEffectScript(args map[string]interface{}, store *control.Store) ([]string, bool, bool) {
	if store == nil || !isExecTool(stringArg(args, "_tool_name")) {
		return nil, false, false
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || scope.TrustLevel != executionenv.TrustTrusted || !scope.StandingGrants.Allowed ||
		scope.WorkspaceID == "" || scope.WorkspaceRoot == "" {
		return nil, false, false
	}
	invocation, ok := parseEffectInvocation(strings.TrimSpace(execCommandPayload(stringArg(args, "_tool_name"), args)))
	if !ok {
		return nil, false, false
	}
	scriptPath, scriptArgs, ok := directObservationScriptInvocation(invocation.argv)
	if !ok || scriptPath != invocation.argv[0] || !strings.Contains(scriptPath, "/") {
		return nil, false, false
	}
	cwd := effectInvocationCwd(scope, stringArg(args, "cwd"))
	absScript := filepath.Join(cwd, scriptPath)
	if filepath.IsAbs(scriptPath) {
		absScript = scriptPath
	}
	physicalRoot, resolvedScript, rel, digest, err := observationScriptMaterial(scope.WorkspaceRoot, absScript)
	if err != nil || resolvedScript == "" || !scopeAllowsPath(scope, filepath.Clean(absScript)) ||
		!shellPathReaches(cwd, scriptPath, resolvedScript) {
		return nil, false, false
	}
	cwdRel, ok := effectWorkspaceDir(physicalRoot, cwd)
	if !ok {
		return nil, false, false
	}
	invoked, scriptInside := effectLogicalPath(scope.WorkspaceRoot, absScript)
	logicalCwd, cwdInside := effectLogicalPath(scope.WorkspaceRoot, cwd)
	if !scriptInside || !cwdInside {
		return nil, false, false
	}
	stdin, ok := invocation.callStdin(scope, cwd)
	if !ok {
		return nil, false, false
	}
	ctx := contextFromArgs(args)
	grants, err := store.ListApprovalGrants(ctx, scope.TenantID, scope.PersonID, false)
	if err != nil {
		return nil, false, false
	}
	credentials, _ := args[credentialReadArgKey].(bool)
	want := effectScriptRule{WorkspaceID: scope.WorkspaceID, Relative: rel, Digest: digest,
		ArgsHash: effectArgsHash(scriptArgs), Network: networkSharedArg(args), Credentials: credentials,
		Cwd: cwdRel, EnvHash: effectEnvHash(invocation.env), StdinHash: effectStdinHash(stdin)}
	var matched []string
	observed := false
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
			rule.Cwd != want.Cwd || rule.EnvHash != want.EnvHash || rule.StdinHash != want.StdinHash ||
			rule.invokedPath() != invoked || rule.logicalCwd() != logicalCwd ||
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
			return nil, false, false
		}
		matched, observed = rule.Targets, rule.ObservationRelative != ""
	}
	return matched, observed, matched != nil
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
	invocation, ok := parseEffectInvocation(strings.TrimSpace(stringArg(args, "command")))
	if !ok || invocation.declaresInputs() {
		return EffectObservationBinding{}, false
	}
	argv := invocation.argv
	path, scriptArgs, ok := directObservationScriptInvocation(argv)
	if !ok || path != argv[0] {
		return EffectObservationBinding{}, false
	}
	cwd := effectInvocationCwd(scope, stringArg(args, "cwd"))
	absScript := filepath.Join(cwd, path)
	if filepath.IsAbs(path) {
		absScript = path
	}
	physicalRoot, resolved, rel, digest, err := observationScriptMaterial(scope.WorkspaceRoot, absScript)
	if err != nil || resolved == "" || !scopeAllowsPath(scope, filepath.Clean(absScript)) ||
		!shellPathReaches(cwd, path, resolved) {
		return EffectObservationBinding{}, false
	}
	cwdRel, ok := effectWorkspaceDir(physicalRoot, cwd)
	if !ok {
		return EffectObservationBinding{}, false
	}
	invoked, scriptInside := effectLogicalPath(scope.WorkspaceRoot, absScript)
	logicalCwd, cwdInside := effectLogicalPath(scope.WorkspaceRoot, cwd)
	if !scriptInside || !cwdInside {
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
			rule.ObservationArgsHash != effectArgsHash(scriptArgs) || rule.Cwd != cwdRel ||
			rule.observationInvokedPath() != invoked || rule.logicalCwd() != logicalCwd ||
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

// The effect contract accepts only one literal script invocation. Its
// declared inputs are literal NAME=VALUE assignments before the script and one
// standard input, a here-string or a file redirected with <. Other shell
// constructs, pipes, redirections and expansion cannot hide an additional
// target behind a reviewed argv shape.
func parseEffectInvocation(payload string) (effectInvocation, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(payload), "")
	if err != nil || len(file.Stmts) != 1 {
		return effectInvocation{}, false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown || len(stmt.Redirs) > 1 {
		return effectInvocation{}, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return effectInvocation{}, false
	}
	var invocation effectInvocation
	seen := make(map[string]bool, len(call.Assigns))
	for _, assign := range call.Assigns {
		if assign == nil || assign.Append || assign.Naked || assign.Index != nil || assign.Array != nil ||
			assign.Name == nil || !effectEnvName.MatchString(assign.Name.Value) || seen[assign.Name.Value] {
			return effectInvocation{}, false
		}
		seen[assign.Name.Value] = true
		value := ""
		if assign.Value != nil {
			if value, ok = staticObservationWord(assign.Value); !ok {
				return effectInvocation{}, false
			}
		}
		invocation.env = append(invocation.env, assign.Name.Value+"="+value)
	}
	slices.Sort(invocation.env)
	for _, redir := range stmt.Redirs {
		if redir == nil || redir.N != nil && redir.N.Value != "0" {
			return effectInvocation{}, false
		}
		value, ok := staticObservationWord(redir.Word)
		if !ok {
			return effectInvocation{}, false
		}
		switch redir.Op {
		case syntax.WordHdoc:
			invocation.stdinText = &value
		case syntax.RdrIn:
			if value == "" {
				return effectInvocation{}, false
			}
			invocation.stdinFile = value
		default:
			return effectInvocation{}, false
		}
	}
	for _, word := range call.Args {
		value, ok := staticObservationWord(word)
		if !ok {
			return effectInvocation{}, false
		}
		invocation.argv = append(invocation.argv, value)
	}
	return invocation, true
}
