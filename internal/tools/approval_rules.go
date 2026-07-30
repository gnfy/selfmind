package tools

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Approval rules are NARROW, persistable authorizations a human may grant while
// answering one ask: "don't ask again for commands that start with `git status`",
// "allow this host", "allow writes under this root". They exist because the only
// reusable memory available before was the coarse action CLASS, which forces a
// choice between answering the same question forever and remembering something
// broader than the person meant.
//
// Three invariants hold for every candidate:
//
//  1. Derivable. The key is recomputed from the CURRENT call, so redeeming a
//     stored grant is a lookup, never a pattern match against remembered text.
//  2. Non-secret and bounded. Keys carry a command prefix, a host, or a
//     directory — never full command text, arguments, tokens, or credentials.
//  3. Offered-only. A client may only pick a rule the daemon offered for THIS
//     request (approvalRuleByKey re-derives and compares), so a decision cannot
//     inject an arbitrary authorization.
//
// A rule is never a way around the hard floor: candidates are computed after
// hardlineToolCall has already returned, and layer 3 consults grants only for
// ops the floor allowed.

// Rule kinds. The prefix is part of the persisted key, so these strings are a
// storage contract: changing one invalidates existing grants by design.
const (
	ApprovalRuleKindCommandPrefix = "exec_prefix"
	ApprovalRuleKindNetworkHost   = "net_host"
	ApprovalRuleKindPathRoot      = "path_root"
)

// approvalRuleCommandPrefixTokens bounds how much of a command a prefix rule
// covers. Two tokens is the sweet spot observed in practice: `git status` and
// `npm run` are useful classes, while one token (`git`) authorizes `git push
// --force` and three tokens rarely match the next call.
const approvalRuleCommandPrefixTokens = 2

// ApprovalRuleCandidate is one narrow authorization a human may grant while
// answering an ask. Label is user-facing and must read as the rule itself.
type ApprovalRuleCandidate struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Label string `json:"label"`
}

// approvalRuleCandidates derives every rule this call could legitimately create.
// It returns nil when nothing narrower than the action class can be described —
// the honest answer, and the reason the caller must not synthesize options of
// its own.
func approvalRuleCandidates(toolName string, args map[string]interface{}, scope ExecutionScope, dangerousReason string) []ApprovalRuleCandidate {
	var candidates []ApprovalRuleCandidate
	if isExecTool(toolName) {
		if prefix, ok := approvalCommandPrefix(toolName, args); ok {
			candidates = append(candidates, ApprovalRuleCandidate{
				Kind:  ApprovalRuleKindCommandPrefix,
				Key:   approvalRuleKey(ApprovalRuleKindCommandPrefix, prefix),
				Label: fmt.Sprintf("commands that start with `%s`", prefix),
			})
		}
		if host, ok := approvalEgressHost(toolName, args); ok {
			candidates = append(candidates, ApprovalRuleCandidate{
				Kind:  ApprovalRuleKindNetworkHost,
				Key:   approvalRuleKey(ApprovalRuleKindNetworkHost, host),
				Label: fmt.Sprintf("network access to %s", host),
			})
		}
	}
	if root, ok := approvalPathRoot(toolName, args, scope, dangerousReason); ok {
		candidates = append(candidates, ApprovalRuleCandidate{
			Kind:  ApprovalRuleKindPathRoot,
			Key:   approvalRuleKey(ApprovalRuleKindPathRoot, root),
			Label: fmt.Sprintf("writes under %s", root),
		})
	}
	return candidates
}

// approvalRuleByKey resolves a key a decision picked against the candidates this
// call actually offers. It is the enforcement point for the offered-only
// invariant: an unknown key is refused rather than stored.
func approvalRuleByKey(candidates []ApprovalRuleCandidate, key string) (ApprovalRuleCandidate, bool) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ApprovalRuleCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.Key == trimmed {
			return candidate, true
		}
	}
	return ApprovalRuleCandidate{}, false
}

