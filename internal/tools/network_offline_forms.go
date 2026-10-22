package tools

import (
	"regexp"
	"slices"
	"strings"

	"selfmind/internal/tools/envprofiles"
)

// Explicit offline and local forms.
//
// A command the table names as reaching a remote service is not one when its own
// words say otherwise: the program was told to use no network (`npm install
// --offline`, `pip install --no-index`), its remote is a path on this machine
// (`git clone ../repo`), or it was only asked for its help or version. Sharing
// the daemon's network with such a call would hand a permission to a command
// that has declared it does not use it.
//
// The veto is about the invocation it appears in. It reads each program of a
// compound command on its own, so one offline segment never hides a remote
// one, and it never applies when the invocation names a remote target: a URL or
// a user@host:path is the remote the command was going to reach, whatever else
// it says. It decides only whether the network is NEEDED. The classifier that
// gates dangerous egress (egressIn, dangerousToolCall) is not touched: `curl
// --version` is still an egress program to the approval funnel, which is the
// conservative reading for a safety gate.
//
// An explicit `network: true` is never vetoed. That request is the model's own
// statement that this call needs the network, and permission decides.

// scpTarget is the user@host:path spelling of an SSH remote.
var scpTarget = regexp.MustCompile(`^[A-Za-z0-9._~-]+@[A-Za-z0-9._-]+:`)

// namesRemoteTarget reports a remote among an invocation's arguments: a URL
// with a scheme other than file, or a user@host:path. Option values count
// (`--registry=https://...`): a flag that points the program at a remote host
// is the remote it will reach.
func namesRemoteTarget(args []string) bool {
	for _, arg := range args {
		candidate := strings.TrimSpace(arg)
		if strings.HasPrefix(candidate, "-") {
			_, value, attached := strings.Cut(candidate, "=")
			if !attached {
				continue
			}
			candidate = value
		}
		lower := strings.ToLower(candidate)
		if strings.Contains(lower, "://") && !strings.HasPrefix(lower, "file://") {
			return true
		}
		if scpTarget.MatchString(candidate) {
			return true
		}
	}
	return false
}

// isLocalRemote reports a remote that is a path on this machine.
func isLocalRemote(operand string) bool {
	switch operand {
	case ".", "..", "~":
		return true
	}
	return strings.HasPrefix(operand, "./") || strings.HasPrefix(operand, "../") || strings.HasPrefix(operand, "/") ||
		strings.HasPrefix(operand, "~/") || strings.HasPrefix(strings.ToLower(operand), "file://")
}

// remoteIsLocal reports whether a verb that takes its remote as the first
// operand was given a path on this machine and nothing that names a remote. The
// first operand is read past the verb's own options that take a value.
func (v networkVerb) remoteIsLocal(rule networkRule, args []string) bool {
	if !v.localRemote || namesRemoteTarget(args) || v.widened(args) {
		return false
	}
	operands := v.operands(rule, args)
	return len(operands) > 0 && isLocalRemote(operands[0])
}

// widened reports an option that makes the transfer reach beyond its first
// operand and is not switched off (`--recurse-submodules=no`).
func (v networkVerb) widened(args []string) bool {
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "--" {
			return false
		}
		option, value, attached := strings.Cut(arg, "=")
		if attached && switchValueIsOff(value) {
			continue
		}
		for _, flag := range v.widenedBy {
			if option == flag {
				return true
			}
		}
	}
	return false
}

// switchValueIsOff reads the value of a boolean option in the spellings that
// turn it off: false, 0, no, in any case.
func switchValueIsOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "no":
		return true
	}
	return false
}

// operands are the words after the verb's subcommand path, with the program's
// global options and the verb's own options that take a value skipped.
func (v networkVerb) operands(rule networkRule, args []string) []string {
	var operands []string
	path := 0
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--":
			return append(operands, args[i+1:]...)
		case arg == "":
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			name, _, attached := strings.Cut(arg, "=")
			if attached {
				continue
			}
			values := v.valueOptions
			if path < len(v.path) {
				values = rule.valueOptions
			}
			if _, takesValue := values[name]; takesValue {
				i++
			}
		case rule.toolchainWords && strings.HasPrefix(arg, "+") && path == 0:
		case path < len(v.path):
			path++
		default:
			operands = append(operands, arg)
		}
	}
	return operands
}

// offlineForm reports whether one invocation is an explicit offline or local
// form of its program: it asked for its help or version, or it carries one of
// the program's own strictly-offline switches, and it names no remote target.
// (A verb whose remote is a local path is decided by the verb: remoteIsLocal.)
func offlineForm(name string, args []string) bool {
	if namesRemoteTarget(args) {
		return false
	}
	if pythonProgram(name) {
		// `python -m pip install --no-index` is pip's offline form.
		module, rest, ok := pythonModuleInvocation(args)
		if !ok {
			return false
		}
		if _, known := pythonModuleRules[module]; !known {
			return false
		}
		return offlineForm(module, rest)
	}
	rule, hasRule := networkRuleFor(name)
	offline := wordSet(offlineFlagsFor(name)...)
	operands, first := 0, ""
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--":
			i = len(args)
		case arg == "":
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			option, value, attached := strings.Cut(arg, "=")
			limit, runner := ownOptionLimit(rule, hasRule, operands, first)
			// A version request comes before the program has been given anything
			// to do. After the subcommand or an operand `--version` is an option
			// with a value (`cargo install ripgrep --version 13.0.0`).
			if operands == 0 && asksForVersion(option) {
				return true
			}
			if operands <= limit && asksForHelp(name, option) {
				return true
			}
			if _, isSwitch := offline[option]; isSwitch && (!runner || operands <= limit) {
				// `--offline=false`, `--offline false`: the switch given a false
				// value is not the switch.
				next := ""
				if !attached && i+1 < len(args) {
					next = args[i+1]
				}
				if !(attached && switchValueIsOff(value)) && !(next != "" && switchValueIsOff(next)) {
					return true
				}
			}
			if _, takesValue := rule.valueOptions[option]; hasRule && takesValue && !attached && operands == 0 {
				i++
			}
		case hasRule && rule.toolchainWords && strings.HasPrefix(arg, "+") && operands == 0:
		default:
			if operands == 0 {
				first = strings.ToLower(arg)
			}
			operands++
		}
	}
	return false
}

