package tools

import "testing"

func execArgs(command string) map[string]interface{} {
	return map[string]interface{}{"_tool_name": "terminal", "command": command}
}

func TestGrantFloorRejectsUnboundedClasses(t *testing.T) {
	cases := []struct {
		name    string
		command string
		family  string
		ok      bool
	}{
		// The two live leaks: a person-scope host grant keyed `command:set` and
		// `command:for` authorised every script that merely started that way.
		{"leading set builtin keeps the real family", "set -euo pipefail\ngcloud builds list --project p", "gcloud", true},
		{"for loop is control flow, not a family", "for t in a b c; do gcloud builds describe t --project p; done", "", false},
		{"if block is control flow", "if gcloud auth list; then gcloud config list; fi", "", false},
		{"set with two programs is not a family", "set -euo pipefail\nmkdir p\ngcloud builds list", "", false},

		// Arbitrary execution vectors can never become a standing permission.
		{"python interpreter", "python3 provision.py", "", false},
		{"inline node", "node script.js", "", false},
		{"git runs hooks and aliases", "git status", "", false},
		{"find has -exec", "find . -name x -delete", "", false},
		{"destructive by nature", "rm -rf build", "", false},
		// A shell wrapper we CAN see through keys on the unwrapped program, which
		// is narrower than codex's literal-prefix model: `bash -lc 'rm -rf /'`
		// still resolves to a banned program and is refused below.
		{"transparent shell wrapper keys on the payload", "bash -lc 'gcloud builds list'", "gcloud", true},
		{"transparent wrapper around a banned program", "bash -lc 'rm -rf /tmp/x'", "", false},

		// Ordinary dangerous operations stay approvable classes.
		{"chmod remains a class", "chmod 755 script.sh", "chmod", true},
		{"kill remains a class", "kill 123", "kill", true},

		// Complex shell means the classified tokens are not the tokens that run.
		{"redirection", "gcloud builds list --format=json > out.json", "", false},
		{"command substitution", "gcloud builds describe $(cat id.txt)", "", false},
		{"variable expansion", "gcloud builds list --project $PROJECT", "", false},
		{"glob", "kubectl apply -f manifests/*.yaml", "", false},
		{"heredoc", "kubectl apply -f - <<EOF\nkind: Pod\nEOF", "", false},

		// Plain single-program invocations remain grantable.
		{"plain gcloud", "gcloud builds list --project p --region us-east4", "gcloud", true},
		{"same program twice", "gcloud config list && gcloud auth list", "gcloud", true},
		{"two distinct programs", "gcloud auth list && kubectl get ns", "", false},
		{"empty payload", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			family, ok := grantCommandFamily("terminal", execArgs(tc.command))
			if ok != tc.ok {
				t.Fatalf("grantCommandFamily(%q) eligible = %v (family %q), want %v", tc.command, ok, family, tc.ok)
			}
			if ok && family != tc.family {
				t.Fatalf("grantCommandFamily(%q) family = %q, want %q", tc.command, family, tc.family)
			}
		})
	}
}

// execute_code runs a model-authored program: there is no class narrower than
// "arbitrary code", so approving it must never be remembered.
func TestGrantFloorRefusesExecuteCode(t *testing.T) {
	if _, ok := grantCommandFamily("execute_code", map[string]interface{}{
		"_tool_name": "execute_code",
		"code":       "print(1)",
	}); ok {
		t.Fatal("execute_code must not be grant-eligible")
	}
}

// The floor must remove the reusable KEY as well, so nothing downstream can
// persist a grant for an ineligible payload.
func TestApprovalPatternKeyRespectsGrantFloor(t *testing.T) {
	scope := ExecutionScope{TenantID: "default", PersonID: "p1", WorkspaceID: "ws1", WorkspaceRoot: "/tmp/ws"}
	ineligible := []string{
		"set -euo pipefail\nmkdir p\ngcloud builds list",
		"python3 -c 'import os'",
		"gcloud builds list > out.json",
		"git push origin main",
	}
	for _, command := range ineligible {
		args := map[string]interface{}{
			"_tool_name":              "terminal",
			"command":                 command,
			"_effective_sandbox_mode": string(SandboxHost),
		}
		if key := approvalPatternKeyForScope("terminal", args, "requests execution on the host outside the isolated sandbox", scope, true); key != "" {
			t.Fatalf("command %q must not produce a reusable grant key, got %q", command, key)
		}
	}

	eligible := map[string]interface{}{
		"_tool_name":              "terminal",
		"command":                 "gcloud builds list --project p",
		"_effective_sandbox_mode": string(SandboxHost),
	}
	key := approvalPatternKeyForScope("terminal", eligible, "requests execution on the host outside the isolated sandbox", scope, true)
	if key == "" {
		t.Fatal("a plain single-program host command must stay grantable")
	}
	if want := "command:gcloud"; !contains(key, want) {
		t.Fatalf("grant key %q must carry the real family %q", key, want)
	}
	for _, leaked := range []string{"command:set", "command:for", "command:unknown"} {
		if contains(key, leaked) {
			t.Fatalf("grant key %q must not key on %q", key, leaked)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