// approvalRuleKeys lists the candidate keys, used to consult grants: a stored
// rule grant matching THIS call skips the ask entirely.
func approvalRuleKeys(candidates []ApprovalRuleCandidate) []string {
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, candidate.Key)
	}
	sort.Strings(keys)
	return keys
}

func approvalRuleKey(kind, value string) string {
	return "rule:" + kind + ":" + strings.TrimSpace(value)
}

// allApprovalKeysGranted reports whether EVERY key is already authorized. Used
// for multi-target writes, where "some targets covered" must not read as yes.
func allApprovalKeysGranted(keys []string, granted func(string) bool) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if !granted(key) {
			return false
		}
	}
	return true
}

// anyApprovalKeyGranted reports whether at least one key is authorized. Used for
// rule candidates, where each candidate independently authorizes this call.
func anyApprovalKeyGranted(keys []string, granted func(string) bool) bool {
	for _, key := range keys {
		if granted(key) {
			return true
		}
	}
	return false
}

func fallbackReason(value, alternative string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return alternative
}

// prefixRuleGeneralPurposeDoors are programs the grant floor refuses as a
// FAMILY — a standing "any git command" grant would cover `git push --force` and
// `git config core.pager=…` — but which a two-token prefix bounds acceptably.
// This is the whole reason prefix rules exist: `git status` is the ask people
// answer most often, and the family key cannot express it.
var prefixRuleGeneralPurposeDoors = map[string]struct{}{
	"git": {}, "find": {}, "make": {},
}

// prefixRuleAllowedProgram reports whether a two-token prefix meaningfully bounds
// this program. Shells, interpreters, privilege wrappers, and irreversible tools
// stay excluded through the grant floor's ban list: for those, the tokens after
// the prefix (or the contents of a named script) decide what runs, so a prefix
// authorizes something it cannot describe.
func prefixRuleAllowedProgram(program string) bool {
	if _, door := prefixRuleGeneralPurposeDoors[program]; door {
		return true
	}
	_, banned := bannedGrantPrograms[program]
	return !banned
}

// approvalCommandPrefix returns the leading tokens of a single-program command,
// or false when the payload cannot bound what a prefix rule would authorize:
// complex shell payloads (pipes, redirection, substitution, globs), several
// programs, and programs whose arguments define the real work.
func approvalCommandPrefix(toolName string, args map[string]interface{}) (string, bool) {
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return "", false
	}
	for _, marker := range grantComplexShellMarkers {
		if strings.Contains(payload, marker) {
			return "", false
		}
	}
	segments, unparsed := expandCommandSegments(payload, 0)
	if unparsed || len(segments) != 1 {
		return "", false
	}
	fields := segments[0]
	progIdx, ok := segmentProgram(fields)
	if !ok {
		return "", false
	}
	// The rule must describe what the person SAW. Segment expansion unwraps
	// wrappers, so `sudo systemctl restart api` would otherwise mint
	// "systemctl restart" — a rule that silently authorizes future PRIVILEGED
	// invocations because the sudo it was granted through is not in the key.
	// Require the literal first token to be the program: any wrapper (sudo, env,
	// timeout, xargs, a shell) disqualifies a prefix rule.
	rawFields := strings.Fields(payload)
	if len(rawFields) == 0 {
		return "", false
	}
	if !strings.EqualFold(filepath.Base(rawFields[0]), filepath.Base(fields[progIdx])) {
		return "", false
	}
	tokens := make([]string, 0, approvalRuleCommandPrefixTokens)
	for _, field := range fields[progIdx:] {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}
		// A prefix must be literal: a flag with a value, a path, or anything
		// with shell-significant characters would authorize more than it reads.
		if strings.ContainsAny(trimmed, "$`\"'\\*?~") {
			break
		}
		if len(tokens) == 0 {
			trimmed = strings.ToLower(filepath.Base(trimmed))
			if !prefixRuleAllowedProgram(trimmed) {
				return "", false
			}
		} else if strings.HasPrefix(trimmed, "-") {
			// Stop before flags: `git -C /elsewhere status` must not become a
			// rule that also covers `git -C /anywhere status`.
			break
		}
		tokens = append(tokens, trimmed)
		if len(tokens) == approvalRuleCommandPrefixTokens {
			break
		}
	}
	// One bare token authorizes a whole program (`git` would cover `git push
	// --force`), which is what the action class already does. A rule must be
	// narrower than the class to be worth offering.
	if len(tokens) < approvalRuleCommandPrefixTokens {
		return "", false
	}
	return strings.Join(tokens, " "), true
}

