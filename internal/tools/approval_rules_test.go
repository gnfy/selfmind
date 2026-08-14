package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestApprovalRuleCandidatesOfferNarrowAuthorizations pins WHICH rules a call may
// create. The prefix rule exists precisely because the grant floor refuses `git`
// as a family — "any git command" would cover `git push --force` — while `git
// status` is the ask people answer most often.
func TestApprovalRuleCandidatesOfferNarrowAuthorizations(t *testing.T) {
	scope := ExecutionScope{WorkspaceRoot: "/workspace/app", TenantID: "t", PersonID: "p"}

	prefix := approvalRuleCandidates("terminal", map[string]interface{}{
		"_tool_name": "terminal", "command": "git status --porcelain",
	}, scope, "")
	if len(prefix) != 1 || prefix[0].Kind != ApprovalRuleKindCommandPrefix {
		t.Fatalf("expected one command-prefix candidate, got %+v", prefix)
	}
	if !strings.Contains(prefix[0].Label, "git status") {
		t.Fatalf("prefix label must name the prefix: %q", prefix[0].Label)
	}
	if strings.Contains(prefix[0].Key, "--porcelain") {
		t.Fatalf("a prefix rule must not carry full command text: %q", prefix[0].Key)
	}

	// Interpreters stay excluded: the tokens after the prefix (or the contents of
	// a named script) decide what actually runs.
	for _, command := range []string{"python3 scripts/deploy.py", "bash deploy.sh", "sudo systemctl restart api"} {
		got := approvalRuleCandidates("terminal", map[string]interface{}{"_tool_name": "terminal", "command": command}, scope, "")
		for _, candidate := range got {
			if candidate.Kind == ApprovalRuleKindCommandPrefix {
				t.Fatalf("%q must not mint a prefix rule, got %q", command, candidate.Key)
			}
		}
	}

	// A complex shell payload cannot be bounded by a prefix at all.
	piped := approvalRuleCandidates("terminal", map[string]interface{}{
		"_tool_name": "terminal", "command": "curl -s https://api.github.com/meta | jq .",
	}, scope, "")
	for _, candidate := range piped {
		if candidate.Kind == ApprovalRuleKindCommandPrefix {
			t.Fatalf("piped payload must not mint a prefix rule: %+v", candidate)
		}
	}

	host := approvalRuleCandidates("terminal", map[string]interface{}{
		"_tool_name": "terminal", "command": "curl -sSL https://api.github.com/repos/x/y",
	}, scope, "network egress command: curl")
	found := false
	for _, candidate := range host {
		if candidate.Kind == ApprovalRuleKindNetworkHost {
			found = true
			if !strings.Contains(candidate.Key, "api.github.com") {
				t.Fatalf("host rule key = %q", candidate.Key)
			}
		}
	}
	if !found {
		t.Fatalf("an egress command with one host should offer a host rule, got %+v", host)
	}

	// Path-root rules only fire for the reason the person is actually being asked.
	outside := approvalRuleCandidates("write_file", map[string]interface{}{
		"_tool_name": "write_file", "path": "/srv/site/index.html",
	}, scope, "accesses path outside project root: /srv/site")
	if len(outside) != 1 || outside[0].Kind != ApprovalRuleKindPathRoot {
		t.Fatalf("expected a path-root candidate, got %+v", outside)
	}
	if unrelated := approvalRuleCandidates("write_file", map[string]interface{}{
		"_tool_name": "write_file", "path": "/srv/site/index.html",
	}, scope, "invokes dangerous command: chmod"); len(unrelated) != 0 {
		t.Fatalf("a path rule must not widen an unrelated ask: %+v", unrelated)
	}
	// Never offer a rule that authorizes everything.
	if root := approvalRuleCandidates("write_file", map[string]interface{}{
		"_tool_name": "write_file", "path": "/passwd",
	}, scope, "accesses path outside project root: /"); len(root) != 0 {
		t.Fatalf("filesystem root must never back a rule: %+v", root)
	}
}

