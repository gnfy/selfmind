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

func currentToolProcessEnv() []string {
	return BuildProcessEnv(os.Environ(), DefaultProcessEnvPolicy())
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
