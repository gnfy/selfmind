package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"

	"selfmind/internal/executionenv"
)

type CredentialRef = executionenv.CredentialRef
type EnvironmentLease = executionenv.Lease

// ProcessEnvPolicy controls which daemon variables may reach a child process.
// Operator tool credentials are deliberately preserved in this first cut:
// arbitrary Agent CLIs commonly rely on environment-based login. The strict
// boundary here is daemon/control-plane state, which a child never needs.
type ProcessEnvPolicy struct {
	StripControlPlane bool
}

func DefaultProcessEnvPolicy() ProcessEnvPolicy {
	return ProcessEnvPolicy{StripControlPlane: true}
}

// BuildProcessEnv is the single construction path for tool child-process
// environments. It preserves the user's toolchain and operator credentials
// while preventing nested commands from inheriting SelfMind's own gateway
// identity/token. Stripped values are registered for output redaction.
func BuildProcessEnv(parent []string, policy ProcessEnvPolicy) []string {
	out := make([]string, 0, len(parent))
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if policy.StripControlPlane && isSelfMindControlEnv(name) {
			if isCredentialShapedName(name) {
				RegisterSensitiveValue(value)
			}
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isSelfMindControlEnv(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(upper, "SELF_") || strings.HasPrefix(upper, "SELFMIND_")
}

// InstallEnvironmentSnapshot samples the operator environment, filters it
// through the single construction path, and installs it as the process's
// current snapshot. It is the ONLY place a snapshot is created, so the filter
// can never be bypassed. Credential-shaped values are registered with the
// redactor and their names become the snapshot's credential sources.
func InstallEnvironmentSnapshot(parent []string, source string) *executionenv.Snapshot {
	refs, principal := SnapshotCredentialRefs(parent)
	sources := make([]string, 0, len(refs))
	for _, ref := range refs {
		sources = append(sources, ref.Kind+":"+ref.Source)
	}
	filtered := BuildProcessEnv(parent, DefaultProcessEnvPolicy())
	return executionenv.DefaultRegistry().Install(filtered, source, principal, sources)
}

// SampleEnvironmentSnapshot builds a snapshot without installing it, so a
// caller can compare a fresh reading against the binding a run already holds.
func SampleEnvironmentSnapshot(parent []string, source string) *executionenv.Snapshot {
	refs, principal := SnapshotCredentialRefs(parent)
	sources := make([]string, 0, len(refs))
	for _, ref := range refs {
		sources = append(sources, ref.Kind+":"+ref.Source)
	}
	filtered := BuildProcessEnv(parent, DefaultProcessEnvPolicy())
	return executionenv.DefaultRegistry().Sample(filtered, source, principal, sources)
}

// leaseProcessEnv resolves the child environment for one tool call through the
// run's lease. Reading os.Environ() at the execution callsite is what let a
// long-lived daemon hand a stale or mid-run-changed environment to a child; the
// lease binding makes every command of a run — including a retry — use the same
// values. Falling back to a fresh filtered sample keeps non-run callers (local
// CLI helpers, tests) working, and is still built through BuildProcessEnv.
func leaseProcessEnv(args map[string]interface{}) []string {
	if scope, ok := currentExecutionScopeAny(args); ok {
		if snapshot, found := executionenv.DefaultRegistry().ForLease(scope.LeaseID); found {
			return snapshot.Env()
		}
		if snapshot, found := executionenv.DefaultRegistry().Get(scope.EnvironmentSnapshotID); found {
			return snapshot.Env()
		}
	}
	if snapshot := executionenv.DefaultRegistry().Current(); snapshot != nil {
		return snapshot.Env()
	}
	return BuildProcessEnv(os.Environ(), DefaultProcessEnvPolicy())
}

// currentToolProcessEnv is the no-scope fallback used by callers that have no
// request context at all.
func currentToolProcessEnv() []string {
	return leaseProcessEnv(nil)
}

// SnapshotCredentialRefs derives durable, non-secret references from the
// operator environment. Credential-shaped values are registered with the
// runtime redactor but never returned or persisted.
func SnapshotCredentialRefs(parent []string) ([]CredentialRef, string) {
	refs := make([]CredentialRef, 0)
	principalParts := make([]string, 0)
	seen := map[string]bool{}
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || isSelfMindControlEnv(name) {
			continue
		}
		upper := strings.ToUpper(name)
		if isCredentialShapedName(upper) {
			RegisterSensitiveValue(value)
			if !seen[upper] {
				refs = append(refs, CredentialRef{Kind: "environment", Source: upper})
				seen[upper] = true
			}
			continue
		}
		if isPrincipalCarrierName(upper) && strings.TrimSpace(value) != "" {
			principalParts = append(principalParts, upper+"="+strings.TrimSpace(value))
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Source < refs[j].Source })
	sort.Strings(principalParts)
	if len(principalParts) == 0 {
		for _, ref := range refs {
			principalParts = append(principalParts, ref.Kind+":"+ref.Source)
		}
	}
	if len(principalParts) == 0 {
		return refs, ""
	}
	sum := sha256.Sum256([]byte(strings.Join(principalParts, "\n")))
	return refs, fmt.Sprintf("%x", sum[:12])
}

func isCredentialShapedName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	switch name {
	case "DATABASE_URL", "REDIS_URL":
		return true
	}
	for _, suffix := range []string{"_DSN", "_PAT", "_KEY"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isPrincipalCarrierName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"PROFILE", "ACCOUNT", "USER", "USERNAME", "PROJECT", "CONTEXT", "HOST"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// resolvedSnapshotForArgs returns the snapshot a call will actually use, so a
// plan can name its binding instead of leaving the field empty.
func resolvedSnapshotForArgs(args map[string]interface{}) *executionenv.Snapshot {
	registry := executionenv.DefaultRegistry()
	if scope, ok := currentExecutionScopeAny(args); ok {
		if snapshot, found := registry.ForLease(scope.LeaseID); found {
			return snapshot
		}
		if snapshot, found := registry.Get(scope.EnvironmentSnapshotID); found {
			return snapshot
		}
	}
	return registry.Current()
}
