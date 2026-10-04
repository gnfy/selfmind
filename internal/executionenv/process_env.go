package executionenv

import "strings"

// ProcessEnvPolicy is the shared child-process environment boundary. The
// caller may observe omitted control-plane values for redaction registration;
// no such values are returned to the child.
type ProcessEnvPolicy struct {
	StripControlPlane bool
	OnOmitControl     func(name, value string)
}

func DefaultProcessEnvPolicy() ProcessEnvPolicy {
	return ProcessEnvPolicy{StripControlPlane: true}
}

func BuildProcessEnv(parent []string, policy ProcessEnvPolicy) []string {
	out := make([]string, 0, len(parent))
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if policy.StripControlPlane && IsControlPlaneEnv(name) {
			if policy.OnOmitControl != nil {
				policy.OnOmitControl(name, value)
			}
			continue
		}
		out = append(out, entry)
	}
	return out
}

func IsControlPlaneEnv(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(upper, "SELF_") || strings.HasPrefix(upper, "SELFMIND_")
}
