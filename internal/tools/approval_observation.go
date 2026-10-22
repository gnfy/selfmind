package tools

import (
	"strings"
)

// observationRule is data, not a vendor branch in the approval engine. New
// agent-facing CLIs can join this catalog when their read-only grammar is known;
// unknown tools and unknown subcommands remain approval-gated.
type observationRule struct {
	program        string
	external       bool // this read-only client observes an external service
	prefixes       [][]string
	reject         []string
	anyArgs        bool
	credentialSafe bool
	// Reviewed resource selectors for the bounded fan-out proof. Unknown
	// dynamic options cannot establish a new target; this never grants approval.
	targetValueFlags []string
	// verify is an extra predicate for a program whose read/write split is not
	// expressible as a leading subcommand plus a list of forbidden substrings.
	// It runs after reject and before prefix matching, and it must fail closed.
	verify func(args []string) bool
	// readsState reports whether this invocation can read anything beyond its
	// own literal arguments, the environment, and the clock. Nil means it
	// always can: a file, a repository, or a service is its purpose. Only an
	// observation that reads state can be bound to an effect claim.
	readsState func(args []string) bool
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
	{program: "cd", anyArgs: true, credentialSafe: true, readsState: readsNoState}, {program: "set", anyArgs: true, readsState: readsNoState},
	{program: "export", anyArgs: true, readsState: readsNoState}, {program: "umask", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "true", anyArgs: true, credentialSafe: true, readsState: readsNoState}, {program: "false", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "test", anyArgs: true, credentialSafe: true, readsState: testReadsFiles}, {program: "[", anyArgs: true, credentialSafe: true, readsState: bracketReadsFiles},
	{program: ":", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "which", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "command", anyArgs: true, credentialSafe: true, readsState: readsNoState, verify: commandLookupArgs},
	{program: "unset", anyArgs: true, credentialSafe: true, readsState: readsNoState, verify: func(args []string) bool { _, ok := proxyUnsetNames(args); return ok }},
	// Ordinary read-only filters. They dominate the middle of a pipeline, and
	// without them a single `| head` disqualified an otherwise provable command.
	// Each one that CAN write names the flag that does so in reject.
	{program: "tr", anyArgs: true}, {program: "cut", anyArgs: true},
	{program: "nl", anyArgs: true}, {program: "seq", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "paste", anyArgs: true, readsState: pasteReadsFile}, {program: "comm", anyArgs: true},
	{program: "column", anyArgs: true}, {program: "diff", anyArgs: true},
	{program: "od", anyArgs: true},
	{program: "shasum", anyArgs: true}, {program: "sha256sum", anyArgs: true},
	{program: "md5", anyArgs: true}, {program: "md5sum", anyArgs: true},
	{program: "sort", anyArgs: true, verify: sortObservationArgs},
	{program: "base64", anyArgs: true, reject: []string{"-o", "--output"}},
	{program: "date", anyArgs: true, verify: dateObservationArgs, readsState: readsNoState},
	// `uniq`, `tee` and `xxd` are deliberately absent: uniq and xxd write to a
	// second positional argument and tee writes to every one, and the rule shape
	// here cannot express "no positional output". `xxd in out` and especially
	// `xxd -r dump.hex out.bin` write an arbitrary file with no flag to reject,
	// and this catalog also decides what a durable watcher may re-run unattended.
	// `od` stays because every operand it takes is an input.
	{program: "ls", anyArgs: true}, {program: "pwd", anyArgs: true, readsState: readsNoState},
	{program: "printf", anyArgs: true, credentialSafe: true, readsState: readsNoState}, {program: "echo", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "sleep", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "cat", anyArgs: true}, {program: "head", anyArgs: true},
	{program: "tail", anyArgs: true}, {program: "wc", anyArgs: true},
	{program: "stat", anyArgs: true}, {program: "file", anyArgs: true, verify: fileObservationArgs},
	{program: "readlink", anyArgs: true}, {program: "realpath", anyArgs: true},
	{program: "basename", anyArgs: true, credentialSafe: true, readsState: readsNoState}, {program: "dirname", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "ps", anyArgs: true}, {program: "lsof", anyArgs: true}, {program: "pgrep", anyArgs: true},
	{program: "df", anyArgs: true}, {program: "du", anyArgs: true}, {program: "uptime", anyArgs: true},
	{program: "uname", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "hostname", anyArgs: true, credentialSafe: true, readsState: readsNoState, verify: func(args []string) bool { return len(args) == 0 }},
	{program: "sw_vers", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "id", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "whoami", anyArgs: true, credentialSafe: true, readsState: readsNoState},
	{program: "grep", anyArgs: true}, {program: "rg", anyArgs: true, verify: rgObservationArgs},
	{program: "jq", anyArgs: true, credentialSafe: true, reject: []string{"-i", "--in-place"}, readsState: jqReadsFile},
	{program: "yq", anyArgs: true, verify: yqObservationArgs, readsState: yqReadsFile},
	{program: "git", verify: gitObservationArgs, prefixes: [][]string{{"status"}, {"diff"}, {"log"}, {"show"}, {"rev-parse"}, {"rev-list"}, {"grep"}, {"merge-base"}, {"ls-files"}, {"ls-tree"}, {"cat-file"}, {"describe"}, {"remote", "get-url"}, {"ls-remote"}}},
	{program: "gcloud", external: true, credentialSafe: true, prefixes: [][]string{{"auth", "list"}, {"builds", "list"}, {"builds", "describe"}, {"builds", "triggers", "list"}, {"builds", "triggers", "describe"}, {"run", "services", "list"}, {"run", "services", "describe"}, {"container", "clusters", "list"}, {"container", "clusters", "describe"}, {"projects", "list"}, {"projects", "describe"}, {"projects", "get-iam-policy"}, {"config", "list"}, {"config", "get-value"}, {"artifacts", "repositories", "list"}, {"artifacts", "docker", "images", "list"}, {"artifacts", "docker", "tags", "list"}, {"artifacts", "docker", "versions", "list"}}},
	{program: "aws", external: true, credentialSafe: true, targetValueFlags: []string{"--ids", "--stack-name"}, prefixes: [][]string{{"sts", "get-caller-identity"}, {"codebuild", "batch-get-builds"}, {"codebuild", "batch-get-projects"}, {"codebuild", "list-builds"}, {"codebuild", "list-builds-for-project"}, {"codepipeline", "get-pipeline-execution"}, {"codepipeline", "list-pipeline-executions"}, {"iam", "get-role"}, {"iam", "get-role-policy"}, {"iam", "get-policy"}, {"iam", "get-policy-version"}, {"iam", "list-roles"}, {"iam", "list-policies"}, {"iam", "list-role-policies"}, {"iam", "list-attached-role-policies"}, {"iam", "simulate-principal-policy"}, {"kms", "describe-key"}, {"kms", "get-key-policy"}, {"kms", "list-keys"}, {"kms", "list-aliases"}, {"ssm", "describe-parameters"}, {"logs", "get-log-events"}}},
	{program: "kubectl", external: true, credentialSafe: true, prefixes: [][]string{{"get"}, {"describe"}, {"diff"}, {"logs"}, {"version"}, {"cluster-info"}, {"auth", "can-i"}, {"config", "get-contexts"}, {"config", "current-context"}, {"config", "get-clusters"}}, reject: []string{"secret", "secrets", "--raw"}, readsState: kubectlReadsState},
	{program: "helm", external: true, credentialSafe: true, prefixes: [][]string{{"list"}, {"status"}, {"history"}, {"show"}, {"search"}, {"template"}, {"lint"}, {"env"}, {"version"}}, readsState: helmReadsState},
	// `gh api` defaults to GET but can perform every method through the same
	// subcommand, so the verb decides whether this is an observation. A
	// substring reject list is not enough to decide that: it read `-X=DELETE`
	// and `-XDELETE` as GETs. ghObservationSafe parses the verb with the same
	// function the class derivation uses. A request body still disqualifies the
	// call outright, whatever its verb.
	{program: "gh", external: true, credentialSafe: true, verify: ghObservationSafe, reject: []string{"--input", "--field", "-f ", "-f=", "--raw-field", "-f'"}, prefixes: [][]string{{"pr", "view"}, {"pr", "list"}, {"pr", "status"}, {"run", "view"}, {"run", "list"}, {"repo", "view"}, {"release", "view"}, {"release", "list"}, {"status"}, {"api"}}},
	{program: "argocd", external: true, credentialSafe: true, prefixes: [][]string{{"app", "get"}, {"app", "list"}, {"app", "diff"}, {"app", "manifests"}, {"version"}, {"account", "get-user-info"}}, readsState: argocdReadsState},
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
		// The catalog vouches for the tool its exact name resolves to, not for
		// whatever file sits at a path — `./cat` or `bin/git` may be a
		// workspace script that only borrows a catalogued basename — nor for
		// another spelling a case-sensitive filesystem may resolve elsewhere
		// (`LS`). The deny side folds case; proof does not.
		program := fields[0]
		rule, ok := observationRuleByProgram[program]
		// credentialSafe is per program; filterReadsOnlyStdin judges the
		// invocation, releasing a filter that provably opened no file. It only
		// widens, and fails closed on anything it cannot parse.
		if !ok || (credentialed && !rule.credentialSafe && !filterReadsOnlyStdin(program, fields[1:]) && !(program == "set" && credentialSafeSet(fields[1:]))) {
			return false
		}
		commandArgs, ok := observationCommandArgs(program, fields[1:])
		if !ok || !rule.matches(commandArgs) || (credentialed && program == "jq" && !credentialSafeJQ(commandArgs)) {
			return false
		}
	}
	return true
}

