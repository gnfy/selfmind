package tools

import (
	"fmt"
	"strings"
)

// Reusable-grant floor.
//
// An approval decision may be REMEMBERED as a class ("approve this kind of
// thing for this task / for me"). That memory is only safe when the class it
// keys on actually bounds what may run. Two live failures showed it did not:
// the class was derived from the FIRST token of the payload, so
// `set -euo pipefail\n<anything>` keyed as `command:set` and
// `for t in ...; do ...` keyed as `command:for` — a person-scope host-execution
// grant for "every script that begins with set -euo pipefail".
//
// This file is the floor that decides whether a reusable grant may exist at
// all. It is deliberately source-level data, not configuration: it defines
// behaviour, not preference (codex keeps the equivalent
// BANNED_PREFIX_SUGGESTIONS table in source for the same reason). The floor
// runs before any approval mode or existing grant and can only ever REMOVE
// grant eligibility, never add it.

// bannedGrantPrograms are programs whose invocation cannot be summarised by a
// reusable class, because the program itself runs whatever it is handed:
// interpreters and shells, tools with a general-purpose exec facility
// (git hooks/aliases/-c, find -exec, xargs), and irreversible operations.
// Approving one of these once is a decision about that one command; it must
// never become a standing permission.
var bannedGrantPrograms = map[string]struct{}{
	// Shells and interpreters: equivalent to arbitrary code execution.
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {}, "mksh": {}, "ash": {}, "hush": {}, "fish": {}, "csh": {}, "tcsh": {},
	"python": {}, "python2": {}, "python3": {}, "node": {}, "nodejs": {}, "deno": {}, "bun": {},
	"perl": {}, "ruby": {}, "php": {}, "lua": {}, "rscript": {}, "osascript": {},
	"powershell": {}, "pwsh": {}, "cmd": {}, "awk": {}, "gawk": {}, "sed": {},
	// Wrappers and evaluation facilities.
	"eval": {}, "exec": {}, "source": {}, ".": {}, "env": {}, "sudo": {}, "doas": {}, "su": {},
	"nohup": {}, "setsid": {}, "timeout": {}, "nice": {}, "ionice": {}, "stdbuf": {}, "xargs": {},
	// General-purpose execution through a non-obvious door.
	"git": {}, "find": {}, "make": {},
	// Arbitrary network clients: the method, the target and the output file are
	// all arguments, so no leading token bounds what a standing permission
	// would authorise. socat's EXEC and SYSTEM addresses run any program.
	"curl": {}, "wget": {}, "httpie": {}, "http": {}, "socat": {},
	// Same shape without the network: xxd's output path is a bare positional
	// operand, so a standing `xxd` would authorise writing anywhere.
	"xxd": {},
	// Irreversible by nature: a standing permission for these cannot be
	// undone by inspecting the next invocation. Ordinary dangerous operations
	// (chmod, chown, kill, ...) deliberately stay OUT of this set: they
	// remain approvable classes, which is the documented distinction between
	// the hard floor and ordinary approval.
	"rm": {}, "rmdir": {}, "dd": {}, "mkfs": {}, "shred": {}, "wipefs": {}, "fdisk": {},
	"shutdown": {}, "reboot": {}, "halt": {}, "poweroff": {},
}

// shellNeutralWords are prologue builtins that carry no execution authority of
// their own. They are skipped when deriving the command family so that a
// leading `set -euo pipefail` does not become the remembered class. Anything
// capable of running code belongs in bannedGrantPrograms, and anything that
// changes control flow belongs in shellControlKeywords — never here.
var shellNeutralWords = map[string]struct{}{
	"set": {}, "shift": {}, "trap": {}, "echo": {}, "printf": {}, "true": {}, "false": {},
	":": {}, "test": {}, "[": {}, "[[": {}, "]": {}, "]]": {}, "cd": {}, "pwd": {},
	"export": {}, "local": {}, "declare": {}, "readonly": {}, "unset": {}, "umask": {},
	"wait": {}, "sleep": {}, "read": {},
}

// shellControlKeywords mark structural control flow. A payload containing one
// is complex shell: the programs actually executed depend on runtime state, so
// no single family can bound a standing permission. Failing closed here is
// deliberate — it costs one extra approval prompt and removes a whole class of
// over-broad grant.
var shellControlKeywords = map[string]struct{}{
	"for": {}, "do": {}, "done": {}, "if": {}, "then": {}, "else": {}, "elif": {}, "fi": {},
	"while": {}, "until": {}, "case": {}, "esac": {}, "select": {}, "function": {},
	"break": {}, "continue": {}, "return": {}, "{": {}, "}": {},
}

