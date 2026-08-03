package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// Tool failure classification.
//
// A failed tool call reaches the model as the error text alone, so the model
// otherwise re-diagnoses every failure from scratch. ClassifyToolError maps a
// failure onto a coarse class from conservative substring/pattern cues, and
// enrichToolFailure appends one compact `error_class: <class>; hint: ...` line
// to the error. The raw error text is always preserved; enrichment only
// appends. This runs inside the tool implementations, below the approval
// middleware, so user-rejection and safety-block errors are never rewritten
// (their exact wording is a cross-package contract with the kernel).

// errorClassRule associates one class with the lowercase substrings and
// optional regexps that identify it. Rules are evaluated in order and the
// first match wins, so more specific classes come first.
type errorClassRule struct {
	class      string
	substrings []string
	regexps    []*regexp.Regexp
}

var errorClassRules = []errorClassRule{
	{
		class: "timeout",
		substrings: []string{
			"context deadline exceeded",
			"deadline exceeded",
			"timed out",
		},
	},
	{
		// Before the generic auth class: an expired session needs a refresh,
		// not a re-login, and never a host escalation.
		class: "credential_expired",
		substrings: []string{
			"token expired",
			"token_expired",
			"credentials expired",
			"credential expired",
			"invalid_grant",
			"reauthentication required",
			"session expired",
			"refresh token is invalid",
		},
	},
	{
		// Before the generic auth class: "there is no credential" is a setup
		// problem with a different fix than "the credential was rejected".
		// A read-only credential STATE directory is deliberately NOT here —
		// that is a sandbox mapping gap (classifySandboxDenial) and mixing the
		// two is what sent the model chasing auth state for a denied write.
		class: "credential_missing",
		substrings: []string{
			"no credentials",
			"could not read credentials",
			"unable to locate credentials",
			"not logged in",
			"login required",
			"could not read username",
			"no active account",
			"do not currently have an active account",
		},
	},
	{
		// Before "permission": ssh's "Permission denied (publickey)" is an
		// auth failure, not a filesystem permission problem.
		class: "auth",
		substrings: []string{
			"unauthorized",
			"forbidden",
			"authentication failed",
			"authentication required",
			"invalid credential",
			"bad credentials",
			"invalid api key",
			"permission denied (publickey",
		},
		regexps: []*regexp.Regexp{
			regexp.MustCompile(`\b(status(?: code)?|http|error|code) 40[13]\b`),
			regexp.MustCompile(`\b40[13]\b.{0,20}\b(unauthorized|forbidden)\b`),
		},
	},
	{
		class: "syntax",
		substrings: []string{
			"syntax error",
			"parse error",
			"unexpected token",
			"unexpected end of file",
			"unterminated quoted string",
			"bad substitution",
			"pipefail",
			"illegal option",
		},
	},
	{
		class: "permission",
		substrings: []string{
			"permission denied",
			"operation not permitted",
			"access is denied",
			"access denied",
			"read-only file system",
		},
	},
	{
		class: "network",
		substrings: []string{
			"connection refused",
			"connection reset",
			"no such host",
			"network is unreachable",
			"could not resolve host",
			"temporary failure in name resolution",
			"tls handshake",
			"x509",
			"dial tcp",
			"dial udp",
		},
	},
	{
		// Before "not_found": missing interpreter/package/env-var cues are an
		// environment setup problem, not a bad path in the command.
		class: "environment",
		substrings: []string{
			"no module named",
			"modulenotfounderror",
			"cannot find module",
			"cannot find package",
			"is not installed",
			"environment variable",
			"unbound variable",
		},
	},
	{
		class: "not_found",
		substrings: []string{
			"command not found",
			"executable file not found",
			"no such file or directory",
			"no such file",
			"does not exist",
			"not found",
		},
	},
}

