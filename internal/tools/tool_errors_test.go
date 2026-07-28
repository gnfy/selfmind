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
		{"missing credential", "terminal", errors.New("command failed: exit status 1"), "fatal: could not read credentials", "auth"},
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
	classes := []string{"syntax", "auth", "timeout", "not_found", "permission", "network", "environment", "unknown"}
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
	authErr := enrichIsolatedSandboxFailure("terminal", errors.New("gh: not logged in"), "")
	if got := authErr.Error(); !strings.Contains(got, "error_class: auth") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("auth failure must preserve its diagnosis without host escalation: %q", got)
	}

	networkErr := enrichIsolatedSandboxFailure("terminal", errors.New("network is unreachable"), "")
	if got := networkErr.Error(); !strings.Contains(got, "error_class: sandbox_no_network") || !strings.Contains(got, "network:shared") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("network failure must request the narrow capability: %q", got)
	}

	syntaxErr := enrichIsolatedSandboxFailure("terminal", errors.New("sh: syntax error: unexpected token"), "")
	if got := syntaxErr.Error(); !strings.Contains(got, "error_class: syntax") || strings.Contains(got, "sandbox=host") {
		t.Fatalf("syntax failure must stay in the sandbox correction path: %q", got)
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
