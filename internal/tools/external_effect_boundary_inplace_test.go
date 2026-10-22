//go:build darwin

package tools

import (
	"context"
	"strings"
	"testing"
)

// The claim boundary's once-only ask releases the exact call, including the
// credential cache it writes in place: it leads with those writes and carries
// the containment summary, and the authorization a parked approval leaves is
// bound to the write set, so an approval made for one configuration never
// releases the call once its configuration writes more.
func TestForeignEffectAskNamesTheInPlaceWrites(t *testing.T) {
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, false)
	f := newGuardFixture(t, "foreign-ask")
	args := func() map[string]interface{} {
		return map[string]interface{}{
			"_tenant_id": f.run.tenant, "_tool_name": "terminal", "cwd": f.ws,
			"command":                 "aws sts get-caller-identity --profile sso-dev",
			"_effective_sandbox_mode": string(SandboxIsolated), credentialReadArgKey: true,
		}
	}
	scope, ok := currentExecutionScopeAny(args())
	if !ok {
		t.Fatal("fixture: the run's scope is missing")
	}
	ask := func() ToolApprovalRequest {
		var shown ToolApprovalRequest
		asking := scope
		asking.Approval = func(_ context.Context, request ToolApprovalRequest) (ToolApprovalDecision, error) {
			shown = request
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-foreign"}, nil
		}
		if err := approveForeignExternalEffects(context.Background(), asking, args(), foreignClaimFixture()); err != nil {
			t.Fatal(err)
		}
		return shown
	}

	request := ask()
	if want := "writes credential caches in place: ~/.aws/sso/cache"; !strings.HasPrefix(request.Reason, want) ||
		!strings.Contains(request.Reason, "another run left an external effect") {
		t.Fatalf("the ask must lead with the in-place writes and keep the claim: %q", request.Reason)
	}
	if !strings.Contains(request.Containment, "host_credential_writes=") {
		t.Fatalf("the ask must carry the containment summary: %q", request.Containment)
	}
	bound := request.AuthorizationFingerprint

	// The constraint that must change the result: the same call under a
	// configuration that writes nothing in place leads with no write fact and
	// asks under another authorization.
	writeAWSConfig(t, f.home, staticAWSConfig)
	plain := ask()
	if strings.Contains(plain.Reason, "credential caches") || strings.Contains(plain.Containment, "host_credential_writes=") {
		t.Fatalf("a call that writes nothing in place must not claim to: %q / %q", plain.Reason, plain.Containment)
	}
	if plain.AuthorizationFingerprint == "" || plain.AuthorizationFingerprint == bound {
		t.Fatalf("an authorization parked for the in-place writes must not cover the call without them: %q vs %q", bound, plain.AuthorizationFingerprint)
	}
}
