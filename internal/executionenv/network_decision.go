package executionenv

// Network decision vocabulary.
//
// A network:shared capability, a trusted workspace and the operator policy are
// PERMISSIONS: they say the daemon's network may be shared with a command. They
// are not a reason to share it. Each execution call therefore records what the
// runtime decided and why, so a command that ran offline in a workspace that
// permits network is a fact on the record and not a silent reduction in what a
// passing check covered.
const (
	// NetworkDecisionShared: the call ran with the daemon's network.
	NetworkDecisionShared = "shared"
	// NetworkWithheldNotBound: the workspace permits network, and the command
	// neither names a remote service nor asked for network, so it ran offline.
	NetworkWithheldNotBound = "withheld: not network-bound by policy"
	// NetworkWithheldPolicyDisabled: the operator policy (exec_sandbox.allow_network
	// false) closes the trust-derived path and no grant or approval opens it.
	NetworkWithheldPolicyDisabled = "withheld: policy disabled"
	// NetworkWithheldNoGrant: nothing permits network for this workspace.
	NetworkWithheldNoGrant = "withheld: no grant"
)

// Where the permission for a call's network came from. It says what permitted
// the network at the time of the call, not that the call used it.
const (
	// NetworkPermissionTrust: a trusted workspace under a policy that allows it.
	NetworkPermissionTrust = "trust"
	// NetworkPermissionGrant: a stored person/workspace grant or a run grant.
	NetworkPermissionGrant = "grant"
	// NetworkPermissionApproval: a one-time approval answered for this call.
	NetworkPermissionApproval = "approval"
)

// NetworkOfflineDespiteGrant reports a call that ran without network although
// a network grant permitted it. Trust is a standing default, not a grant someone
// gave: running local work offline under it changes nothing a person relied on.
func NetworkOfflineDespiteGrant(decision, permission string) bool {
	return decision == NetworkWithheldNotBound && permission == NetworkPermissionGrant
}