// approvalEgressHost extracts the single host an egress command targets. Several
// hosts, or a target that is not a recognizable host, yield false: a rule that
// cannot name exactly what it opens must not be offered.
func approvalEgressHost(toolName string, args map[string]interface{}) (string, bool) {
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return "", false
	}
	segments, unparsed := expandCommandSegments(payload, 0)
	if unparsed {
		return "", false
	}
	if egress, _ := egressCommand(payload, segments); !egress {
		return "", false
	}
	hosts := map[string]struct{}{}
	for _, fields := range segments {
		for _, field := range fields {
			if host, ok := hostFromToken(field); ok {
				hosts[host] = struct{}{}
			}
		}
	}
	if len(hosts) != 1 {
		return "", false
	}
	for host := range hosts {
		return host, true
	}
	return "", false
}

// hostFromToken reads a host out of a URL or a bare host[:port] token. It is
// deliberately strict: a token that merely contains a dot (a filename, a version
// string) must not become a network authorization.
func hostFromToken(token string) (string, bool) {
	trimmed := strings.Trim(strings.TrimSpace(token), "'\"")
	if trimmed == "" || strings.HasPrefix(trimmed, "-") {
		return "", false
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", false
		}
		host := parsed.Hostname()
		if host == "" {
			return "", false
		}
		return strings.ToLower(host), true
	}
	// Bare host[:port] (nc/ssh/telnet style). Require a dotted name or an
	// explicit port so ordinary arguments cannot pass.
	candidate := trimmed
	if idx := strings.LastIndex(candidate, ":"); idx > 0 {
		port := candidate[idx+1:]
		if port != "" && strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
			candidate = candidate[:idx]
		}
	}
	if strings.ContainsAny(candidate, "/\\ =@") || !strings.Contains(candidate, ".") {
		return "", false
	}
	if strings.HasSuffix(candidate, ".") || strings.HasPrefix(candidate, ".") {
		return "", false
	}
	// Reject things that look like files rather than hosts.
	if ext := strings.ToLower(filepath.Ext(candidate)); ext != "" {
		if _, known := nonHostExtensions[ext]; known {
			return "", false
		}
	}
	return strings.ToLower(candidate), true
}