// grantComplexShellMarkers disqualify a payload from producing a reusable
// grant. A redirection, substitution, expansion, wildcard or escape means the
// tokens the class was derived from are not the tokens that will run, so the
// class cannot bound the command: `\python3` is python3 and `-\X DELETE` is
// `-X DELETE`. Same rule codex applies: commands using these features are
// never evaluated against remembered rules.
var grantComplexShellMarkers = []string{">", "<", "$", "`", "*", "?", `\`}

// grantDeviceMarkers name raw block devices — every disk prefix the floor
// knows, the macOS whole-disk nodes included — and the shell's network
// pseudo-devices. A payload naming one is dangerous for what it touches, not
// for which program touches it, so the program's class cannot bound it: a
// remembered `smartctl --scan` must not release `smartctl -t long /dev/sda`.
var grantDeviceMarkers = append(append(append([]string{}, destructiveSubstrings...), egressSubstrings...),
	hardlineDiskDevices...)

// HostEscapeApprovalReason is the dangerous-op reason recorded for a command
// that asks to run outside the sandbox. It is exported because a persisted
// grant key embeds it, and reviewing old keys must match on the same string
// rather than a copy that can drift.
const HostEscapeApprovalReason = "requests execution on the host outside the isolated sandbox"

// The structural parts of an exec grant key. Both shapes name the command
// class the floor derived and nothing else:
//
//	host:      exec:<HostEscapeApprovalReason>|resource=workspace:<hash>:command:<class>
//	sandboxed: exec:<tool>|class=<class>
//
// The host shape keeps the host-escape text as a fixed marker so classes
// remembered before it was fixed stay valid; it is never the reason the call
// was asked about.
const (
	hostGrantKeyBase       = "exec:" + HostEscapeApprovalReason
	grantKeyResourceMarker = "|resource="
	grantKeyCommandMarker  = ":command:"
	grantKeyClassMarker    = "|class="
)

// DescribeGrantClass renders what a "remember this" decision would authorize,
// or "" when the payload cannot back a reusable grant. The result is shown to
// the user at decision time, so it must name the class in plain words and must
// never contain the command text, a path, or a fingerprint. For an exec call it
// names the class the key is built from, never the reason the call asked: a
// label naming the mode while the key covered the whole tool is how one
// remembered read released every later command.
func DescribeGrantClass(toolName, dangerousReason string, args map[string]interface{}) string {
	if !isExecTool(toolName) {
		return patternReasonBucket(dangerousReason)
	}
	prefix, eligible := grantCommandPrefix(toolName, args)
	if !eligible {
		return ""
	}
	family := grantPrefixLabel(prefix)
	if effectiveSandboxModeArg(args) == SandboxHost {
		return fmt.Sprintf("host execution of %q in this workspace", family)
	}
	return fmt.Sprintf("%q commands", family)
}

// ReviewPersistedGrantKey decides whether a grant key that is already stored
// remains eligible under the current floor. It exists because the floor was
// introduced after grants had been recorded: keys minted by the old
// first-token derivation (`command:set`, `command:for`), keys from before host
// grants were workspace-scoped at all, and sandboxed keys built from the
// approval reason (`exec:terminal requires approval in on-request mode`,
// `exec:terminal`), which named a tool rather than a class, must be withdrawn,
// while legitimate command classes stay. A stored key is never rewritten to its
// normalized form, which could match more than the person approved: an exec
// key the current derivation would not mint is withdrawn. family is returned
// for reporting and may be empty.
func ReviewPersistedGrantKey(patternKey string) (family string, keep bool) {
	// A binding to in-place credential writes only narrows a key; the class it
	// binds is what faces the floor.
	key, _, _ := strings.Cut(strings.TrimSpace(patternKey), GrantKeyHostWritesMarker)
	if key == "" {
		return "", false
	}
	if !strings.HasPrefix(key, "exec:") {
		// Write/path tool classes bucket by reason and carry no command family.
		return "", true
	}
	if base, resource, host := strings.Cut(key, grantKeyResourceMarker); host {
		commandIdx := strings.LastIndex(resource, grantKeyCommandMarker)
		if commandIdx < 0 {
			return "", false
		}
		family = strings.ToLower(strings.TrimSpace(resource[commandIdx+len(grantKeyCommandMarker):]))
		// A host key minted under any other reason, or naming a program that is
		// dangerous for its own sake, is one the derivation no longer produces
		// on the host and nothing redeems.
		return family, base == hostGrantKeyBase && IsGrantableCommandFamily(family) && !hostDangerousFamily(family)
	}
	if tool, class, sandboxed := strings.Cut(strings.TrimPrefix(key, "exec:"), grantKeyClassMarker); sandboxed {
		family = strings.ToLower(strings.TrimSpace(class))
		return family, isExecTool(tool) && IsGrantableCommandFamily(family)
	}
	// Anything else under exec: names a tool, a mode or a reason instead of a
	// class — including a host escape with no workspace/command resource, which
	// authorises every command for the person.
	return "", false
}

// IsGrantableCommandFamily reports whether a command family may back a reusable
// grant. It is the same floor grantCommandPrefix applies when minting a key,
// exposed so already-persisted keys can be re-checked.
//
// A key minted before prefixes carries one token; a current one carries the
// whole prefix. Only the leading program decides eligibility — the trailing
// subcommand words narrow a class, they never rescue a banned one.
func IsGrantableCommandFamily(family string) bool {
	family = strings.ToLower(strings.TrimSpace(family))
	// An escape never survives derivation today; a family holding one was
	// minted from a misread command.
	if family == "" || family == "unknown" || strings.Contains(family, `\`) {
		return false
	}
	if head, _, multi := strings.Cut(family, " "); multi {
		family = strings.TrimSpace(head)
	}
	if programName(family) != family {
		return false
	}
	if _, banned := bannedGrantPrograms[family]; banned {
		return false
	}
	if _, control := shellControlKeywords[family]; control {
		return false
	}
	if _, neutral := shellNeutralWords[family]; neutral {
		return false
	}
	return true
}

// hostDangerousFamily reports a class whose program is dangerous for its own
// sake (chmod, kill, ssh): the dangerous heuristic always flags it, so on the
// host it is a once-only decision and never a class.
func hostDangerousFamily(family string) bool {
	head, _, _ := strings.Cut(family, " ")
	_, dangerous := dangerousBinaries[head]
	_, egress := egressBinaries[head]
	return dangerous || egress
}

// grantClassForDecision reports the class a "remember this" decision would
// create for THIS call. It returns "" whenever no reusable key was minted, so a
// surface can never tell the user that something was remembered when the floor
// refused to persist it.
func grantClassForDecision(toolName, dangerousReason string, args map[string]interface{}, patternKey string) string {
	if strings.TrimSpace(patternKey) == "" {
		return ""
	}
	return DescribeGrantClass(toolName, dangerousReason, args)
}
