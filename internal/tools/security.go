package tools

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*Bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
}

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)((?:[A-Za-z0-9]+[_-])*(?:api[_-]?key|token|secret|password|credential|authorization))(\s*[:=]\s*)(["']?)([^\s"',}]+)`)

// SecretRegistry holds exact runtime secret values that cannot be recognized
// reliably by shape alone (for example an opaque gateway token). It is shared
// by write-time and read-time redaction: callers register credentials when
// configuration or an execution environment is materialized, while every
// existing RedactSensitive call automatically gains exact-value masking.
//
// Values intentionally remain registered for the life of the process. That
// keeps historical output safe after credential rotation and avoids a window
// where an old value becomes visible again.
type SecretRegistry struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

func NewSecretRegistry() *SecretRegistry {
	return &SecretRegistry{values: map[string]struct{}{}}
}

// Register adds an opaque secret value. Very short values are ignored because
// replacing common fragments such as "yes" or "1234" would destroy ordinary
// diagnostics without adding meaningful protection.
func (r *SecretRegistry) Register(value string) {
	if r == nil {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) < 6 {
		return
	}
	r.mu.Lock()
	r.values[value] = struct{}{}
	r.mu.Unlock()
}

func (r *SecretRegistry) Redact(value string) string {
	if r == nil || value == "" {
		return value
	}
	r.mu.RLock()
	values := make([]string, 0, len(r.values))
	for secret := range r.values {
		values = append(values, secret)
	}
	r.mu.RUnlock()
	// Mask longer values first so overlapping credentials cannot leave a
	// recognizable suffix behind.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	out := value
	for _, secret := range values {
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}

var defaultSecretRegistry = NewSecretRegistry()

// RegisterSensitiveValue makes an exact credential value available to the
// process-wide redactor. The value itself is never persisted by this API.
func RegisterSensitiveValue(value string) {
	defaultSecretRegistry.Register(value)
}

func RedactSensitive(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	out := defaultSecretRegistry.Redact(value)
	out = sensitivePatterns[0].ReplaceAllString(out, `${1}[REDACTED]`)
	out = redactSensitiveAssignments(out)
	out = sensitivePatterns[1].ReplaceAllString(out, `${1}[REDACTED]`)
	out = sensitivePatterns[2].ReplaceAllString(out, `sk-[REDACTED]`)
	return out
}

func redactSensitiveAssignments(value string) string {
	return sensitiveAssignmentPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := sensitiveAssignmentPattern.FindStringSubmatch(match)
		if len(parts) != 5 || isCredentialReference(parts[4]) ||
			(strings.EqualFold(parts[1], "authorization") && strings.EqualFold(parts[4], "bearer")) {
			return match
		}
		return parts[1] + parts[2] + parts[3] + "[REDACTED]"
	})
}

func isCredentialReference(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "$(") || strings.HasPrefix(value, "${") ||
		strings.HasPrefix(value, "$") || strings.HasPrefix(value, "`") ||
		(strings.HasPrefix(value, "%") && strings.HasSuffix(value, "%"))
}

// RedactionMiddleware is the single tool-result exit boundary. The redacted
// value is what the model, event preview, artifact spool, and downstream
// formatters receive.
func RedactionMiddleware() Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			output, err := next(args)
			output = RedactSensitive(output)
			if err == nil {
				return output, nil
			}
			redacted := RedactSensitive(err.Error())
			if redacted == err.Error() {
				return output, err
			}
			return output, errors.New(redacted)
		}
	}
}