// ghObservationSafe rejects a `gh api` call that is not provably a read. Every
// other gh subcommand in the catalog names its own read-only operation, so only
// `api` needs the check. gh's own grammar decides (ghAPIRequest): an
// unresolvable verb or option is a mutation, a request body disqualifies the
// call whatever its verb, and another host may receive an enterprise token.
func ghObservationSafe(args []string) bool {
	if len(args) == 0 || strings.ToLower(strings.Trim(strings.TrimSpace(args[0]), `"'`)) != "api" {
		return true
	}
	call, ok := ghAPIRequest(args[1:])
	if !ok || call.body || call.host {
		return false
	}
	_, read := apiReadMethods[call.method]
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

// credentialSafeSet allows shell option-setting invocations such as `set -e`,
// `set -u`, `set -o pipefail`, or `set -euo pipefail` in credentialed
// observations while keeping bare `set` or `set -o` / `set +o` (which dump
// shell variables or option state) and positional assignments (`set -- ...`)
// gated.
func credentialSafeSet(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') || arg == "--" || arg == "++" {
			return false
		}
		for j := 1; j < len(arg); j++ {
			ch := arg[j]
			if ch == 'o' {
				opt := ""
				if j+1 < len(arg) {
					opt = arg[j+1:]
					j = len(arg)
				} else {
					if i+1 >= len(args) {
						return false
					}
					i++
					opt = strings.TrimSpace(args[i])
				}
				if !safeShellSetOption(opt) {
					return false
				}
				continue
			}
			switch ch {
			case 'e', 'u', 'f', 'n', 'C', 'E', 'H', 'P', 'T', 'B', 'h', 'm', 'b':
			default:
				return false
			}
		}
	}
	return true
}