// ownOptionLimit is how many operands may precede an option for it still to be
// the program's own, and whether the program runs another one.
//
// A program with subcommands takes its own help flag after the subcommand
// (`kubectl get pods --help`). One without reads its options before its first
// operand, and what follows belongs to the command it runs (`ssh host df -h`).
// A runner (`npx pkg --offline`, `docker run alpine --help`) hands every option
// after the program it runs to that program: for `npx` that is anything after the
// first operand, for `docker run` anything after the word that follows `run`.
func ownOptionLimit(rule networkRule, hasRule bool, operands int, first string) (limit int, runner bool) {
	switch {
	case rule.runner:
		return 0, true
	case hasRule && operands >= 1 && slices.Contains(rule.runnerVerbs, first):
		return 1, true
	case hasRule:
		return 2, false
	}
	return 0, false
}

// asksForVersion reports an option that asks a program for its version.
func asksForVersion(option string) bool {
	return option == "--version" || option == "-V"
}

// asksForHelp reports an option that asks a program for its help. `-h` is help
// except where it names a host, a unit or a hostname.
func asksForHelp(program, option string) bool {
	switch option {
	case "--help":
		return true
	case "-h":
		_, other := shortHIsNotHelp[program]
		return !other
	}
	return false
}

// commandClearlyNeedsNetwork reports whether the command runs an explicit egress
// program (curl, ssh, scp, rsync, ...), or is a durable external watch. An
// invocation that only asks for the program's help or version does not count: the
// program is not asked to reach anything. The approval classifier (egressIn)
// reads the same programs without this exception.
func commandClearlyNeedsNetwork(toolName string, args map[string]interface{}) bool {
	if strings.EqualFold(strings.TrimSpace(toolName), "watch_external") {
		return true
	}
	command := strings.TrimSpace(execCommandPayload(toolName, args))
	if command == "" {
		return false
	}
	segments, _ := expandCommandSegments(command, 0)
	return egressOutsideOfflineForms(command, segments)
}

// egressOutsideOfflineForms is egressCommand with the invocations that are
// offline forms of their program set aside. The pseudo-device redirects
// (/dev/tcp/) are read from the whole payload, as they always are.
func egressOutsideOfflineForms(cmd string, segs [][]string) bool {
	commands, _ := shellCommands(cmd, 0)
	for _, fields := range segs {
		commands = append(commands, readCommand{id: identifyCommand(fields), fallback: true})
	}
	kept := make([]readCommand, 0, len(commands))
	for _, read := range commands {
		if program := read.id.program(); program.Name != "" && offlineForm(program.Name, program.Args) {
			continue
		}
		kept = append(kept, read)
	}
	hit, _ := egressIn(cmd, kept)
	return hit
}

// commandReachesCredentialedService reports whether the command runs a
// credential-bearing service CLI (gcloud, kubectl, aws, gh, docker, ...) other
// than to ask it for help or its version. It is commandNeedsOperatorCredentials
// for the question "does this reach a remote service", not for "does this need
// the person's credentials": the credential:read decision is unchanged.
func commandReachesCredentialedService(toolName string, args map[string]interface{}) bool {
	programs := execCommandPrograms(toolName, args)
	if len(programs) == 0 {
		return false
	}
	if toolName != "execute_code" {
		if offlineOnly := programsOnlyInOfflineForms(strings.TrimSpace(execCommandPayload(toolName, args))); len(offlineOnly) > 0 {
			kept := make([]string, 0, len(programs))
			for _, program := range programs {
				if _, skip := offlineOnly[program]; !skip {
					kept = append(kept, program)
				}
			}
			programs = kept
		}
	}
	for _, profile := range envprofiles.Match(programs) {
		if profile != nil && profile.CredentialAccess == envprofiles.CredentialAccessOperator {
			return true
		}
	}
	return false
}

// programsOnlyInOfflineForms names the programs a payload runs only as offline
// forms: every invocation of each is one.
func programsOnlyInOfflineForms(payload string) map[string]struct{} {
	if payload == "" {
		return nil
	}
	online := map[string]bool{}
	seen := map[string]bool{}
	for _, read := range preparationCommands(payload, 0) {
		program := read.id.program()
		if program.Name == "" {
			continue
		}
		seen[program.Name] = true
		if !offlineForm(program.Name, program.Args) {
			online[program.Name] = true
		}
	}
	out := map[string]struct{}{}
	for name := range seen {
		if !online[name] {
			out[name] = struct{}{}
		}
	}
	return out
}
