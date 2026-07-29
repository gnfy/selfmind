package tools

import (
	"fmt"
	"path/filepath"
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
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {}, "fish": {}, "csh": {}, "tcsh": {},
	"python": {}, "python2": {}, "python3": {}, "node": {}, "nodejs": {}, "deno": {}, "bun": {},
	"perl": {}, "ruby": {}, "php": {}, "lua": {}, "rscript": {}, "osascript": {},
	"powershell": {}, "pwsh": {}, "cmd": {}, "awk": {}, "gawk": {}, "sed": {},
	// Wrappers and evaluation facilities.
	"eval": {}, "exec": {}, "source": {}, ".": {}, "env": {}, "sudo": {}, "doas": {}, "su": {},
	"nohup": {}, "setsid": {}, "timeout": {}, "nice": {}, "ionice": {}, "stdbuf": {}, "xargs": {},
	// General-purpose execution through a non-obvious door.
	"git": {}, "find": {}, "make": {},
	// Irreversible by nature: a standing permission for these cannot be
	// undone by inspecting the next invocation. Ordinary dangerous operations
	// (chmod, chown, mv, kill, ...) deliberately stay OUT of this set: they
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
// grant. A redirection, substitution, expansion or wildcard means the tokens
// the class was derived from are not the tokens that will run, so the class
// cannot bound the command. Same rule codex applies: commands using these
// features are never evaluated against remembered rules.
var grantComplexShellMarkers = []string{">", "<", "$", "`", "*", "?"}

// HostEscapeApprovalReason is the dangerous-op reason recorded for a command
// that asks to run outside the sandbox. It is exported because a persisted
// grant key embeds it, and reviewing old keys must match on the same string
// rather than a copy that can drift.
const HostEscapeApprovalReason = "requests execution on the host outside the isolated sandbox"

// grantKeyResourceMarker and grantKeyCommandMarker are the structural parts of
// a host-execution grant key: "exec:<reason>|resource=workspace:<hash>:command:<family>".
const (
	grantKeyResourceMarker = "|resource="
	grantKeyCommandMarker  = ":command:"
)

// DescribeGrantClass renders what a "remember this" decision would authorize,
// or "" when the payload cannot back a reusable grant. The result is shown to
// the user at decision time, so it must name the class in plain words and must
// never contain the command text, a path, or a fingerprint.
func DescribeGrantClass(toolName, dangerousReason string, args map[string]interface{}) string {
	if !isExecTool(toolName) {
		return patternReasonBucket(dangerousReason)
	}
	family, eligible := grantCommandFamily(toolName, args)
	if !eligible {
		return ""
	}
	if effectiveSandboxModeArg(args) == SandboxHost {
		return fmt.Sprintf("host execution of %q in this workspace", family)
	}
	if strings.TrimSpace(dangerousReason) != "" {
		return dangerousReason
	}
	return fmt.Sprintf("%q commands", family)
}

// ReviewPersistedGrantKey decides whether a grant key that is already stored
// remains eligible under the current floor. It exists because the floor was
// introduced after grants had been recorded: keys minted by the old
// first-token derivation (`command:set`, `command:for`) and keys from before
// host grants were workspace-scoped at all must be withdrawn, while legitimate
// command families stay. family is returned for reporting and may be empty.
func ReviewPersistedGrantKey(patternKey string) (family string, keep bool) {
	key := strings.TrimSpace(patternKey)
	if key == "" {
		return "", false
	}
	if !strings.HasPrefix(key, "exec:") {
		// Write/path tool classes bucket by reason and carry no command family.
		return "", true
	}
	hostEscape := strings.Contains(key, HostEscapeApprovalReason)
	resourceIdx := strings.Index(key, grantKeyResourceMarker)
	if resourceIdx < 0 {
		// A host escape with no workspace/command resource authorises every
		// command for the person — exactly the shape the resource fingerprint
		// was introduced to prevent.
		return "", !hostEscape
	}
	resource := key[resourceIdx+len(grantKeyResourceMarker):]
	commandIdx := strings.LastIndex(resource, grantKeyCommandMarker)
	if commandIdx < 0 {
		return "", false
	}
	family = strings.ToLower(strings.TrimSpace(resource[commandIdx+len(grantKeyCommandMarker):]))
	return family, IsGrantableCommandFamily(family)
}

// IsGrantableCommandFamily reports whether a command family may back a reusable
// grant. It is the same floor grantCommandFamily applies when minting a key,
// exposed so already-persisted keys can be re-checked.
func IsGrantableCommandFamily(family string) bool {
	family = strings.ToLower(strings.TrimSpace(family))
	if family == "" || family == "unknown" {
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

// grantCommandFamily derives the reusable class for an exec payload, or
// ("", false) when no reusable grant may be created. The boolean is the floor:
// callers must fall back to a one-time approval when it is false.
func grantCommandFamily(toolName string, args map[string]interface{}) (string, bool) {
	// execute_code runs a model-authored program. There is no class narrower
	// than "arbitrary code", so it is approved per call and never remembered.
	if strings.EqualFold(strings.TrimSpace(toolName), "execute_code") {
		return "", false
	}
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
	if unparsed {
		// A wrapper whose payload could not be read cannot be classified.
		return "", false
	}
	family := ""
	for _, fields := range segments {
		progIdx, ok := segmentProgram(fields)
		if !ok {
			// Only environment assignments: the class would be derived from
			// tokens that never execute.
			return "", false
		}
		base := strings.ToLower(strings.TrimSpace(filepath.Base(fields[progIdx])))
		if base == "" {
			return "", false
		}
		if _, control := shellControlKeywords[base]; control {
			return "", false
		}
		if _, neutral := shellNeutralWords[base]; neutral {
			continue
		}
		if _, banned := bannedGrantPrograms[base]; banned {
			return "", false
		}
		if family == "" {
			family = base
			continue
		}
		if family != base {
			// Several distinct programs in one payload: no single family
			// describes what a grant would authorise.
			return "", false
		}
	}
	if family == "" {
		return "", false
	}
	return family, true
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
