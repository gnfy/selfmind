package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"selfmind/internal/executionenv"
	"selfmind/internal/ui/components"
)

// renderCLIApproval renders an ask the way the terminal panel does: the reason
// and the containment travel from the approval request into the panel.
func renderCLIApproval(request ToolApprovalRequest, width int) string {
	return ansi.Strip(components.NewApprovalPromptDetailed(components.ApprovalDetails{
		Tool: request.ToolName, Target: execCommandPayload(request.ToolName, request.Args),
		Reason: request.Reason, Containment: request.Containment,
	}).View(width))
}

// M2: every surface truncates the reason (IM to 120 characters, the terminal
// panel to two lines), and the boundary facts an approver must not miss —
// credential caches written in place, or running on the host although the call
// did not ask to — lead it, in a form short enough to survive.
func TestApprovalReasonLeadsWithTheBoundaryFacts(t *testing.T) {
	longDeploy := "#!/bin/sh\naws --profile sso-dev sts get-caller-identity\n"
	for _, tc := range []struct {
		name, config, profile string
	}{
		{"sso", ssoProfileConfig, "sso-dev"},
		// Another configuration and profile with the same writes.
		{"role", roleProfileConfig, "ops-admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireInPlaceBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, tc.config)
			run := newBoundaryRun(t, "facts-"+tc.name, home, writeStub(t, base, "aws", awsCredentialStateStub),
				executionenv.TrustTrusted, ApprovalOnRequest, nil)
			if err := os.WriteFile(filepath.Join(run.workspace, "deploy.sh"), []byte(longDeploy), 0o755); err != nil {
				t.Fatal(err)
			}
			run.approve = false
			command := "./deploy.sh --stage production --region eu-west-1 --profile " + tc.profile + " --notify release-channel --wait 600"
			if _, _, err := run.call(command, nil); err == nil {
				t.Fatal("a declined ask must not run")
			}
			if len(run.asks) != 1 {
				t.Fatalf("the call must be asked once, asks=%d", len(run.asks))
			}
			fact := "writes credential caches in place: ~/.aws/sso/cache, ~/.aws/cli/cache"
			if reason := run.asks[0].Reason; !strings.HasPrefix(reason, fact) {
				t.Fatalf("the reason must lead with the in-place writes:\n%q", reason)
			}
			panel := renderCLIApproval(run.asks[0], 80)
			if !strings.Contains(strings.Join(strings.Fields(panel), " "), fact) {
				t.Fatalf("the terminal panel must show the in-place writes:\n%s", panel)
			}
		})
	}

	// The host-fallback fact leads the same way.
	t.Run("host-fallback", func(t *testing.T) {
		requireInPlaceBackend(t)
		withExecSandboxPolicy(t, true, false, false)
		base := fixtureBase(t)
		home, _ := symlinkedCacheHome(t, base, ssoProfileConfig, "sso/cache")
		run := newBoundaryRun(t, "facts-host", home, writeStub(t, base, "aws", awsCredentialStateStub),
			executionenv.TrustTrusted, ApprovalOnRequest, nil)
		if err := os.WriteFile(filepath.Join(run.workspace, "deploy.sh"), []byte(longDeploy), 0o755); err != nil {
			t.Fatal(err)
		}
		run.approve = false
		_, _, _ = run.call("./deploy.sh --stage production", nil)
		if len(run.asks) != 1 || !strings.HasPrefix(run.asks[0].Reason, "runs on the host: ") {
			t.Fatalf("the reason must lead with the host boundary: %+v", run.asks)
		}
		if panel := renderCLIApproval(run.asks[0], 80); !strings.Contains(panel, "runs on the host") {
			t.Fatalf("the terminal panel must show the host boundary:\n%s", panel)
		}
	})
}