func safeShellSetOption(opt string) bool {
	switch strings.ToLower(strings.TrimSpace(opt)) {
	case "errexit", "nounset", "pipefail", "noglob", "noexec",
		"noclobber", "errtrace", "functrace", "physical", "posix", "notify",
		"braceexpand", "hashall", "histexpand", "history", "ignoreeof", "interactive-comments",
		"monitor", "onecmd":
		return true
	default:
		return false
	}
}

var observationGlobalValueFlags = map[string]map[string]struct{}{
	"git": {"-C": {}},
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
	"gh":     {"-R": {}, "--repo": {}},
	"argocd": {"--config": {}, "--controller-name": {}, "--grpc-web-root-path": {}, "--server": {}},
}

var observationGlobalBoolFlags = map[string]map[string]struct{}{
	"git":     {"--no-pager": {}, "--no-optional-locks": {}, "--literal-pathspecs": {}},
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
		// Short value flags may attach their value (-Crepo, -Rowner/repo).
		// This applies only before the subcommand; diff -C is a different flag.
		if len(name) > 2 && !strings.HasPrefix(name, "--") {
			if _, known := valueFlags[name[:2]]; known {
				name = name[:2]
				hasAttachedValue = true
			}
		}
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
	var ok bool
	args, ok = observationCommandArgs(r.program, args)
	if !ok {
		return false
	}
	// All consumers, including prefix plumbing and unattended watchers, use
	// the same argument proof. A prefix match alone cannot drop this check.
	if r.verify != nil && !r.verify(args) {
		return false
	}
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

func commandLookupArgs(args []string) bool {
	hasLookupFlag := false
	hasOperand := false
	afterSep := false
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		if afterSep {
			hasOperand = true
			continue
		}
		if arg == "--" {
			afterSep = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if len(arg) < 2 || strings.HasPrefix(arg, "--") {
				return false
			}
			for _, ch := range arg[1:] {
				switch ch {
				case 'v', 'V':
					hasLookupFlag = true
				case 'p':
				default:
					return false
				}
			}
			continue
		}
		hasOperand = true
	}
	return hasLookupFlag && hasOperand
}