// TestHostFromTokenRejectsNonHosts keeps a network authorization from being minted
// out of an ordinary argument that merely contains a dot.
func TestHostFromTokenRejectsNonHosts(t *testing.T) {
	for _, token := range []string{"deploy.sh", "report.json", "-sSL", "v1.2.3.tar.gz", "./local.txt", "user@host", ""} {
		if host, ok := hostFromToken(token); ok {
			t.Fatalf("%q must not read as a host, got %q", token, host)
		}
	}
	for token, want := range map[string]string{
		"https://api.github.com/x": "api.github.com",
		"api.github.com":           "api.github.com",
		"registry.npmjs.org:443":   "registry.npmjs.org",
		"HTTP://Example.COM":       "example.com",
	} {
		host, ok := hostFromToken(token)
		if !ok || host != want {
			t.Fatalf("hostFromToken(%q) = %q,%v want %q", token, host, ok, want)
		}
	}
}

// TestGrantedRuleSkipsTheAsk is the payoff of batch B2: a rule the person granted
// once ends that question, without granting the broader action class.
func TestGrantedRuleSkipsTheAsk(t *testing.T) {
	grants := newFakeGrantStore()
	cleanup := SetExecutionScope("person-rule", ExecutionScope{
		TenantID: "tenant-rule", PersonID: "person-rule", TaskID: "task-rule",
		WorkspaceRoot: "/workspace/app", ApprovalMode: ApprovalReadOnly, Grants: grants,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatal("a granted rule must not reach the human ask")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()
	_ = grants.GrantApproval(context.Background(), "person", "tenant-rule", "person-rule", "person-rule",
		approvalRuleKey(ApprovalRuleKindCommandPrefix, "git status"), time.Time{})

	ran := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-rule", "_tool_name": "terminal", "command": "git status --porcelain",
	}); err != nil {
		t.Fatalf("granted rule should run: %v", err)
	}
	if !ran {
		t.Fatal("command should have executed")
	}
}

// TestMultiTargetWriteNeedsEveryTargetGranted pins batch B3: a patch touching two
// roots must not be released by a grant that covers one of them.
func TestMultiTargetWriteNeedsEveryTargetGranted(t *testing.T) {
	scope := ExecutionScope{WorkspaceRoot: "/workspace/app"}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: /srv/site/index.html",
		"+hello",
		"*** Update File: /opt/other/config.yaml",
		"+key: value",
		"*** End Patch",
	}, "\n")
	keys := approvalTargetRuleKeys("patch", map[string]interface{}{"patch": patch}, scope)
	if len(keys) != 2 {
		t.Fatalf("expected one key per outside target, got %v", keys)
	}
	granted := map[string]bool{keys[0]: true}
	if allApprovalKeysGranted(keys, func(key string) bool { return granted[key] }) {
		t.Fatal("one covered target must not authorize the whole patch")
	}
	granted[keys[1]] = true
	if !allApprovalKeysGranted(keys, func(key string) bool { return granted[key] }) {
		t.Fatal("all targets covered should authorize the patch")
	}

	// In-workspace targets need no grant at all.
	inside := approvalTargetRuleKeys("patch", map[string]interface{}{
		"patch": "*** Begin Patch\n*** Update File: internal/app/main.go\n+x\n*** End Patch",
	}, scope)
	if len(inside) != 0 {
		t.Fatalf("workspace targets need no rule keys, got %v", inside)
	}
}

// TestUnofferedRuleKeyIsRefused is the offered-only invariant: a decision arriving
// from ANY surface cannot mint an authorization the daemon never proposed.
func TestUnofferedRuleKeyIsRefused(t *testing.T) {
	grants := newFakeGrantStore()
	cleanup := SetExecutionScope("person-forge", ExecutionScope{
		TenantID: "tenant-forge", PersonID: "person-forge", TaskID: "task-forge",
		WorkspaceRoot: "/workspace/app", ApprovalMode: ApprovalReadOnly, Grants: grants,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{
				Approved: true, ApprovalID: "apr_forge", Scope: "person",
				// Never offered for this call.
				GrantKey: approvalRuleKey(ApprovalRuleKindPathRoot, "/"),
			}, nil
		},
	})
	defer cleanup()

	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) { return "ok", nil })
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-forge", "_tool_name": "terminal", "command": "git status",
	}); err == nil || !strings.Contains(err.Error(), "was not offered") {
		t.Fatalf("forged scope must reject the call, got %v", err)
	}
	forged := grants.key("person", "person-forge", approvalRuleKey(ApprovalRuleKindPathRoot, "/"))
	if grants.granted[forged] {
		t.Fatal("a rule key the call never offered must never be stored")
	}
}
