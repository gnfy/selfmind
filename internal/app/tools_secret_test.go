package app

import (
	"strings"
	"testing"

	"selfmind/internal/tools"
)

func TestRegisterSensitiveHeadersOnlyRegistersCredentialHeaders(t *testing.T) {
	const (
		secretValue = "opaque-auth-header-value-7d102e"
		bearerValue = "opaque-bearer-payload-7d102e"
		normalValue = "application/vnd.selfmind-test-7d102e+json"
	)
	registerSensitiveHeaders(map[string]string{
		"Authorization": secretValue,
		"X-Auth-Token":  "Bearer " + bearerValue,
		"Content-Type":  normalValue,
	})

	if got := tools.RedactSensitive("value=" + secretValue); strings.Contains(got, secretValue) {
		t.Fatalf("credential header value leaked: %q", got)
	}
	if got := tools.RedactSensitive("value=" + normalValue); !strings.Contains(got, normalValue) {
		t.Fatalf("ordinary header value was over-redacted: %q", got)
	}
	if got := tools.RedactSensitive("value=" + bearerValue); strings.Contains(got, bearerValue) {
		t.Fatalf("bare bearer payload leaked: %q", got)
	}
}
