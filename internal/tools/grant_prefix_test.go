package tools

import (
	"context"
	"testing"
)

func prefixOf(t *testing.T, command string) (string, bool) {
	t.Helper()
	prefix, ok := grantCommandPrefix("terminal", execArgs(command))
	return grantPrefixLabel(prefix), ok
}

// TestGrantPrefixSeparatesReadsFromWrites is the reason the prefix reaches a
// verb instead of stopping at the program. Measured over a week of this
// deployment's approvals, a two-token cut put `aws codebuild start-build` in the
// same class as `aws codebuild batch-get-builds`, and `gcloud builds import`
// with `gcloud builds describe`. Remembering the read would then have released
// the write.
func TestGrantPrefixSeparatesReadsFromWrites(t *testing.T) {
	read, readOK := prefixOf(t, "aws codebuild batch-get-builds --ids abc --profile cw3")
	write, writeOK := prefixOf(t, "aws codebuild start-build --project-name p --profile cw3")
	if !readOK || !writeOK {
		t.Fatalf("both must be grant-eligible: %v %v", readOK, writeOK)
	}
	if read == write {
		t.Fatalf("a read and a write must not share a class: %q", read)
	}
	if read != "aws codebuild batch-get-builds" || write != "aws codebuild start-build" {
		t.Fatalf("prefixes = %q / %q", read, write)
	}
}

// TestGrantPrefixIgnoresTrailingArguments keeps the stored set small: build ids
// and resource names change on every invocation, so a class that included them
// would need one row per call.
func TestGrantPrefixIgnoresTrailingArguments(t *testing.T) {
	first, ok1 := prefixOf(t, "gcloud builds describe 4226f7d7 --project track-463207")
	second, ok2 := prefixOf(t, "gcloud builds describe 121ffcbf --project track-463207 --region us-east4")
	if !ok1 || !ok2 || first != second {
		t.Fatalf("the same operation must key identically: %q (%v) vs %q (%v)", first, ok1, second, ok2)
	}
	if first != "gcloud builds describe" {
		t.Fatalf("prefix = %q", first)
	}
}

// TestGrantPrefixSkipsPipelinePlumbing: a `| head` or a leading `cd` must not
// become the remembered class. Naive per-segment derivation produced 621
// classes for 478 approvals over one week, 461 of them used exactly once,
// because plumbing dominated the stored set.
func TestGrantPrefixSkipsPipelinePlumbing(t *testing.T) {
	label, ok := prefixOf(t, "cd /w/cicd && aws codebuild batch-get-builds --ids abc | jq -r '.builds' | head -5")
	if !ok || label != "aws codebuild batch-get-builds" {
		t.Fatalf("prefix = %q (%v), want the operation alone", label, ok)
	}
	// Plumbing that can write is still an operation: the catalog rule knows.
	if _, ok := prefixOf(t, "aws codebuild list-builds | sort -o saved.txt"); ok {
		t.Fatal("a writing filter must not be treated as plumbing")
	}
}

// TestGrantPrefixRefusesAnUnidentifiedSubcommand fails closed: a CLI that
// dispatches on a subcommand path says nothing a person can consent to until
// the operation is identified, and `gcloud` alone would cover both
// `gcloud builds describe` and `gcloud builds import`.
func TestGrantPrefixRefusesAnUnidentifiedSubcommand(t *testing.T) {
	if label, ok := prefixOf(t, "gcloud alpha some thing other deeper words here"); ok {
		t.Fatalf("an unidentified subcommand must not be grant-eligible, got %q", label)
	}
	// A plain command's name IS its class, and its arguments must stay out.
	label, ok := prefixOf(t, "chmod 755 script.sh")
	if !ok || label != "chmod" {
		t.Fatalf("plain command prefix = %q (%v), want chmod", label, ok)
	}
}

// TestGrantPrefixCarriesTheHTTPMethod: `gh api` performs every method through
// one subcommand and was the largest single approval class in one week of real
// traffic. A remembered GET must never release a DELETE.
func TestGrantPrefixCarriesTheHTTPMethod(t *testing.T) {
	get, ok1 := prefixOf(t, "gh api repos/acme/site/branches")
	del, ok2 := prefixOf(t, "gh api -X DELETE repos/acme/site/git/refs/heads/tmp/x")
	if !ok1 || !ok2 {
		t.Fatalf("both must be grant-eligible: %v %v", ok1, ok2)
	}
	if get == del {
		t.Fatalf("method must separate the classes: %q", get)
	}
	if get != "gh api get repos/acme/site/branches" {
		t.Fatalf("get prefix = %q", get)
	}
	if del != "gh api delete repos/acme/site/git" {
		t.Fatalf("delete prefix = %q", del)
	}
	// The resource path is bounded, so one rule covers a repository's refs
	// without naming every ref.
	other, ok := prefixOf(t, "gh api -X DELETE repos/acme/site/git/refs/heads/tmp/y")
	if !ok || other != del {
		t.Fatalf("sibling refs must share one class: %q vs %q", other, del)
	}
}

