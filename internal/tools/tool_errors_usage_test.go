package tools

import (
	"errors"
	"strings"
	"testing"
)

// A CLI that rejects its own arguments never reached a service. The class and
// hint must say so even when the usage text mentions profiles or MFA, so the
// model does not report a permission denial (observed live with aws exit 252).
func TestClassifyToolErrorTreatsLocalUsageRejectionAsCLIUsage(t *testing.T) {
	err := errors.New("command failed: exit status 252")
	output := "usage: aws [options] <command> <subcommand> [<subcommand> ...] [parameters]\n" +
		"To see help text, you can run: aws help\n" +
		"aws: error: argument --sort-order: expected one argument\n" +
		"Note: the cw2 profile requires MFA for this role."
	if class := ClassifyToolError("terminal", err, output); class != "cli_usage" {
		t.Fatalf("class=%q, want cli_usage", class)
	}
	enriched := enrichToolFailure("terminal", err, output)
	if !strings.Contains(enriched.Error(), "not an authentication, permission, or MFA failure") {
		t.Fatalf("hint must rule out a permission diagnosis: %v", enriched)
	}
}

func TestClassifyToolErrorKeepsServiceDenialAsAuth(t *testing.T) {
	err := errors.New("command failed: exit status 254")
	output := "An error occurred (AccessDeniedException) when calling the ListBuildsForProject operation: User is not authorized to perform: codebuild:ListBuildsForProject with an explicit deny in an identity-based policy (EnforceMFA)"
	if class := ClassifyToolError("terminal", err, output); class == "cli_usage" {
		t.Fatalf("a service denial must not be classified as a local usage error")
	}
}
