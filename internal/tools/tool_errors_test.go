package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		err    error
		output string
		want   string
	}{
		{"shell syntax error", "terminal", errors.New("command failed: exit status 2"), "sh: 1: Syntax error: Unterminated quoted string", "syntax"},
		{"pipefail unsupported", "terminal", errors.New("command failed: exit status 2"), "sh: 1: set: Illegal option -o pipefail", "syntax"},
		{"http 401 status", "web_extract", errors.New("request failed with status 401"), "", "auth"},
		{"unauthorized text", "terminal", nil, "error: unauthorized: authentication required", "auth"},
		{"missing credential", "terminal", errors.New("command failed: exit status 1"), "fatal: could not read credentials", "credential_missing"},
		{"gcloud no active account", "terminal", errors.New("command failed: exit status 1"), "ERROR: (gcloud.auth.list) You do not currently have an active account selected.", "credential_missing"},
		{"aws missing credentials", "terminal", errors.New("command failed: exit status 255"), "Unable to locate credentials. You can configure credentials by running \"aws configure\".", "credential_missing"},
		{"expired session", "terminal", errors.New("command failed: exit status 1"), "invalid_grant: Token has been expired or revoked.", "credential_expired"},
		// A read-only credential STATE directory must NOT be classified as an
		// auth problem by the generic taxonomy: that misdiagnosis is what the
		// sandbox-gated classifier exists to prevent.
		{"credential state write denial is not auth", "terminal", errors.New("command failed: exit status 1"),
			"ERROR: (gcloud.auth.list) Unable to create private file [/home/u/.config/gcloud/credentials.db]: [Errno 30] Read-only file system", "permission"},
		{"ssh publickey is auth not permission", "terminal", errors.New("command failed: exit status 255"), "git@github.com: Permission denied (publickey).", "auth"},
		{"context deadline", "search_files", errors.New("context deadline exceeded"), "", "timeout"},
		{"timed out message", "terminal", errors.New("command timed out after 30 seconds"), "", "timeout"},
		{"shell command not found", "terminal", errors.New("command failed: exit status 127"), "sh: 1: gofmtx: not found", "not_found"},
		{"missing file", "read_file", errors.New("open /tmp/missing.txt: no such file or directory"), "", "not_found"},
		{"filesystem permission", "read_file", errors.New("open /etc/shadow: permission denied"), "", "permission"},
		{"connection refused", "terminal", errors.New("dial tcp 127.0.0.1:9999: connect: connection refused"), "", "network"},
		{"dns failure", "terminal", errors.New("command failed: exit status 6"), "curl: (6) Could not resolve host: nope.invalid", "network"},
		{"missing python module", "terminal", errors.New("command failed: exit status 1"), "ModuleNotFoundError: No module named 'requests'", "environment"},
		{"missing env var", "terminal", errors.New("command failed: exit status 1"), "required environment variable API_BASE is empty", "environment"},
		{"unknown", "terminal", errors.New("command failed: exit status 1"), "boom", "unknown"},
		{"empty", "terminal", nil, "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyToolError(tc.tool, tc.err, tc.output); got != tc.want {
				t.Fatalf("ClassifyToolError(%q, %v, %q) = %q, want %q", tc.tool, tc.err, tc.output, got, tc.want)
			}
		})
	}
}

func TestEveryErrorClassHasHint(t *testing.T) {
	classes := []string{
		"syntax", "auth", "timeout", "not_found", "permission", "network", "environment", "unknown",
		"credential_missing", "credential_expired", "sandbox_fs_denied", "credential_state_readonly",
	}
	for _, class := range classes {
		if strings.TrimSpace(errorClassHints[class]) == "" {
			t.Fatalf("class %q has no hint", class)
		}
	}
	for _, rule := range errorClassRules {
		if strings.TrimSpace(errorClassHints[rule.class]) == "" {
			t.Fatalf("rule class %q has no hint", rule.class)
		}
	}
}

func TestIsolatedSandboxFailureOnlyEscalatesHostStateFailures(t *testing.T) {
	authErr := enrichIsolatedSandboxFailure("terminal", 1, errors.New("gh: not logged in"), "")
	if got := authErr.Error(); !strings.Contains(got, "error_class: credential_missing") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("missing credential must preserve its diagnosis without host escalation: %q", got)
	}

	networkErr := enrichIsolatedSandboxFailure("terminal", 1, errors.New("network is unreachable"), "")
	if got := networkErr.Error(); !strings.Contains(got, "error_class: sandbox_no_network") || !strings.Contains(got, "network:shared") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("network failure must request the narrow capability: %q", got)
	}

	syntaxErr := enrichIsolatedSandboxFailure("terminal", 2, errors.New("sh: syntax error: unexpected token"), "")
	if got := syntaxErr.Error(); !strings.Contains(got, "error_class: syntax") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("syntax failure must stay in the sandbox correction path: %q", got)
	}
}

