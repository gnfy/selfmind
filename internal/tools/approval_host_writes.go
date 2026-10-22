package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// In-place credential writes are part of the boundary an approval covers.
//
// On a backend that cannot mount, a call whose operator credentials refresh a
// cache writes the operator's real cache in place (envprofiles
// PlanHostStateWrites). The containment summary names those paths, so an
// exact decision is already bound to them; a remembered class, rule or phase
// bundle was not, and one approved while the configuration needed no writes
// went on releasing the call after it started writing the real SSO cache. The
// approval now carries the write set three ways: reusable keys are bound to it,
// the reason every surface truncates leads with it, and dispatch refuses a plan
// that writes outside what the releasing decision covered.

// approvedHostWritesArg carries the in-place credential writes the releasing
// decision covered from the approval funnel to the execution engine.
const approvedHostWritesArg = "_approved_host_credential_writes"

// GrantKeyHostWritesMarker binds a reusable approval key to the in-place
// credential writes it was approved with. It is part of the stored key.
const GrantKeyHostWritesMarker = "|host_writes="

// hostCredentialWritesBinding is the key suffix for a write set, or "" when
// the call writes no host credential state.
func hostCredentialWritesBinding(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return GrantKeyHostWritesMarker + hex.EncodeToString(sum[:8])
}

func bindHostCredentialWrites(key, binding string) string {
	if key == "" || binding == "" {
		return key
	}
	return key + binding
}

// bindRuleCandidatesToHostWrites binds each offered rule, key and label, so a
// rule remembered for one write set never releases another.
func bindRuleCandidatesToHostWrites(candidates []ApprovalRuleCandidate, binding string, display []string) []ApprovalRuleCandidate {
	if binding == "" || len(candidates) == 0 {
		return candidates
	}
	out := make([]ApprovalRuleCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.Key = bindHostCredentialWrites(candidate.Key, binding)
		candidate.Label = describeHostCredentialWritesGrant(candidate.Label, binding, display)
		out = append(out, candidate)
	}
	return out
}

// describeHostCredentialWritesGrant states in a remember option's label that it
// covers only these in-place writes.
func describeHostCredentialWritesGrant(label, binding string, display []string) string {
	if strings.TrimSpace(label) == "" || binding == "" {
		return label
	}
	return label + " while writing " + strings.Join(display, ", ") + " in place"
}

// compactHostPaths renders host paths for a person: under the run's HOME as
// `~/…`, short enough to survive every surface's truncation.
func compactHostPaths(args map[string]interface{}, paths []string) []string {
	return compactPathsUnderHome(envValue(leaseProcessEnv(args), "HOME"), paths)
}

// compactPathsUnderHome renders paths under home as `~/…`.
func compactPathsUnderHome(home string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	home = strings.TrimSpace(home)
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if home != "" && home != string(filepath.Separator) {
			if rel, err := filepath.Rel(home, path); err == nil && rel != "." && rel != ".." &&
				!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				path = "~/" + filepath.ToSlash(rel)
			}
		}
		out = append(out, path)
	}
	return out
}

// approvalBoundaryFact is the boundary fact an approver must not miss: the
// credential caches a sandboxed call writes in place, or that the call runs on
// the host although it did not ask to, and why.
func approvalBoundaryFact(args map[string]interface{}, containment ContainmentAssessment) string {
	if fact := hostCredentialWritesFact(args, containment.HostCredentialWrites); fact != "" {
		return fact
	}
	if fallback := strings.TrimSpace(stringArg(args, effectiveSandboxReasonArg)); fallback != "" && effectiveSandboxModeArg(args) == SandboxHost {
		return "runs on the host: " + fallback
	}
	return ""
}

// hostCredentialWritesFact states the in-place credential writes for a person,
// or "" when there are none.
func hostCredentialWritesFact(args map[string]interface{}, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "writes credential caches in place: " + strings.Join(compactHostPaths(args, paths), ", ")
}

// prependApprovalReason puts a fact before the review facts already gathered,
// keeping every one of them.
func prependApprovalReason(fact, reason string) string {
	fact, reason = strings.TrimSpace(fact), strings.TrimSpace(reason)
	switch {
	case fact == "":
		return reason
	case reason == "":
		return fact
	case strings.HasPrefix(reason, fact):
		return reason
	}
	return fact + "; " + reason
}

// approvedHostWritesCover refuses a plan that writes host credential state the
// releasing decision did not cover — the configuration changed between
// approval and dispatch. A call no approval funnel resolved carries no record
// and is not checked here.
func approvedHostWritesCover(args map[string]interface{}, planned []string) error {
	approved, recorded := args[approvedHostWritesArg].([]string)
	if !recorded || len(planned) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(approved))
	for _, path := range approved {
		covered[path] = true
	}
	var extra []string
	for _, path := range planned {
		if !covered[path] {
			extra = append(extra, path)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	paths := strings.Join(compactHostPaths(args, extra), ", ")
	return newStableToolRecoveryError(fmt.Errorf("in-place credential writes were not approved: %s", paths),
		"approved_host_writes_changed", "environment",
		"This call's approval did not cover writing "+paths+" in place; the host's credential configuration changed after it was approved. It was not dispatched.",
		"No process was started. Calling again asks for approval of the boundary as it is now.",
		"preparation", "after_state_change", "not_dispatched", false)
}
