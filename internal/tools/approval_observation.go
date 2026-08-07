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
	{program: "jq", anyArgs: true, credentialSafe: true, reject: []string{"-i", "--in-place"}},
	{program: "yq", anyArgs: true, reject: []string{"-i", "--inplace"}},
	{program: "git", prefixes: [][]string{{"status"}, {"diff"}, {"log"}, {"show"}, {"rev-parse"}, {"merge-base"}, {"ls-files"}, {"ls-tree"}, {"cat-file"}, {"describe"}, {"remote", "get-url"}}},
	{program: "gcloud", credentialSafe: true, prefixes: [][]string{{"auth", "list"}, {"builds", "list"}, {"builds", "describe"}, {"builds", "triggers", "list"}, {"builds", "triggers", "describe"}, {"run", "services", "list"}, {"run", "services", "describe"}, {"container", "clusters", "list"}, {"container", "clusters", "describe"}, {"projects", "list"}, {"projects", "describe"}, {"projects", "get-iam-policy"}, {"config", "list"}, {"config", "get-value"}}},
	{program: "aws", credentialSafe: true, prefixes: [][]string{{"sts", "get-caller-identity"}, {"codebuild", "batch-get-builds"}, {"codebuild", "list-builds"}, {"codebuild", "list-builds-for-project"}, {"codepipeline", "get-pipeline-execution"}, {"codepipeline", "list-pipeline-executions"}, {"iam", "get-role"}, {"iam", "get-role-policy"}, {"iam", "get-policy"}, {"iam", "get-policy-version"}, {"iam", "list-roles"}, {"iam", "list-policies"}, {"iam", "list-role-policies"}, {"iam", "list-attached-role-policies"}, {"iam", "simulate-principal-policy"}, {"kms", "describe-key"}, {"kms", "get-key-policy"}, {"kms", "list-keys"}, {"kms", "list-aliases"}, {"ssm", "describe-parameters"}}},
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

// deterministicObservationExec accepts only statically parseable shell
// payloads whose every effective program matches the declarative catalog. The
// observation parser is intentionally separate from the conservative safety
// tokenizer: quoted jq/format expressions must not create approval fatigue,
// while substitutions, heredocs, script files, interpreters, privilege
// wrappers, and unknown agent CLIs remain gated.
func deterministicObservationExec(toolName string, args map[string]interface{}) bool {
	if !isExecTool(toolName) || strings.EqualFold(toolName, "execute_code") {
		return false
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return false
	}
	commands, ok := parseObservationCommands(payload)
	if !ok || len(commands) == 0 {
		return false
	}
	credentialed, _ := args[credentialReadArgKey].(bool)
	for _, fields := range commands {
		program := strings.ToLower(filepath.Base(fields[0]))
		rule, ok := observationRuleByProgram[program]
		if !ok || (credentialed && !rule.credentialSafe) {
			return false
		}
		commandArgs, ok := observationCommandArgs(program, fields[1:])
		if !ok || !rule.matches(commandArgs) || (credentialed && program == "jq" && !credentialSafeJQ(commandArgs)) {
			return false
		}
	}
	return true
}

// credentialSafeJQ permits the common "cloud CLI | jq 'literal-filter'" shape
// without letting a credential-bearing jq process read environment variables
// or arbitrary files. Value-taking/unknown options and any trailing file
// operand remain human-gated.
func credentialSafeJQ(args []string) bool {
	filterSeen := false
	for _, arg := range args {
		v := strings.TrimSpace(arg)
		if !filterSeen && strings.HasPrefix(v, "-") {
			trimmed := strings.TrimLeft(v, "-")
			if trimmed == "" || strings.ContainsAny(trimmed, "0123456789") {
				return false
			}
			for _, flag := range strings.Split(trimmed, "") {
				if !strings.Contains("cerRMnSsja", flag) {
					return false
				}
			}
			continue
		}
		if filterSeen {
			return false
		}
		lower := strings.ToLower(v)
		if lower == "" || strings.Contains(lower, "env") {
			return false
		}
		filterSeen = true
	}
	return filterSeen
}

var observationGlobalValueFlags = map[string]map[string]struct{}{
	"aws": {
		"--profile": {}, "--region": {}, "--output": {}, "--endpoint-url": {},
		"--ca-bundle": {}, "--cli-connect-timeout": {}, "--cli-read-timeout": {},
	},
	"gcloud": {
		"--account": {}, "--billing-project": {}, "--configuration": {},
		"--format": {}, "--project": {}, "--trace-token": {},
		"--user-output-enabled": {}, "--verbosity": {},
	},
	"kubectl": {
		"--as": {}, "--as-group": {}, "--as-uid": {}, "--cache-dir": {},
		"--certificate-authority": {}, "--client-certificate": {}, "--client-key": {},
		"--cluster": {}, "--context": {}, "--kubeconfig": {}, "--namespace": {},
		"--request-timeout": {}, "--server": {}, "--tls-server-name": {},
		"--user": {}, "-n": {},
	},
	"helm": {
		"--kube-apiserver": {}, "--kube-context": {}, "--kube-tls-server-name": {},
		"--kubeconfig": {}, "--namespace": {}, "--registry-config": {},
		"--repository-cache": {}, "--repository-config": {}, "-n": {},
	},
	"gh":     {"--hostname": {}, "-R": {}, "--repo": {}},
	"argocd": {"--config": {}, "--controller-name": {}, "--grpc-web-root-path": {}, "--server": {}},
}

var observationGlobalBoolFlags = map[string]map[string]struct{}{
	"aws":     {"--debug": {}, "--no-cli-pager": {}, "--no-paginate": {}, "--no-sign-request": {}},
	"gcloud":  {"--quiet": {}, "-q": {}},
	"kubectl": {"--disable-compression": {}, "--insecure-skip-tls-verify": {}, "--match-server-version": {}, "--warnings-as-errors": {}},
	"helm":    {"--debug": {}, "--kube-insecure-skip-tls-verify": {}},
	"gh":      {"--help": {}},
	"argocd":  {"--core": {}, "--grpc-web": {}, "--insecure": {}, "--plaintext": {}},
}

// observationCommandArgs strips only known global options that precede a CLI's
// subcommand. Unknown leading options fail closed because their value shape is
// unknown; subcommand-specific flags remain available to the rule's reject
// checks and otherwise do not affect prefix matching.
func observationCommandArgs(program string, args []string) ([]string, bool) {
	valueFlags := observationGlobalValueFlags[program]
	boolFlags := observationGlobalBoolFlags[program]
	if len(valueFlags) == 0 && len(boolFlags) == 0 {
		return args, true
	}
	for i := 0; i < len(args); {
		arg := strings.TrimSpace(args[i])
		if arg == "--" {
			return args[i+1:], i+1 < len(args)
		}
		if !strings.HasPrefix(arg, "-") {
			return args[i:], true
		}
		name := arg
		hasAttachedValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
			hasAttachedValue = true
		}
		if _, ok := boolFlags[name]; ok {
			i++
			continue
		}
		if _, ok := valueFlags[name]; !ok {
			return nil, false
		}
		i++
		if !hasAttachedValue {
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return nil, false
			}
			i++
		}
	}
	return nil, false
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