// errorClassHints maps each class to one short actionable next step. Hints
// must stay generic: no project-specific environment overrides, and no words
// that collide with the kernel/evidence error-text contracts ("rejected",
// "cancelled by user", "denied", "blocked", "approval").
var errorClassHints = map[string]string{
	"syntax":                    "The shell rejected the command syntax; simplify quoting or run via bash -c.",
	"auth":                      "Authentication or credentials failed; fix auth state first instead of repeating the same call.",
	"credential_missing":        "No credential is present for this tool; complete its normal login on the host instead of retrying the same call.",
	"credential_expired":        "The credential exists but is expired; refresh the session instead of re-running the same call.",
	"sandbox_fs_denied":         "The isolated sandbox denied a write outside the writable roots; write to a path inside the workspace roots instead. Host execution is not the fix for a denied write.",
	"credential_state_readonly": "The tool needs to write its OWN state or config directory, which is read-only inside the sandbox. This is a sandbox state-mapping gap, not an authentication failure: do not re-run the same command and do not treat it as a login problem.",
	"timeout":                   "The operation exceeded its time limit; narrow the scope, raise the timeout, or run it in the background.",
	"not_found":                 "Check cwd and that the executable/file exists before retrying.",
	"permission":                "The current user lacks permission for this path or operation; choose an accessible target instead of retrying as-is.",
	"network":                   "A network or TLS connection failed; verify host reachability and the endpoint before retrying.",
	"environment":               "A required interpreter, package, or environment variable is missing; set up the environment before retrying.",
	"unknown":                   "Read the error and captured output to identify a cause before changing the next command.",
}

// ClassifyToolError returns a coarse failure class for a failed tool call:
// one of "syntax", "auth", "timeout", "not_found", "permission", "network",
// "environment", or "unknown". It is a pure function over the error text and
// the captured output; toolName is accepted so future rules can specialize
// per tool without changing callers.
func ClassifyToolError(toolName string, err error, output string) string {
	_ = toolName
	var sb strings.Builder
	if err != nil {
		sb.WriteString(err.Error())
		sb.WriteByte('\n')
	}
	sb.WriteString(output)
	text := strings.ToLower(sb.String())
	if strings.TrimSpace(text) == "" {
		return "unknown"
	}
	for _, rule := range errorClassRules {
		for _, sub := range rule.substrings {
			if strings.Contains(text, sub) {
				return rule.class
			}
		}
		for _, re := range rule.regexps {
			if re.MatchString(text) {
				return rule.class
			}
		}
	}
	return "unknown"
}

// enrichToolFailure appends the compact classification line to a tool failure
// so the model can pick a corrected next step without a re-diagnosis turn.
// The original error text and wrap chain are preserved unchanged.
func enrichToolFailure(toolName string, err error, output string) error {
	if err == nil {
		return nil
	}
	class := ClassifyToolError(toolName, err, output)
	return fmt.Errorf("%w\nerror_class: %s; hint: %s", err, class, errorClassHints[class])
}

// enrichIsolatedSandboxFailure requests a host retry only when the failure
// actually points to host-only credentials, networking, or environment state.
// Syntax and path mistakes stay on the normal correction path so they cannot
// become repeated approval requests.
// isolatedSandboxTimeoutHint is appended to a command-timeout failure that ran
// inside the isolated sandbox. Timeouts bypass the error classifier (they have
// no diagnostic stderr), so without this the ONE failure shape a network-less
// sandbox most often produces — a retrying client (gRPC, kubectl) disguising
// instant connect failures as a hang — carried zero sandbox context and read
// as "the network is slow" (observed live against an IP-allowlisted ArgoCD).
// Empty when the sandbox shares the host network: a timeout there is a real
// timeout and must not be blamed on the sandbox.
func isolatedSandboxTimeoutHint(decision SandboxDecision) string {
	if decision.Mode != SandboxIsolated {
		return ""
	}
	if decision.NetworkShared {
		return ""
	}
	return "\nsandbox_context: isolated network-disabled; hint: The command timed out while isolated. Inspect the captured output before deciding whether it needs network:shared, different credentials, or a longer timeout."
}

func enrichSandboxTimeout(toolName string, err error, output string, decision SandboxDecision) error {
	if err == nil {
		return nil
	}
	if hint := isolatedSandboxTimeoutHint(decision); hint != "" {
		return fmt.Errorf("%w%s", err, hint)
	}
	return enrichToolFailure(toolName, err, output)
}

