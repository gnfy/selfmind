package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackWritesPrivateRedactedReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:1")
	const secret = "sk-super-secret-feedback-value"
	output := filepath.Join(home, "report.json")
	configPath := filepath.Join(home, "config.yaml")
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(),
		args: []string{
			"selfmind",
			"feedback",
			"--out",
			output,
			"request failed with api_key=" + secret,
		},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: configPath,
	}

	handled, code := app.runFeedbackCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("feedback = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("feedback report leaked a secret:\n%s", data)
	}
	var report feedbackReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Message == "" || !strings.Contains(report.Message, "[REDACTED]") {
		t.Fatalf("feedback message was not usefully redacted: %q", report.Message)
	}
	if info, err := os.Stat(output); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("feedback permissions = %o, want private", info.Mode().Perm())
	}
}

func TestValidateSelfMindHomeRejectsUnrelatedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := validateSelfMindHome(filepath.Join(home, "other")); err == nil {
		t.Fatal("expected unrelated directory to be rejected")
	}
	if err := validateSelfMindHome(filepath.Join(home, ".selfmind")); err != nil {
		t.Fatalf("expected ~/.selfmind to be accepted: %v", err)
	}
}

func TestFeedbackSendCreatesIssueInDefaultRepository(t *testing.T) {
	installFeedbackGHHelper(t)
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("SELFMIND_TEST_EXPECT_REPO", defaultFeedbackRepository)
	t.Setenv("SELFMIND_TEST_GH_AUTH", "ok")

	home := t.TempDir()
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "feedback", "--send", "The CLI stopped after a tool call"},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: filepath.Join(home, "config.yaml"),
	}

	handled, code := app.runFeedbackCommandIfRequested()
	if !handled || code != 0 {
		t.Fatalf("feedback = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "https://github.com/gnfy/selfmind/issues/123") {
		t.Fatalf("missing created issue URL: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestFeedbackSendExplainsMissingGitHubCLIAndKeepsReport(t *testing.T) {
	oldLookPath := feedbackLookPath
	feedbackLookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}
	t.Cleanup(func() { feedbackLookPath = oldLookPath })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "feedback", "--send", "Cannot continue a task"},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: filepath.Join(home, "config.yaml"),
	}

	handled, code := app.runFeedbackCommandIfRequested()
	if !handled || code != 1 {
		t.Fatalf("feedback = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "GitHub CLI (gh) is not installed") ||
		!strings.Contains(stderr.String(), "The local report was preserved") ||
		!strings.Contains(stderr.String(), "https://github.com/gnfy/selfmind/issues/new?") {
		t.Fatalf("missing actionable fallback: %q", stderr.String())
	}
}

func TestFeedbackSendExplainsExpiredGitHubAuthentication(t *testing.T) {
	installFeedbackGHHelper(t)
	t.Setenv("SELF_GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("SELFMIND_TEST_GH_AUTH", "expired")

	home := t.TempDir()
	t.Setenv("HOME", home)
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx:        context.Background(),
		args:       []string{"selfmind", "feedback", "--send", "Authentication test"},
		stdout:     &stdout,
		stderr:     &stderr,
		configPath: filepath.Join(home, "config.yaml"),
	}

	handled, code := app.runFeedbackCommandIfRequested()
	if !handled || code != 1 {
		t.Fatalf("feedback = handled:%v code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "authentication is missing or expired") ||
		!strings.Contains(stderr.String(), "gh auth login --hostname github.com") {
		t.Fatalf("missing authentication guidance: %q", stderr.String())
	}
}

func installFeedbackGHHelper(t *testing.T) {
	t.Helper()
	oldLookPath := feedbackLookPath
	oldCommand := feedbackCommandContext
	feedbackLookPath = func(string) (string, error) { return "gh", nil }
	feedbackCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		helperArgs := []string{"-test.run=TestFeedbackGHHelperProcess", "--"}
		helperArgs = append(helperArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(), "SELFMIND_TEST_GH_HELPER=1")
		return cmd
	}
	t.Cleanup(func() {
		feedbackLookPath = oldLookPath
		feedbackCommandContext = oldCommand
	})
}

func TestFeedbackGHHelperProcess(t *testing.T) {
	if os.Getenv("SELFMIND_TEST_GH_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(90)
	}
	args := os.Args[separator+1:]
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		if os.Getenv("SELFMIND_TEST_GH_AUTH") == "expired" {
			_, _ = os.Stderr.WriteString("not logged in\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "issue" && args[1] == "create" {
		repository := argumentValue(args, "--repo")
		if want := os.Getenv("SELFMIND_TEST_EXPECT_REPO"); want != "" && repository != want {
			_, _ = os.Stderr.WriteString("unexpected repository: " + repository + "\n")
			os.Exit(2)
		}
		bodyPath := argumentValue(args, "--body-file")
		body, err := os.ReadFile(bodyPath)
		if err != nil || !strings.Contains(string(body), "selfmind-feedback-id:") {
			_, _ = os.Stderr.WriteString("missing feedback marker\n")
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("https://github.com/" + repository + "/issues/123\n")
		os.Exit(0)
	}
	os.Exit(91)
}

func argumentValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}
