package tools

import (
	"runtime"
	"strings"
)

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
		assessment.Enforced = enabled && runtime.GOOS == "linux" && ExecSandboxAvailable()
	}
	if networkSharedArg(args) {
		assessment.Network = containmentNetworkShared
	}
	if allowed, _ := args[credentialReadArgKey].(bool); allowed {
		assessment.Credentials = containmentCredentialsSelected
	}
	assessment.ObservationOnly = deterministicObservationExec(toolName, args)
	return assessment
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
	return strings.Join(parts, ", ")
}

// execSandboxContained is retained as the narrow compatibility predicate used
// by mode tests and retro-resolution. New approval decisions should use the
// full assessment so network and credential exposure remain observable.
func execSandboxContained(toolName string, args map[string]interface{}) bool {
	return assessExecContainment(toolName, args).AutoApprove()
}

const containedExecReason = "sandbox containment"
