package tools

import (
	"context"
	"testing"

	"selfmind/internal/control"
)

// classAsks records every human ask and answers the first one with "remember
// for this workspace"; later asks are approved once.
type classAsks struct {
	requests []ToolApprovalRequest
}

func (a *classAsks) answer(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
	a.requests = append(a.requests, req)
	if len(a.requests) == 1 {
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr-class", Scope: "workspace"}, nil
	}
	return ToolApprovalDecision{Approved: true, ApprovalID: "apr-once"}, nil
}

// classKeyScope installs a sandboxed, NOT enforced execution boundary, so smart
// mode reaches the human ask instead of releasing a call as contained, and a
// real control store, whose workspace-row lookup is what a standing answer is
// redeemed through. The funnel itself refuses an isolated boundary it cannot
// enforce before any ask, so runClassKeyCall pins the resolved boundary
// directly: these tests are about the class key, not about enforcement.
func classKeyScope(t *testing.T, mode ApprovalMode, asks *classAsks) (string, *control.Store) {
	t.Helper()
	withExecSandboxPolicy(t, false, true, false)
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	person := "person-class-" + string(mode)
	scope := ExecutionScope{
		TenantID: "tenant-class", PersonID: person, TaskID: "task-class", RunID: "run-class-" + string(mode),
		WorkspaceID: "ws-class", WorkspaceRoot: t.TempDir(),
		ApprovalMode: mode, Approval: asks.answer, Grants: store, StandingGrants: InteractiveStandingGrants(),
	}
	if mode == ApprovalSmart {
		scope.Judge = &fakeJudge{reply: `{"outcome":"escalate","risk_level":"low","user_authorization":"unknown","rationale":"The person did not name this command."}`}
	}
	t.Cleanup(SetExecutionScope(person, scope))
	return person, store
}

func runClassKeyCall(person, command string) (ran bool, err error) {
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	_, err = exec(map[string]interface{}{
		"_tenant_id": person, "_tool_name": "terminal", "command": command,
		"_effective_sandbox_mode": string(SandboxIsolated),
	})
	return ran, err
}

// The 2026-10-06 review reproduction: in on-request mode the remembered key was
// built from the mode text, `exec:terminal requires approval in on-request
// mode`, and in smart mode it was `exec:terminal`, so remembering one sandboxed
// `kubectl get pods` released every later terminal command, `kubectl delete`
// and `terraform destroy` included, while the prompt named the mode or a
// `kubectl get` class. The key now IS the command class the prompt names.
func TestRememberedSandboxedClassCoversOnlyItsCommandClass(t *testing.T) {
	for _, mode := range []ApprovalMode{ApprovalOnRequest, ApprovalSmart} {
		t.Run(string(mode), func(t *testing.T) {
			asks := &classAsks{}
			person, _ := classKeyScope(t, mode, asks)
			if ran, err := runClassKeyCall(person, "kubectl get pods"); !ran || err != nil || len(asks.requests) != 1 {
				t.Fatalf("first call must ask once and run: ran=%v err=%v asks=%d", ran, err, len(asks.requests))
			}
			if got := asks.requests[0].GrantClass; got != `"kubectl get" commands` {
				t.Errorf("the remember option must name the command class it stores, got %q", got)
			}
			// The same class, with other targets or behind a wrapper that changes
			// neither who runs it nor how it is read, is released.
			for _, command := range []string{"kubectl get svc", "kubectl get deployment web -o yaml", "time kubectl get nodes"} {
				before := len(asks.requests)
				if ran, err := runClassKeyCall(person, command); !ran || err != nil || len(asks.requests) != before {
					t.Errorf("%q is in the remembered class and must not ask: ran=%v err=%v asks=%d", command, ran, err, len(asks.requests)-before)
				}
			}
			// Every other class asks again, including the same read run as root.
			for _, command := range []string{"kubectl delete deployment web", "terraform destroy", "kubectl apply -f deploy.yaml", "sudo kubectl get pods"} {
				before := len(asks.requests)
				_, _ = runClassKeyCall(person, command)
				if len(asks.requests) != before+1 {
					t.Errorf("%q was released by the remembered \"kubectl get\" class", command)
				}
			}
		})
	}
}

// The variant changes the program and the verb: the class is whatever the
// person was shown, so a remembered read of one service never covers that
// service's write, nor a read of another one.
func TestRememberedSandboxedClassVariantFollowsTheShownClass(t *testing.T) {
	asks := &classAsks{}
	person, _ := classKeyScope(t, ApprovalOnRequest, asks)
	if ran, _ := runClassKeyCall(person, "gcloud builds list --project p"); !ran {
		t.Fatal("first call must run after the ask")
	}
	if got := asks.requests[0].GrantClass; got != `"gcloud builds list" commands` {
		t.Errorf("label = %q", got)
	}
	before := len(asks.requests)
	if ran, _ := runClassKeyCall(person, "gcloud builds list --project q --limit 5"); !ran || len(asks.requests) != before {
		t.Fatal("the same class must be released")
	}
	for _, command := range []string{"gcloud builds submit --tag x", "gcloud run services list", "helm list"} {
		before := len(asks.requests)
		_, _ = runClassKeyCall(person, command)
		if len(asks.requests) != before+1 {
			t.Errorf("%q was released by the remembered \"gcloud builds list\" class", command)
		}
	}
}