// sandboxWriteDenialSignals are decisive evidence that the ISOLATED filesystem
// view rejected a write. They are only consulted for a command that actually
// ran inside the sandbox: "was there a sandbox" is the precondition, exactly as
// codex gates its own denial heuristic. Without that precondition a flat rule
// table lets an unrelated cue win — the live failure mode was
// `credentials.db` matching an auth rule, so a denied write was reported as
// "fix auth state first" together with "host execution is not the fix",
// closing off the ONLY correct remedy.
var sandboxWriteDenialSignals = []string{
	"read-only file system",
	"operation not permitted",
	"configuration directory may not be writable",
	"unable to create private file",
	"failed to write file",
}

// sandboxAuthShapedSignals mark a failure that merely mentions a permission
// word while actually being an auth rejection (ssh publickey, HTTP 401/403).
// They veto the weak "permission denied" signal.
var sandboxAuthShapedSignals = []string{
	"publickey", "unauthorized", "forbidden", " 401", " 403", "(403", "(401",
}

// credentialStatePathMarkers identify a denied write that landed on a tool's
// own credential/state directory rather than on arbitrary user data. The two
// cases need different remedies: a state directory needs a sandbox state
// mapping, an arbitrary path needs a different target.
var credentialStatePathMarkers = []string{
	"credential", "token", "/.config/", "/.aws", "/.kube", "/.docker",
	"/.gnupg", "/.ssh", "/.cache/", "/.local/state", "config directory",
	"configuration directory",
}

// sandboxDenialQuickRejectExitCodes are shell-level rejections (usage error,
// not executable, not found) that a sandbox never produces.
var sandboxDenialQuickRejectExitCodes = map[int]struct{}{2: {}, 126: {}, 127: {}}

// classifySandboxDenial reports whether an ISOLATED command failed because the
// sandbox filesystem view denied a write, and which of the two denial classes
// it is. Callers must only invoke it for commands that really ran isolated.
func classifySandboxDenial(exitCode int, err error, output string) (string, bool) {
	if _, quickReject := sandboxDenialQuickRejectExitCodes[exitCode]; quickReject {
		return "", false
	}
	var sb strings.Builder
	if err != nil {
		sb.WriteString(err.Error())
		sb.WriteByte('\n')
	}
	sb.WriteString(output)
	text := strings.ToLower(sb.String())
	if strings.TrimSpace(text) == "" {
		return "", false
	}

	denied := false
	for _, signal := range sandboxWriteDenialSignals {
		if strings.Contains(text, signal) {
			denied = true
			break
		}
	}
	if !denied && strings.Contains(text, "permission denied") {
		denied = true
		for _, veto := range sandboxAuthShapedSignals {
			if strings.Contains(text, veto) {
				denied = false
				break
			}
		}
	}
	if !denied {
		return "", false
	}
	for _, marker := range credentialStatePathMarkers {
		if strings.Contains(text, marker) {
			return "credential_state_readonly", true
		}
	}
	return "sandbox_fs_denied", true
}

func enrichIsolatedSandboxFailure(toolName string, exitCode int, err error, output string) error {
	if err == nil {
		return nil
	}
	// Sandbox write denial is decided FIRST: it is the one class whose remedy
	// the generic taxonomy actively contradicts.
	if class, ok := classifySandboxDenial(exitCode, err, output); ok {
		return fmt.Errorf("%w\nerror_class: %s; hint: %s", err, class, errorClassHints[class])
	}
	class := ClassifyToolError(toolName, err, output)
	switch class {
	case "network":
		return fmt.Errorf("%w\nerror_class: sandbox_no_network; hint: The isolated command needs network access. Request the workspace-scoped network:shared capability; do not switch to host execution.", err)
	case "auth", "credential_missing", "credential_expired", "environment":
		return fmt.Errorf("%w\nerror_class: %s; hint: %s Host execution is not an authentication or setup fix.", err, class, errorClassHints[class])
	default:
		return fmt.Errorf("%w\nerror_class: %s; hint: %s", err, class, errorClassHints[class])
	}
}
