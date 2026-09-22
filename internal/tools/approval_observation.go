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
	// verify is an extra predicate for a program whose read/write split is not
	// expressible as a leading subcommand plus a list of forbidden substrings.
	// It runs after reject and before prefix matching, and it must fail closed.
	verify func(args []string) bool
}

var observationRules = []observationRule{
	// Shell builtins that produce no effect of their own. They are here because
	// a payload's FIRST segment is usually one of them: 64% of one week's real
	// commands led with `cd`, and an unknown program fails the whole check at
	// that first segment, so what followed was never even classified.
	//
	// This set is deliberately NARROWER than grant_floor.go's shellNeutralWords,
	// which answers a different question ("may this word name a remembered
	// class"). `trap` can carry a command body, and `read`/`declare`/`local`
	// bind names a later segment may expand, so neither belongs here: this list
	// must mean "runs and changes nothing outside the shell".
	//
	// credentialSafe here means the program cannot EMIT a credential: its output
	// is derived only from its own arguments — which the parser has already
	// proven to be static literals — or it produces no output at all. None of
	// them opens a file and prints what is inside.
	//
	// It matters because a credentialed payload requires EVERY program in it to
	// be credential-safe, and 73 of one corpus's commands died on a leading
	// `cd` and another 32 on an `echo` banner. That is the same shape as the
	// original fatigue (an unknown first segment disqualifying the rest), one
	// level in.
	//
	// `set` and `export` are deliberately NOT here although they carry no file
	// access: with no operands both print the whole variable environment, which
	// is exactly the emission this flag is meant to exclude.
	{program: "cd", anyArgs: true, credentialSafe: true}, {program: "set", anyArgs: true},
	{program: "export", anyArgs: true}, {program: "umask", anyArgs: true, credentialSafe: true},
	{program: "true", anyArgs: true, credentialSafe: true}, {program: "false", anyArgs: true, credentialSafe: true},
	{program: "test", anyArgs: true, credentialSafe: true}, {program: "[", anyArgs: true, credentialSafe: true},
	{program: ":", anyArgs: true, credentialSafe: true},
	{program: "which", anyArgs: true, credentialSafe: true},
	// Ordinary read-only filters. They dominate the middle of a pipeline, and
	// without them a single `| head` disqualified an otherwise provable command.
	// Each one that CAN write names the flag that does so in reject.
	{program: "tr", anyArgs: true}, {program: "cut", anyArgs: true},
	{program: "nl", anyArgs: true}, {program: "seq", anyArgs: true, credentialSafe: true},
	{program: "paste", anyArgs: true}, {program: "comm", anyArgs: true},
	{program: "column", anyArgs: true}, {program: "diff", anyArgs: true},
	{program: "od", anyArgs: true},
	{program: "shasum", anyArgs: true}, {program: "sha256sum", anyArgs: true},
	{program: "md5", anyArgs: true}, {program: "md5sum", anyArgs: true},
	{program: "sort", anyArgs: true, reject: []string{"-o", "--output"}},
	{program: "base64", anyArgs: true, reject: []string{"-o", "--output"}},
	{program: "date", anyArgs: true, reject: []string{"-s", "--set"}},
	// `uniq`, `tee` and `xxd` are deliberately absent: uniq and xxd write to a
	// second positional argument and tee writes to every one, and the rule shape
	// here cannot express "no positional output". `xxd in out` and especially
	// `xxd -r dump.hex out.bin` write an arbitrary file with no flag to reject,
	// and this catalog also decides what a durable watcher may re-run unattended.
	// `od` stays because every operand it takes is an input.
	{program: "ls", anyArgs: true}, {program: "pwd", anyArgs: true},
	{program: "printf", anyArgs: true, credentialSafe: true}, {program: "echo", anyArgs: true, credentialSafe: true}, {program: "sleep", anyArgs: true, credentialSafe: true},
	{program: "cat", anyArgs: true}, {program: "head", anyArgs: true},
	{program: "tail", anyArgs: true}, {program: "wc", anyArgs: true},
	{program: "stat", anyArgs: true}, {program: "file", anyArgs: true},
	{program: "readlink", anyArgs: true}, {program: "realpath", anyArgs: true},
	{program: "basename", anyArgs: true, credentialSafe: true}, {program: "dirname", anyArgs: true, credentialSafe: true},
	{program: "grep", anyArgs: true}, {program: "rg", anyArgs: true, reject: []string{"--pre", "--pre-glob"}},
	{program: "jq", anyArgs: true, credentialSafe: true, reject: []string{"-i", "--in-place"}},
	{program: "yq", anyArgs: true, reject: []string{"-i", "--inplace"}},
	{program: "git", prefixes: [][]string{{"status"}, {"diff"}, {"log"}, {"show"}, {"rev-parse"}, {"merge-base"}, {"ls-files"}, {"ls-tree"}, {"cat-file"}, {"describe"}, {"remote", "get-url"}, {"ls-remote"}}},
	{program: "gcloud", credentialSafe: true, prefixes: [][]string{{"auth", "list"}, {"builds", "list"}, {"builds", "describe"}, {"builds", "triggers", "list"}, {"builds", "triggers", "describe"}, {"run", "services", "list"}, {"run", "services", "describe"}, {"container", "clusters", "list"}, {"container", "clusters", "describe"}, {"projects", "list"}, {"projects", "describe"}, {"projects", "get-iam-policy"}, {"config", "list"}, {"config", "get-value"}, {"artifacts", "repositories", "list"}, {"artifacts", "docker", "images", "list"}, {"artifacts", "docker", "tags", "list"}, {"artifacts", "docker", "versions", "list"}}},
	{program: "aws", credentialSafe: true, prefixes: [][]string{{"sts", "get-caller-identity"}, {"codebuild", "batch-get-builds"}, {"codebuild", "batch-get-projects"}, {"codebuild", "list-builds"}, {"codebuild", "list-builds-for-project"}, {"codepipeline", "get-pipeline-execution"}, {"codepipeline", "list-pipeline-executions"}, {"iam", "get-role"}, {"iam", "get-role-policy"}, {"iam", "get-policy"}, {"iam", "get-policy-version"}, {"iam", "list-roles"}, {"iam", "list-policies"}, {"iam", "list-role-policies"}, {"iam", "list-attached-role-policies"}, {"iam", "simulate-principal-policy"}, {"kms", "describe-key"}, {"kms", "get-key-policy"}, {"kms", "list-keys"}, {"kms", "list-aliases"}, {"ssm", "describe-parameters"}, {"logs", "get-log-events"}}},
	{program: "kubectl", credentialSafe: true, prefixes: [][]string{{"get"}, {"describe"}, {"diff"}, {"logs"}, {"version"}, {"cluster-info"}, {"auth", "can-i"}}, reject: []string{"secret", "secrets", "--raw"}},
	{program: "helm", credentialSafe: true, prefixes: [][]string{{"list"}, {"status"}, {"history"}, {"show"}, {"search"}, {"template"}, {"lint"}, {"env"}, {"version"}}},
	// `gh api` defaults to GET but can perform every method through the same
	// subcommand, so the verb decides whether this is an observation. A
	// substring reject list is not enough to decide that: it read `-X=DELETE`
	// and `-XDELETE` as GETs. ghObservationSafe parses the verb with the same
	// function the class derivation uses. A request body still disqualifies the
	// call outright, whatever its verb.
	{program: "gh", credentialSafe: true, verify: ghObservationSafe, reject: []string{"--input", "--field", "-f ", "-f=", "--raw-field", "-f'"}, prefixes: [][]string{{"pr", "view"}, {"pr", "list"}, {"pr", "status"}, {"run", "view"}, {"run", "list"}, {"repo", "view"}, {"release", "view"}, {"release", "list"}, {"status"}, {"api"}}},
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
		// credentialSafe is per program; filterReadsOnlyStdin judges the
		// invocation, releasing a filter that provably opened no file. It only
		// widens, and fails closed on anything it cannot parse.
		if !ok || (credentialed && !rule.credentialSafe && !filterReadsOnlyStdin(program, fields[1:])) {
			return false
		}
		commandArgs, ok := observationCommandArgs(program, fields[1:])
		if !ok || !rule.matches(commandArgs) || (credentialed && program == "jq" && !credentialSafeJQ(commandArgs)) {
			return false
		}
		if rule.verify != nil && !rule.verify(commandArgs) {
			return false
		}
	}
	return true
}

// ghObservationSafe rejects a `gh api` call that is not provably a read. Every
// other gh subcommand in the catalog names its own read-only operation, so only
// `api` needs the check. An unresolvable verb is a mutation.
func ghObservationSafe(args []string) bool {
	if len(args) == 0 || strings.ToLower(strings.Trim(strings.TrimSpace(args[0]), `"'`)) != "api" {
		return true
	}
	method, ok := apiMethodFromArgs(args)
	if !ok {
		return false
	}
	_, read := apiReadMethods[method]
	return read
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