// A call the floor cannot classify offers no remember option, and a client that
// answers "workspace" anyway stores nothing.
func TestCallWithoutClassOffersNoRememberOption(t *testing.T) {
	for _, mode := range []ApprovalMode{ApprovalOnRequest, ApprovalSmart} {
		t.Run(string(mode), func(t *testing.T) {
			asks := &classAsks{}
			person, store := classKeyScope(t, mode, asks)
			for _, command := range []string{
				"sudo kubectl get pods",
				"env KUBECONFIG=/tmp/x kubectl get pods",
				"bash -c 'kubectl get pods'",
				`\kubectl get pods`,
				"NODE_OPTIONS=--require=./hook.js kubectl get pods",
				"$KUBECTL get pods",
				"python3 deploy.py",
				"hdparm -I /dev/sda",
			} {
				before := len(asks.requests)
				_, _ = runClassKeyCall(person, command)
				if len(asks.requests) != before+1 {
					t.Fatalf("%q must ask", command)
				}
				if class := asks.requests[len(asks.requests)-1].GrantClass; class != "" {
					t.Errorf("%q offered a remember option %q although it has no class", command, class)
				}
			}
			grants, err := store.ListApprovalGrants(context.Background(), "tenant-class", person, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(grants) != 0 {
				t.Fatalf("a call without a class stored %d grants: %+v", len(grants), grants)
			}
		})
	}
}

// The host key no longer carries the reason, so it must not let an old row
// release a command that is dangerous for its own sake: on the host such a
// call has no class at all, whatever the store holds.
func TestHostKeyNeverReleasesACommandDangerousForItsOwnSake(t *testing.T) {
	store := newFakeGrantStore()
	scope := ExecutionScope{
		TenantID: "tenant-host-class", PersonID: "person-host-class", WorkspaceID: "ws-host-class", WorkspaceRoot: "/work/host",
		ApprovalMode: ApprovalOnRequest, Grants: store, StandingGrants: InteractiveStandingGrants(),
	}
	asks := 0
	scope.Approval = func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr-host"}, nil
	}
	t.Cleanup(SetExecutionScope("person-host-class", scope))
	for _, command := range []string{"ssh build.example.test uptime", "chmod 755 script.sh", "kill 4242"} {
		args := map[string]interface{}{"_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(SandboxHost)}
		// A row in the host shape for this class, as an older derivation or a
		// hand-made grant could leave it.
		stored := hostGrantKeyBase + grantKeyResourceMarker + approvalResourceFingerprint(scope, "terminal", args)
		store.granted[store.key("person", scope.PersonID, stored)] = true
		if _, reason := dangerousToolCall("", "terminal", args); approvalPatternKeyForScope("terminal", args, reason, scope, true) != "" {
			t.Errorf("%q minted a host class", command)
		}
		before := asks
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
		run := map[string]interface{}{"_tenant_id": "person-host-class", "_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(SandboxHost)}
		if _, err := exec(run); err != nil || asks != before+1 {
			t.Errorf("%q was released by a stored host row: err=%v asks=%d", command, err, asks-before)
		}
		if _, keep := ReviewPersistedGrantKey(stored); keep {
			t.Errorf("review kept the unreachable host row for %q", command)
		}
	}
	// The constraint that must change the result: an ordinary host class is
	// still remembered and released.
	args := map[string]interface{}{"_tool_name": "terminal", "command": "kubectl get pods", "_effective_sandbox_mode": string(SandboxHost)}
	_, reason := dangerousToolCall("", "terminal", args)
	if key := approvalPatternKeyForScope("terminal", args, reason, scope, true); key == "" {
		t.Fatal("kubectl get must keep its host class")
	} else if _, keep := ReviewPersistedGrantKey(key); !keep {
		t.Fatalf("the host class %q must survive review", key)
	}
}

// The boot review never rewrites a stored key into the class form, which could
// match more than the person approved: a tool-wide key, a mode or reason text
// standing in for a class, is withdrawn, while a class key minted today stays.
func TestBootReviewWithdrawsToolWideExecKeys(t *testing.T) {
	for _, key := range []string{
		"exec:terminal requires approval in on-request mode",
		"exec:terminal requires approval in smart mode",
		"exec:verify requires approval in auto-edit mode",
		"exec:terminal",
		"exec:shell",
		"exec:invokes an opaque wrapped command",
		"exec:contains dangerous pattern: /dev/sd",
		"exec:network egress via /dev/tcp/",
	} {
		if family, keep := ReviewPersistedGrantKey(key); keep {
			t.Errorf("ReviewPersistedGrantKey(%q) kept a key that covers no single class (family %q)", key, family)
		}
	}
	args := map[string]interface{}{"_tool_name": "terminal", "command": "kubectl get pods", "_effective_sandbox_mode": string(SandboxIsolated)}
	minted := approvalPatternKeyForScope("terminal", args, "", ExecutionScope{WorkspaceID: "ws-class"}, true)
	if minted == "" {
		t.Fatal("kubectl get must mint a class key")
	}
	if family, keep := ReviewPersistedGrantKey(minted); !keep || family != "kubectl get" {
		t.Fatalf("a class key minted today must survive review: %q -> %q %v", minted, family, keep)
	}
	// The reason a call was asked about never changes its key.
	for _, reason := range []string{"terminal requires approval in on-request mode", "terminal requires approval in smart mode", ""} {
		if key := approvalPatternKeyForScope("terminal", args, reason, ExecutionScope{WorkspaceID: "ws-class"}, true); key != minted {
			t.Errorf("reason %q changed the class key: %q vs %q", reason, key, minted)
		}
	}
}
