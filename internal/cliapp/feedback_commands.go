package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/crashreport"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
)

const defaultFeedbackRepository = "gnfy/selfmind"

var (
	feedbackLookPath       = exec.LookPath
	feedbackCommandContext = exec.CommandContext
	feedbackRepositoryRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type feedbackReport struct {
	ID            string                 `json:"id"`
	CreatedAt     time.Time              `json:"created_at"`
	Message       string                 `json:"message"`
	Version       string                 `json:"version"`
	Fingerprint   string                 `json:"fingerprint"`
	OS            string                 `json:"os"`
	Arch          string                 `json:"arch"`
	InstallMethod string                 `json:"install_method,omitempty"`
	Gateway       map[string]interface{} `json:"gateway"`
	ConfigPath    string                 `json:"config_path"`
	CrashReport   string                 `json:"crash_report,omitempty"`
	Privacy       string                 `json:"privacy"`
}

func (a *App) runFeedbackCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "feedback" {
		return false, 0
	}
	fs := flag.NewFlagSet("selfmind feedback", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	out := fs.String("out", "", "write the redacted JSON report to this file")
	send := fs.Bool("send", false, "submit the report (GitHub Issue by default)")
	repository := fs.String("repo", "", "GitHub repository in OWNER/REPO form")
	includeCrash := fs.Bool("include-crash", false, "include the latest local crash report")
	if err := fs.Parse(a.args[2:]); err != nil {
		return true, 2
	}
	message := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if message == "" {
		fmt.Fprintln(a.stderr, "usage: selfmind feedback [--out FILE|--send] [--repo OWNER/REPO] [--include-crash] <message>")
		return true, 2
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	report := a.buildFeedbackReport(cfg, message, *includeCrash)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	data = append(data, '\n')

	path := strings.TrimSpace(*out)
	if path == "" {
		path, err = defaultFeedbackPath(report.ID)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return true, 1
		}
	}
	if err := writePrivateFile(path, data); err != nil {
		fmt.Fprintf(a.stderr, "Write feedback report: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Feedback report saved: %s\n", path)

	if *send {
		endpoint := strings.TrimSpace(cfg.Feedback.Endpoint)
		if endpoint == "" {
			endpoint = strings.TrimSpace(os.Getenv("SELFMIND_FEEDBACK_ENDPOINT"))
		}
		if endpoint != "" {
			if err := sendFeedback(a.ctx, endpoint, data); err != nil {
				fmt.Fprintf(a.stderr, "Send feedback: %v\n", err)
				return true, 1
			}
			fmt.Fprintln(a.stdout, "Feedback sent.")
			return true, 0
		}

		repo := resolveFeedbackRepository(*repository, cfg.Feedback.Repository)
		issueURL, err := submitGitHubFeedback(a.ctx, repo, cfg.Feedback.Labels, report, path)
		if err != nil {
			fmt.Fprintf(a.stderr, "Submit feedback: %v\n", err)
			fmt.Fprintf(a.stderr, "The local report was preserved: %s\n", path)
			fmt.Fprintf(a.stderr, "Create the issue manually: %s\n", feedbackManualIssueURL(repo, report))
			return true, 1
		}
		fmt.Fprintf(a.stdout, "Feedback issue created: %s\n", issueURL)
	}
	return true, 0
}

func resolveFeedbackRepository(flagValue, configValue string) string {
	for _, value := range []string{
		strings.TrimSpace(flagValue),
		strings.TrimSpace(os.Getenv("SELFMIND_FEEDBACK_REPOSITORY")),
		strings.TrimSpace(configValue),
		defaultFeedbackRepository,
	} {
		if value != "" {
			return value
		}
	}
	return defaultFeedbackRepository
}

func submitGitHubFeedback(
	ctx context.Context,
	repository string,
	labels []string,
	report feedbackReport,
	reportPath string,
) (string, error) {
	if !feedbackRepositoryRE.MatchString(repository) {
		return "", fmt.Errorf("invalid GitHub repository %q; expected OWNER/REPO", repository)
	}
	ghPath, err := feedbackLookPath("gh")
	if err != nil {
		return "", fmt.Errorf("GitHub CLI (gh) is not installed; install it and run `gh auth login --hostname github.com`")
	}

	auth := feedbackCommandContext(ctx, ghPath, "auth", "status", "--hostname", "github.com")
	if output, err := auth.CombinedOutput(); err != nil {
		detail := firstNonEmptyLine(string(output))
		if detail != "" {
			detail = ": " + tools.RedactSensitive(detail)
		}
		return "", fmt.Errorf("GitHub authentication is missing or expired%s; run `gh auth login --hostname github.com`", detail)
	}

	bodyPath := reportPath + ".issue.md"
	if err := writePrivateFile(bodyPath, []byte(githubIssueBody(report))); err != nil {
		return "", fmt.Errorf("write temporary GitHub issue body: %w", err)
	}
	defer os.Remove(bodyPath)

	args := []string{
		"issue", "create",
		"--repo", repository,
		"--title", githubIssueTitle(report.Message),
		"--body-file", bodyPath,
	}
	for _, label := range labels {
		if label = strings.TrimSpace(label); label != "" {
			args = append(args, "--label", label)
		}
	}
	cmd := feedbackCommandContext(ctx, ghPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(tools.RedactSensitive(string(output)))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("GitHub Issue creation failed: %s", detail)
	}
	issueURL := lastNonEmptyLine(string(output))
	if !strings.HasPrefix(issueURL, "https://") {
		return "", fmt.Errorf("GitHub CLI returned no issue URL")
	}
	return issueURL, nil
}

func githubIssueTitle(message string) string {
	title := firstNonEmptyLine(message)
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		title = "SelfMind feedback"
	}
	runes := []rune(title)
	if len(runes) > 90 {
		title = string(runes[:87]) + "..."
	}
	return "[Feedback] " + title
}

func githubIssueBody(report feedbackReport) string {
	var gateway string
	if data, err := json.MarshalIndent(report.Gateway, "", "  "); err == nil {
		gateway = string(data)
	}
	var body strings.Builder
	body.WriteString("## Feedback\n\n")
	body.WriteString(report.Message)
	body.WriteString("\n\n## Environment\n\n")
	fmt.Fprintf(&body, "- SelfMind: `%s`\n", report.Version)
	fmt.Fprintf(&body, "- Build: `%s`\n", report.Fingerprint)
	fmt.Fprintf(&body, "- Platform: `%s/%s`\n", report.OS, report.Arch)
	if report.InstallMethod != "" {
		fmt.Fprintf(&body, "- Install method: `%s`\n", report.InstallMethod)
	}
	body.WriteString("\n## Diagnostics\n\n```json\n")
	body.WriteString(gateway)
	body.WriteString("\n```\n\n")
	body.WriteString("Prompts, assistant output, tool output, credentials, and environment values were not attached.\n")
	if report.CrashReport != "" {
		body.WriteString("A crash report was retained in the private local report and was not published to this Issue.\n")
	}
	fmt.Fprintf(&body, "\n<!-- selfmind-feedback-id:%s -->\n", report.ID)
	return body.String()
}

func feedbackManualIssueURL(repository string, report feedbackReport) string {
	if !feedbackRepositoryRE.MatchString(repository) {
		repository = defaultFeedbackRepository
	}
	values := url.Values{}
	values.Set("title", githubIssueTitle(report.Message))
	body := "## Feedback\n\n" + boundString(report.Message, 700) +
		"\n\n<!-- selfmind-feedback-id:" + report.ID + " -->"
	values.Set("body", body)
	return "https://github.com/" + repository + "/issues/new?" + values.Encode()
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func lastNonEmptyLine(value string) string {
	lines := strings.Split(value, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (a *App) buildFeedbackReport(cfg *config.Config, message string, includeCrash bool) feedbackReport {
	now := time.Now().UTC()
	report := feedbackReport{
		ID:            now.Format("20060102T150405.000000000Z"),
		CreatedAt:     now,
		Message:       tools.RedactSensitive(message),
		Version:       buildinfo.Version,
		Fingerprint:   buildinfo.Fingerprint(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		InstallMethod: strings.TrimSpace(os.Getenv("SELFMIND_INSTALL_METHOD")),
		ConfigPath:    filepath.Base(cfg.Path),
		Gateway:       map[string]interface{}{"state": "unavailable"},
		Privacy:       "No prompts, model output, tool output, API keys, or environment values are included unless explicitly attached.",
	}
	ctx, cancel := contextWithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	if data, code, err := gatewayrt.RequestStatus(ctx, a.gatewayURL()); err == nil && code < 400 {
		var status map[string]interface{}
		if json.Unmarshal(data, &status) == nil {
			report.Gateway = sanitizeGatewayStatus(status)
		}
	}
	if includeCrash {
		if path, ok := crashreport.Latest(); ok {
			if data, err := os.ReadFile(path); err == nil {
				report.CrashReport = boundString(tools.RedactSensitive(string(data)), 64*1024)
			}
		}
	}
	return report
}

func sanitizeGatewayStatus(status map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for _, key := range []string{"state", "active_run_count", "queued_count"} {
		if value, ok := status[key]; ok {
			result[key] = value
		}
	}
	if runtimeValue, ok := status["runtime"].(map[string]interface{}); ok {
		runtimeSafe := map[string]interface{}{}
		for _, key := range []string{"build_fingerprint", "pid", "uptime_seconds"} {
			if value, ok := runtimeValue[key]; ok {
				runtimeSafe[key] = value
			}
		}
		result["runtime"] = runtimeSafe
	}
	return result
}

func defaultFeedbackPath(id string) (string, error) {
	home, err := selfMindHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "feedback", id+".json"), nil
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sendFeedback(ctx context.Context, endpoint string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("feedback endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func boundString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "\n... truncated ..."
}
