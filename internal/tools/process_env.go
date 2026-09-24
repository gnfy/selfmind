package tools

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"selfmind/internal/executionenv"
)

type CredentialRef = executionenv.CredentialRef
type EnvironmentLease = executionenv.Lease

const (
	proxyModeInherited                  = "inherited"
	proxyModeOmittedForIsolatedNetwork  = "omitted_for_isolated_network"
	proxyModeOmittedUnreachableLoopback = "omitted_unreachable_loopback"
)

type processProxyDecision struct {
	Mode       string
	Suppressed int
}

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

// adaptProcessEnvForNetwork derives the proxy portion of one child environment
// from the execution plan's network view. Environment snapshots remain
// immutable; this is a per-invocation projection, just like the sandbox's
// writable and network views.
//
// An isolated network cannot reach any daemon-advertised proxy. A shared
// network can normally reach the same routes as the daemon, but a loopback
// proxy may have stopped since the snapshot was taken. In that one decidable
// case we omit the unusable proxy and record the decision, instead of making
// every CLI discover the stale listener independently. Non-loopback proxies
// are preserved: probing remote infrastructure here would add latency and
// cannot distinguish a transient failure from an intentional route policy.
func adaptProcessEnvForNetwork(parent []string, networkShared bool, reachable func(string) bool) ([]string, processProxyDecision) {
	decision := processProxyDecision{Mode: proxyModeInherited}
	if reachable == nil {
		reachable = loopbackProxyReachable
	}
	result := make([]string, 0, len(parent))
	observed := make(map[string]bool)
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !isProxyRouteVariable(name) {
			result = append(result, entry)
			continue
		}
		if !networkShared {
			decision.Mode = proxyModeOmittedForIsolatedNetwork
			decision.Suppressed++
			continue
		}
		address, loopback := loopbackProxyAddress(value)
		if !loopback {
			result = append(result, entry)
			continue
		}
		available, seen := observed[address]
		if !seen {
			available = reachable(address)
			observed[address] = available
		}
		if available {
			result = append(result, entry)
			continue
		}
		decision.Mode = proxyModeOmittedUnreachableLoopback
		decision.Suppressed++
	}
	return result, decision
}

func isProxyRouteVariable(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
		return true
	default:
		return false
	}
}

func loopbackProxyAddress(value string) (string, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", false
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false
		}
	}
	return net.JoinHostPort(host, port), true
}

func loopbackProxyReachable(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
