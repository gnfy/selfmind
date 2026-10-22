package tools

import (
	"strings"
	"sync/atomic"
)

// containmentAssessments counts derivations. The approval funnel derives one
// per call and hands it to every decision that needs it; a test pins that.
var containmentAssessments atomic.Int64

// ContainmentAssessment keeps the three execution boundaries independent.
// Collapsing them into one bool made a network-enabled filesystem sandbox look
// identical to host execution, even though their blast radii are very
// different. The assessment is derived from the effective execution request;
// it is never inferred from a pending approval's redacted display payload.
type ContainmentAssessment struct {
	Filesystem      string `json:"filesystem"`
	Network         string `json:"network"`
	Credentials     string `json:"credentials"`
	Enforced        bool   `json:"enforced"`
	ObservationOnly bool   `json:"observation_only,omitempty"`
	// HostCredentialWrites are the declared credential-state subpaths an
	// enforcing backend that cannot mount makes writable in place on the host.
	// They are part of the boundary the judge and the person approve.
	HostCredentialWrites []string `json:"host_credential_writes,omitempty"`
}

const (
	containmentFilesystemIsolated  = "isolated"
	containmentFilesystemHost      = "host"
	containmentFilesystemUnknown   = "unknown"
	containmentNetworkNone         = "none"
	containmentNetworkShared       = "shared"
	containmentCredentialsNone     = "none"
	containmentCredentialsSelected = "selected"
)

// assessExecContainment describes what THIS exec call can reach. The effective
// sandbox annotation is required so /mode retro-resolution can never infer that
// a pending host escape was isolated.
func assessExecContainment(toolName string, args map[string]interface{}) ContainmentAssessment {
	containmentAssessments.Add(1)
	assessment := ContainmentAssessment{
		Filesystem:  containmentFilesystemUnknown,
		Network:     containmentNetworkNone,
		Credentials: containmentCredentialsNone,
	}
	if !isExecTool(toolName) {
		return assessment
	}

	switch SandboxMode(strings.ToLower(strings.TrimSpace(stringArg(args, "_effective_sandbox_mode")))) {
	case SandboxHost:
		assessment.Filesystem = containmentFilesystemHost
	case SandboxIsolated:
		assessment.Filesystem = containmentFilesystemIsolated
		enabled, _, _ := execSandboxPolicyForArgs(args)
		assessment.Enforced = enabled && ExecSandboxAvailable()
		if assessment.Enforced && !SandboxBackendForMode(SandboxIsolated).MountsToolState() {
			// The same plan preparation will make, under the credential
			// decision this call actually holds.
			if plan, err := previewHostStateWrites(toolName, args, false); err == nil {
				assessment.HostCredentialWrites = plan.Paths()
			}
		}
	}
	if networkSharedArg(args) {
		assessment.Network = containmentNetworkShared
	}
	// Credentials are what the call's sandbox carries: the credential:read
	// decision for a tool it names, or the operator state preparation hands an
	// undecidable payload from the host inventory.
	if credentialReadResolved(args) || receivesOperatorCredentials(toolName, args) {
		assessment.Credentials = containmentCredentialsSelected
	}
	assessment.ObservationOnly = observationOnlyExec(toolName, args)
	if assessment.ObservationOnly && assessment.Network == containmentNetworkShared && networkSharedByRequestAlone(toolName, args) {
		// The release for a proven observation rests on the network being the
		// observation's own: `kubectl get pods` reaches a cluster by purpose.
		// When the command says it is local (`ls`, `git status`) and the network
		// is shared only because the call asked for it, nothing proves the
		// request is for the observation; the model chose it, and it reaches the
		// funnel as any other networked call does.
		assessment.ObservationOnly = false
	}
	return assessment
}

// networkSharedByRequestAlone reports a call whose command does not name a
// remote service and whose network is therefore explained only by its explicit
// `network: true`.
func networkSharedByRequestAlone(toolName string, args map[string]interface{}) bool {
	return networkRequestedByCall(toolName, args) && !commandPlausiblyNeedsEgress(toolName, args)
}

// AutoApprove reports whether smart mode may skip both judge and human. A fully
// isolated no-network/no-credential call retains the original C1 behaviour.
// Shared-network or credential-bearing calls are released only when a bounded,
// declarative command rule proves they are observation-only. Arbitrary scripts
// and unknown agent CLIs never satisfy that rule.
func (a ContainmentAssessment) AutoApprove() bool {
	if a.Filesystem != containmentFilesystemIsolated || !a.Enforced {
		return false
	}
	if a.Network == containmentNetworkNone && a.Credentials == containmentCredentialsNone {
		return true
	}
	return a.ObservationOnly
}

func (a ContainmentAssessment) Summary() string {
	parts := []string{
		"filesystem=" + a.Filesystem,
		"network=" + a.Network,
		"credentials=" + a.Credentials,
	}
	if a.ObservationOnly {
		parts = append(parts, "operation=observation-only")
	}
	if !a.Enforced && a.Filesystem == containmentFilesystemIsolated {
		parts = append(parts, "enforcement=unavailable")
	}
	if len(a.HostCredentialWrites) > 0 {
		parts = append(parts, "host_credential_writes="+strings.Join(a.HostCredentialWrites, ";"))
	}
	return strings.Join(parts, ", ")
}

// execSandboxContained is retained as the narrow compatibility predicate used
// by mode tests and retro-resolution. New approval decisions should use the
// full assessment so network and credential exposure remain observable.
func execSandboxContained(toolName string, args map[string]interface{}) bool {
	return assessExecContainment(toolName, args).AutoApprove()
}

const containedExecReason = "sandbox containment"