// TestGrantPrefixRefusesArbitraryNetworkClients: curl and wget take the method,
// the target and the output file as arguments, so no leading token bounds what
// a standing permission would authorise.
func TestGrantPrefixRefusesArbitraryNetworkClients(t *testing.T) {
	for _, command := range []string{
		"curl -s https://example.com/status",
		"curl -X POST -d @body.json https://api.example.com/v1/things",
		"wget https://example.com/x -O out.bin",
	} {
		if label, ok := prefixOf(t, command); ok {
			t.Errorf("%q must not be grant-eligible, got %q", command, label)
		}
	}
}

// TestDangerousHostCommandStaysOneShot: chmod, chown, mv and kill are left out
// of the floor's banned set so they can be remembered under an ENFORCED
// sandbox, where the blast radius is the workspace. On the host there is no
// such bound, and a standing `chmod` would cover every path the person can
// reach, so the decision stays one-shot there. Over three days of real traffic
// none of the 378 host approvals came from this heuristic, so the cost is
// nothing measurable.
func TestDangerousHostCommandStaysOneShot(t *testing.T) {
	dangerousHost := map[string]interface{}{
		"_tool_name": "terminal", "command": "chmod 777 /tmp/thing",
		"sandbox": "host", "_effective_sandbox_mode": string(SandboxHost),
	}
	// The floor still considers the class mintable — that is what makes the
	// host guard necessary rather than redundant.
	if _, eligible := grantCommandPrefix("terminal", dangerousHost); !eligible {
		t.Fatal("chmod must remain a mintable class for sandboxed execution")
	}
	if flagged, _ := dangerousToolCall("", "terminal", dangerousHost); !flagged {
		t.Fatal("fixture must trip the dangerous-op heuristic")
	}

	// The trap this pins: dangerousToolCall returns true with the host-escape
	// reason for EVERY host request, so a guard that read the bare flag would
	// put every host call back on a one-shot decision and quietly undo the
	// standing answer. An ordinary host command must still be offered one.
	policyFor := func(command string) string {
		captured := ""
		scope := ExecutionScope{
			TenantID: "t", PersonID: "p", WorkspaceID: "ws", WorkspaceRoot: "/work",
			ApprovalMode: ApprovalOnRequest, StandingGrants: InteractiveStandingGrants(),
			Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
				captured = req.DecisionPolicy
				return ToolApprovalDecision{Approved: true, ApprovalID: "apr"}, nil
			},
		}
		cleanup := SetExecutionScope("p", scope)
		defer cleanup()
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
		if _, err := exec(map[string]interface{}{
			"_tenant_id": "p", "_tool_name": "terminal", "command": command,
			"sandbox": "host", "_effective_sandbox_mode": string(SandboxHost),
		}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return captured
	}

	if policy := policyFor("aws codebuild batch-get-builds --ids b --profile cw3"); policy == ApprovalDecisionPolicyOnceOnly {
		t.Fatal("an ordinary host command lost its standing answer: the guard read the bare dangerous flag")
	}
	if policy := policyFor("chmod 777 /tmp/thing"); policy != ApprovalDecisionPolicyOnceOnly {
		t.Fatalf("a destructive host command must stay one-shot, policy = %q", policy)
	}
}

// TestUnresolvedSandboxModeIsTreatedAsUncontained: the guard tests "not
// provably isolated", not "host". An exec call whose sandbox mode fails to
// resolve is annotated with neither mode, and an unresolved mode is not
// evidence of containment, so it must be judged by the same rule as the host.
//
// That rule is not "refuse everything": a verb-bounded class is what bounds a
// standing answer, so an ordinary read still earns one. What an uncontained
// boundary forbids is remembering a command the danger heuristic flagged —
// which under the earlier `== host` test slipped through whenever the mode
// failed to resolve.
func TestUnresolvedSandboxModeIsTreatedAsUncontained(t *testing.T) {
	policyFor := func(command string) (string, bool) {
		captured, asked := "", false
		scope := ExecutionScope{
			TenantID: "t", PersonID: "p", WorkspaceID: "ws", WorkspaceRoot: "/work",
			ApprovalMode: ApprovalOnRequest, StandingGrants: InteractiveStandingGrants(),
			Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
				captured, asked = req.DecisionPolicy, true
				return ToolApprovalDecision{Approved: true, ApprovalID: "apr"}, nil
			},
		}
		cleanup := SetExecutionScope("p", scope)
		defer cleanup()
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
		if _, err := exec(map[string]interface{}{
			"_tenant_id": "p", "_tool_name": "terminal", "command": command,
			"sandbox": "not-a-mode",
		}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return captured, asked
	}

	policy, asked := policyFor("chmod 777 /tmp/thing")
	if !asked {
		t.Fatal("an unresolved boundary must still raise the approval")
	}
	if policy != ApprovalDecisionPolicyOnceOnly {
		t.Fatalf("a destructive command on an unresolved boundary must stay one-shot, policy = %q", policy)
	}
	if policy, _ := policyFor("aws codebuild batch-get-builds --ids b --profile cw3"); policy == ApprovalDecisionPolicyOnceOnly {
		t.Fatal("an ordinary read still has a class that bounds a standing answer")
	}
}
