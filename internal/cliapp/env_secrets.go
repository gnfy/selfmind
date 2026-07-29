package cliapp

import "strings"

// isCredentialShapedEnvName mirrors the tool layer's credential-shape test. It is
// duplicated here rather than imported so the service installer cannot be made
// to depend on tool internals; both lists describe the same naming conventions
// and both must stay conservative.
func isCredentialShapedEnvName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY",
		"CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY", "SESSION_KEY",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// valueEmbedsCredentials reports whether a value carries inline credentials, the
// classic case being a proxy URL of the form scheme://user:password@host. Such a
// value must never be written to a world-readable service definition.
func valueEmbedsCredentials(value string) bool {
	scheme := strings.Index(value, "://")
	if scheme < 0 {
		return false
	}
	rest := value[scheme+len("://"):]
	authority := rest
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 {
		authority = rest[:slash]
	}
	return strings.Contains(authority, "@")
}
