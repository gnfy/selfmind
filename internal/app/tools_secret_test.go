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

func TestRegisterSensitiveExtrasProtectsCredentialLikeValues(t *testing.T) {
	const secret = "opaque-extra-query-token-7d102e"
	registerSensitiveExtras(map[string]interface{}{
		"user_id":  "ordinary-user-id",
		"metadata": map[string]interface{}{"access_token": secret},
	})
	if got := tools.RedactSensitive("value=" + secret); strings.Contains(got, secret) {
		t.Fatalf("extra option credential leaked: %q", got)
	}
}