// approvalTargetRuleKeys returns one path-root rule key per target this write
// would touch OUTSIDE the scope's roots (batch B3). It is the "all keys" half of
// codex's approval-cache semantics: a multi-file patch must not be released by a
// single grant that only covers one of its targets, and conversely a "writes
// under /x" rule legitimately covers a patch whose every target is under /x.
//
// In-scope targets produce no key: the workspace root is already authorized, so
// there is nothing extra for a grant to say about them.
func approvalTargetRuleKeys(toolName string, args map[string]interface{}, scope ExecutionScope) []string {
	if !isWriteTool(toolName) {
		return nil
	}
	roots := append([]string{}, scope.AllowedRoots...)
	if trimmed := strings.TrimSpace(scope.WorkspaceRoot); trimmed != "" {
		roots = append(roots, trimmed)
	}
	seen := map[string]struct{}{}
	var keys []string
	for _, target := range approvalWriteTargets(toolName, args, scope) {
		if target == "" {
			continue
		}
		inScope := false
		for _, root := range roots {
			if isWithin(filepath.Clean(root), target) {
				inScope = true
				break
			}
		}
		if inScope {
			continue
		}
		key := approvalRuleKey(ApprovalRuleKindPathRoot, filepath.Dir(target))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// approvalWriteTargets lists the absolute paths a write-shaped call would touch:
// the single path argument, or every file named by a V4A patch envelope. Relative
// paths resolve against the scope root, matching how the call itself would.
func approvalWriteTargets(toolName string, args map[string]interface{}, scope ExecutionScope) []string {
	var targets []string
	absolute := func(path string) string {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			return ""
		}
		if !filepath.IsAbs(trimmed) {
			root := strings.TrimSpace(scope.WorkspaceRoot)
			if root == "" {
				return ""
			}
			trimmed = filepath.Join(root, trimmed)
		}
		return filepath.Clean(trimmed)
	}
	for _, key := range []string{"path", "file_path", "output_path"} {
		if value, ok := args[key].(string); ok {
			if abs := absolute(value); abs != "" {
				targets = append(targets, abs)
			}
		}
	}
	if patch, ok := args["patch"].(string); ok {
		for _, path := range approvalPatchPaths(patch) {
			if abs := absolute(path); abs != "" {
				targets = append(targets, abs)
			}
		}
	}
	return targets
}

// approvalPatchPaths reads the file paths out of a V4A patch envelope. The marker
// list is duplicated from the patch tool rather than imported from the kernel:
// internal/tools must not depend on gateway or kernel packages, and a wrong
// answer here fails toward asking, never toward approving.
func approvalPatchPaths(patch string) []string {
	var paths []string
	for i, line := range strings.Split(patch, "\n") {
		if i >= approvalSummaryMaxLines {
			break
		}
		trimmed := strings.TrimSpace(line)
		for _, marker := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "*** Move File:"} {
			if !strings.HasPrefix(trimmed, marker) {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			// "*** Move File: old -> new" authorizes BOTH sides.
			if before, after, found := strings.Cut(value, "->"); found {
				paths = append(paths, strings.TrimSpace(before), strings.TrimSpace(after))
				break
			}
			if value != "" {
				paths = append(paths, value)
			}
			break
		}
	}
	return paths
}

// nonHostExtensions are suffixes that make a dotted token a file, not a host.
var nonHostExtensions = map[string]struct{}{
	".sh": {}, ".py": {}, ".js": {}, ".ts": {}, ".go": {}, ".json": {}, ".yaml": {},
	".yml": {}, ".txt": {}, ".md": {}, ".log": {}, ".tar": {}, ".gz": {}, ".zip": {},
	".html": {}, ".css": {}, ".png": {}, ".jpg": {}, ".pdf": {}, ".sql": {}, ".db": {},
}

// approvalPathRoot offers a writable-root rule for an out-of-workspace target,
// which is the ask people answer most often when working across repositories. It
// only fires for that specific dangerous reason: the rule must match the reason
// the person is being asked, not silently widen an unrelated ask.
func approvalPathRoot(toolName string, args map[string]interface{}, scope ExecutionScope, dangerousReason string) (string, bool) {
	if !strings.Contains(strings.ToLower(dangerousReason), "outside project root") {
		return "", false
	}
	if !isWriteTool(toolName) && !isExecTool(toolName) {
		return "", false
	}
	target := ""
	for _, key := range []string{"path", "file_path", "output_path"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			target = strings.TrimSpace(value)
			break
		}
	}
	if target == "" || !filepath.IsAbs(target) {
		return "", false
	}
	root := filepath.Dir(filepath.Clean(target))
	// A rule rooted at "/" or a home directory is not narrower than "let it
	// write anywhere", so it is not offered.
	if root == "" || root == "/" || root == filepath.Clean(strings.TrimRight(os.Getenv("HOME"), "/")) {
		return "", false
	}
	if scope.WorkspaceRoot != "" && isWithin(filepath.Clean(scope.WorkspaceRoot), root) {
		// Already inside the workspace: nothing to authorize.
		return "", false
	}
	return root, true
}
