package tools

import (
	"path/filepath"
	"strings"
)

// observationRule is data, not a vendor branch in the approval engine. New
// agent-facing CLIs can join this catalog when their read-only grammar is known;
// unknown tools and unknown subcommands remain approval-gated.
type observationRule struct {
	program        string
	prefixes       [][]string
	reject         []string
	anyArgs        bool
	credentialSafe bool
}

var observationRules = []observationRule{
	{program: "ls", anyArgs: true}, {program: "pwd", anyArgs: true},
	{program: "cat", anyArgs: true}, {program: "head", anyArgs: true},
	{program: "tail", anyArgs: true}, {program: "wc", anyArgs: true},
	{program: "stat", anyArgs: true}, {program: "file", anyArgs: true},
	{program: "readlink", anyArgs: true}, {program: "realpath", anyArgs: true},
	{program: "basename", anyArgs: true}, {program: "dirname", anyArgs: true},
	{program: "grep", anyArgs: true}, {program: "rg", anyArgs: true, reject: []string{"--pre", "--pre-glob"}},
	{program: "jq", anyArgs: true, reject: []string{"-i", "--in-place"}},
	{program: "yq", anyArgs: true, reject: []string{"-i", "--inplace"}},
	{program: "git", prefixes: [][]string{{"status"}, {"diff"}, {"log"}, {"show"}, {"rev-parse"}, {"merge-base"}, {"ls-files"}, {"ls-tree"}, {"cat-file"}, {"describe"}, {"remote", "get-url"}}},
	{program: "gcloud", credentialSafe: true, prefixes: [][]string{{"builds", "list"}, {"builds", "describe"}, {"run", "services", "list"}, {"run", "services", "describe"}, {"container", "clusters", "list"}, {"container", "clusters", "describe"}, {"projects", "list"}, {"projects", "describe"}, {"config", "list"}, {"config", "get-value"}}},
	{program: "aws", credentialSafe: true, prefixes: [][]string{{"sts", "get-caller-identity"}, {"codebuild", "batch-get-builds"}, {"codebuild", "list-builds"}, {"codebuild", "list-builds-for-project"}, {"codepipeline", "get-pipeline-execution"}, {"codepipeline", "list-pipeline-executions"}}},
	{program: "kubectl", credentialSafe: true, prefixes: [][]string{{"get"}, {"describe"}, {"diff"}, {"logs"}, {"version"}, {"cluster-info"}, {"auth", "can-i"}}, reject: []string{"secret", "secrets", "--raw"}},
	{program: "helm", credentialSafe: true, prefixes: [][]string{{"list"}, {"status"}, {"history"}, {"show"}, {"search"}, {"template"}, {"lint"}, {"env"}, {"version"}}},
	{program: "gh", credentialSafe: true, prefixes: [][]string{{"pr", "view"}, {"pr", "list"}, {"pr", "status"}, {"run", "view"}, {"run", "list"}, {"repo", "view"}, {"status"}}},
	{program: "argocd", credentialSafe: true, prefixes: [][]string{{"app", "get"}, {"app", "list"}, {"app", "diff"}, {"app", "manifests"}, {"version"}, {"account", "get-user-info"}}},
}

var observationRuleByProgram = func() map[string]observationRule {
	out := make(map[string]observationRule, len(observationRules))
	for _, rule := range observationRules {
		out[rule.program] = rule
	}
	return out
}()

// deterministicObservationExec accepts only parseable shell payloads with no
// redirection and whose every effective program matches the declarative catalog.
// It intentionally rejects execute_code, heredocs, script files, interpreters,
// package scripts, and unknown agent CLIs.
func deterministicObservationExec(toolName string, args map[string]interface{}) bool {
	if !isExecTool(toolName) || strings.EqualFold(toolName, "execute_code") {
		return false
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" || strings.ContainsAny(payload, "><") {
		return false
	}
	segments, unparsed := expandCommandSegments(payload, 0)
	if unparsed || len(segments) == 0 {
		return false
	}
	matched := 0
	credentialed, _ := args[credentialReadArgKey].(bool)
	for _, fields := range segments {
		idx, ok := segmentProgram(fields)
		if !ok {
			continue
		}
		program := strings.ToLower(filepath.Base(fields[idx]))
		if _, neutral := shellNeutralWords[program]; neutral {
			continue
		}
		rule, ok := observationRuleByProgram[program]
		if !ok || (credentialed && !rule.credentialSafe) || !rule.matches(fields[idx+1:]) {
			return false
		}
		matched++
	}
	return matched > 0
}

func (r observationRule) matches(args []string) bool {
	raw := make([]string, 0, len(args))
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		v := strings.ToLower(strings.Trim(strings.TrimSpace(arg), `"'`))
		if v != "" {
			raw = append(raw, v)
		}
		if v == "" || strings.HasPrefix(v, "-") {
			continue
		}
		normalized = append(normalized, v)
	}
	all := strings.Join(raw, " ")
	for _, rejected := range r.reject {
		if strings.Contains(all, strings.ToLower(rejected)) {
			return false
		}
	}
	if r.anyArgs {
		return true
	}
	for _, prefix := range r.prefixes {
		if len(normalized) < len(prefix) {
			continue
		}
		ok := true
		for i := range prefix {
			if normalized[i] != prefix[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
