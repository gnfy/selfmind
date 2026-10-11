package tools

import (
	"slices"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// callWith is boundaryRun.call with extra arguments, through the same production
// order: capability resolution, then the approval funnel, around the real
// terminal tool.
func (r *boundaryRun) callWith(command string, extra map[string]interface{}) (map[string]interface{}, string, error) {
	args := map[string]interface{}{
		"_tenant_id": r.tenant, "_tool_name": "terminal", "command": command, "cwd": r.workspace, "timeout": 30,
	}
	for key, value := range extra {
		args[key] = value
	}
	body := func(args map[string]interface{}) (string, error) { return NewExecuteCommandTool().Execute(args) }
	out, err := ExecutionCapabilityMiddleware()(SmartApprovalMiddleware("")(body))(args)
	return args, out, err
}

// The network decision composes with the boundary an approval covers. A call
// that asks for the network while its credentials refresh a cache in place is
// one decision about two things: the ask shows both the shared network and the
// in-place writes, dispatch is held to those writes, the network decision is
// recorded beside them, and the hard floor still answers before either.
func TestApprovedBoundaryAndInPlaceWritesCoexistWithANetworkRequest(t *testing.T) {
	requireInPlaceBackend(t)
	// The operator policy allows the network, so a trusted workspace permits it.
	withExecSandboxPolicy(t, true, false, true)
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
	bin := writeStub(t, base, "npm", npmDeployStub)
	run := newBoundaryRun(t, "network-request-with-writes", home, bin, executionenv.TrustTrusted, ApprovalOnRequest, nil)

	// A local deploy script: the network is permitted and not needed, and the
	// ask names the writes and an unshared network.
	args, out, err := run.callWith("npm run deploy", nil)
	if err != nil || !strings.Contains(out, "cache written") || len(run.asks) != 1 {
		t.Fatalf("offline call: asks=%d err=%v (%s)", len(run.asks), err, out)
	}
	offline := run.asks[0]
	if !strings.Contains(offline.Containment, "host_credential_writes=") || !strings.Contains(offline.Containment, "network=none") {
		t.Fatalf("the offline ask must name the writes and no network: %q", offline.Containment)
	}
	if argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound || argPermission(args) != executionenv.NetworkPermissionTrust {
		t.Fatalf("offline decision: shared=%v decision=%q permission=%q", argShared(args), argDecision(args), argPermission(args))
	}
	writes, _ := args[approvedHostWritesArg].([]string)
	if len(writes) == 0 {
		t.Fatalf("the funnel did not bind the approved writes: %+v", args[approvedHostWritesArg])
	}

	// The same script asking for the network: the ask now says shared, the write
	// set is the same, the decision is recorded as shared under trust, and the
	// run still writes only what the approval covered.
	args, out, err = run.callWith("npm run deploy", map[string]interface{}{"network": true})
	if err != nil || !strings.Contains(out, "cache written") || len(run.asks) != 2 {
		t.Fatalf("networked call: asks=%d err=%v (%s)", len(run.asks), err, out)
	}
	networked := run.asks[1]
	if !strings.Contains(networked.Containment, "network=shared") || !strings.Contains(networked.Containment, "host_credential_writes=") {
		t.Fatalf("the networked ask must name the shared network and the writes: %q", networked.Containment)
	}
	if networked.Containment == offline.Containment {
		t.Fatalf("a shared network and an unshared one are one boundary: %q", networked.Containment)
	}
	if !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared || argPermission(args) != executionenv.NetworkPermissionTrust {
		t.Fatalf("networked decision: shared=%v decision=%q permission=%q", argShared(args), argDecision(args), argPermission(args))
	}
	if again, _ := args[approvedHostWritesArg].([]string); !slices.Equal(again, writes) {
		t.Fatalf("the networked call is bound to other writes: %v vs %v", again, writes)
	}
	if !strings.Contains(networked.Reason, "in place") {
		t.Fatalf("the ask must lead with the in-place writes it enables: %q", networked.Reason)
	}

	// The hard floor answers before any of it: nothing is asked.
	asked := len(run.asks)
	if _, _, err := run.callWith("rm -rf /", map[string]interface{}{"network": true}); err == nil || !strings.Contains(err.Error(), "blocked by safety policy") || len(run.asks) != asked {
		t.Fatalf("hard floor: err=%v asks=%d", err, len(run.asks)-asked)
	}
}