// The live failure this test locks down: gcloud cannot write
// ~/.config/gcloud/credentials.db inside the sandbox, the text mentions
// "credentials", and the flat taxonomy reported "fix auth state first" plus
// "host execution is not an authentication or setup fix" — contradicting the
// only correct remedy. The sandbox-gated classifier must win.
func TestIsolatedCredentialStateDenialIsNotReportedAsAuth(t *testing.T) {
	output := "WARNING: Could not setup log file in /home/u/.config/gcloud/logs, (OSError: [Errno 30] Read-only file system\n" +
		"ERROR: (gcloud.auth.list) Unable to create private file [/home/u/.config/gcloud/credentials.db]: [Errno 30] Read-only file system"
	err := enrichIsolatedSandboxFailure("terminal", 1, errors.New("command failed: exit status 1"), output)
	got := err.Error()
	if !strings.Contains(got, "error_class: credential_state_readonly") {
		t.Fatalf("state write denial must classify as credential_state_readonly: %q", got)
	}
	for _, forbidden := range []string{"error_class: auth", "fix auth state", "not an authentication or setup fix"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("state write denial must not carry auth guidance (%q): %q", forbidden, got)
		}
	}
}

func TestIsolatedSandboxDenialGating(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		output   string
		want     string
		denied   bool
	}{
		{"workspace-external write", 1, "mkdir: cannot create directory '/opt/x': Read-only file system", "sandbox_fs_denied", true},
		{"state dir write", 1, "Unable to create private file [/home/u/.config/gcloud/credentials.db]: Read-only file system", "credential_state_readonly", true},
		{"kube state dir", 1, "open /home/u/.kube/cache/x: permission denied", "credential_state_readonly", true},
		// Shell-level rejections are never sandbox denials.
		{"usage error", 2, "sh: 1: set: Illegal option -o pipefail", "", false},
		{"not found", 127, "sh: 1: nope: not found", "", false},
		{"not executable", 126, "sh: 1: ./x: Permission denied", "", false},
		// An auth rejection that merely contains a permission word must not be
		// blamed on the sandbox.
		{"ssh publickey", 255, "git@github.com: Permission denied (publickey).", "", false},
		{"http 403", 1, "Error 403: Forbidden - permission denied on resource", "", false},
		{"plain failure", 1, "boom", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, ok := classifySandboxDenial(tc.exitCode, errors.New("command failed"), tc.output)
			if ok != tc.denied {
				t.Fatalf("classifySandboxDenial denied = %v, want %v (class %q)", ok, tc.denied, class)
			}
			if ok && class != tc.want {
				t.Fatalf("classifySandboxDenial class = %q, want %q", class, tc.want)
			}
		})
	}
}

func TestTerminalFailureAppendsErrorClassLine(t *testing.T) {
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command": "definitely-not-a-real-command-xyz --version",
		"cwd":     t.TempDir(),
		"timeout": 10,
	})
	if err == nil {
		t.Fatalf("expected failure, got success with output %q", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "command failed:") {
		t.Fatalf("raw failure text missing from error: %q", msg)
	}
	if !strings.Contains(msg, "error_class: not_found; hint: ") {
		t.Fatalf("error_class line missing from error: %q", msg)
	}
	// The raw shell stderr must be preserved untouched in the captured output.
	if !strings.Contains(strings.ToLower(out), "not found") {
		t.Fatalf("raw stderr not preserved in output: %q", out)
	}
}

func TestTerminalTimeoutAppendsErrorClassLine(t *testing.T) {
	_, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command": "sleep 5",
		"cwd":     t.TempDir(),
		"timeout": 1,
	})
	if err == nil {
		t.Fatal("expected timeout failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "command timed out after 1 seconds") {
		t.Fatalf("raw timeout text missing from error: %q", msg)
	}
	if !strings.Contains(msg, "error_class: timeout; hint: ") {
		t.Fatalf("error_class line missing from error: %q", msg)
	}
}

func TestTerminalSuccessOutputUnchanged(t *testing.T) {
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command": "echo enrich-check",
		"cwd":     t.TempDir(),
		"timeout": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "error_class:") {
		t.Fatalf("success output must not carry enrichment: %q", out)
	}
}

func TestReadFileMissingAppendsErrorClassLine(t *testing.T) {
	_, err := NewReadFileTool().Execute(map[string]interface{}{
		"path": filepath.Join(t.TempDir(), "missing.txt"),
	})
	if err == nil {
		t.Fatal("expected failure for missing file")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no such file or directory") {
		t.Fatalf("raw os error missing: %q", msg)
	}
	if !strings.Contains(msg, "error_class: not_found; hint: ") {
		t.Fatalf("error_class line missing from error: %q", msg)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrap chain lost: %v", err)
	}
}
