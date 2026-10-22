package tools

import (
	"strings"
	"testing"
)

// The keys below are the real ones recorded on 2026-07-28: ten person-scope
// host-execution grants, plus one older row from before host grants carried a
// workspace/command resource. `set` and `for` are shell prologue/control words,
// so those two grants authorised every script that merely started that way.
func TestReviewPersistedGrantKey(t *testing.T) {
	const hostReason = "exec:" + HostEscapeApprovalReason
	const resource = "|resource=workspace:7a417b1b00a598fc:command:"

	cases := []struct {
		name   string
		key    string
		family string
		keep   bool
	}{
		{"prologue builtin", hostReason + resource + "set", "set", false},
		{"control keyword", hostReason + resource + "for", "for", false},
		{"interpreter", hostReason + resource + "python3", "python3", false},
		{"git", hostReason + resource + "git", "git", false},
		{"find", hostReason + resource + "find", "find", false},
		{"legacy host grant with no resource", hostReason, "", false},
		{"unresolved family", hostReason + resource + "unknown", "unknown", false},

		{"credential CLI stays", hostReason + resource + "gcloud", "gcloud", true},
		{"kubectl stays", hostReason + resource + "kubectl", "kubectl", true},
		{"aws stays", hostReason + resource + "aws", "aws", true},
		{"gh stays", hostReason + resource + "gh", "gh", true},
		{"argocd stays", hostReason + resource + "argocd", "argocd", true},

		// A sandboxed class names its command class and stays; one keyed on the
		// reason it was asked about is not minted any more and nothing redeems
		// it, so it is withdrawn rather than listed as if it still applied.
		{"sandboxed class", "exec:terminal|class=chmod", "chmod", true},
		{"sandboxed banned class", "exec:terminal|class=python3", "python3", false},
		// A class bound to its in-place credential writes is the same class:
		// the boot review keeps it and reports the class without the binding.
		{"class bound to host writes", "exec:terminal|class=npm run deploy" + GrantKeyHostWritesMarker + "0123456789abcdef", "npm run deploy", true},
		{"banned class bound to host writes", "exec:terminal|class=python3" + GrantKeyHostWritesMarker + "0123456789abcdef", "python3", false},
		{"reason-keyed sandboxed class", "exec:invokes dangerous command: chmod", "", false},
		{"host class keyed on another reason", "exec:invokes dangerous command: chmod" + resource + "chmod", "chmod", false},
		// Non-exec classes bucket by reason and are unaffected by this floor.
		{"write tool class", "write_file:accesses restricted path", "", true},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			family, keep := ReviewPersistedGrantKey(tc.key)
			if keep != tc.keep {
				t.Fatalf("ReviewPersistedGrantKey(%q) keep = %v (family %q), want %v", tc.key, keep, family, tc.keep)
			}
			if family != tc.family {
				t.Fatalf("ReviewPersistedGrantKey(%q) family = %q, want %q", tc.key, family, tc.family)
			}
		})
	}
}

// A key minted today must survive its own review: mint and review have to agree,
// or the boot sweep would withdraw grants the user just made.
func TestMintedGrantKeysSurviveReview(t *testing.T) {
	scope := ExecutionScope{TenantID: "default", PersonID: "p1", WorkspaceID: "ws1", WorkspaceRoot: "/tmp/ws"}
	for _, mode := range []SandboxMode{SandboxHost, SandboxIsolated} {
		for _, command := range []string{
			"gcloud builds list --project p",
			"kubectl get ns",
			"chmod 755 script.sh",
			"ssh build.example.test uptime",
		} {
			args := map[string]interface{}{
				"_tool_name":              "terminal",
				"command":                 command,
				"_effective_sandbox_mode": string(mode),
			}
			_, reason := dangerousToolCall("", "terminal", args)
			key := approvalPatternKeyForScope("terminal", args, reason, scope, true)
			if key == "" {
				// Only a program dangerous for its own sake goes without a
				// class, and only on the host.
				if mode != SandboxHost || !hostDangerousFamily(strings.Fields(command)[0]) {
					t.Fatalf("command %q (%s) should mint a reusable key", command, mode)
				}
				continue
			}
			if _, keep := ReviewPersistedGrantKey(key); !keep {
				t.Fatalf("freshly minted key %q must survive review", key)
			}
		}
	}
}
