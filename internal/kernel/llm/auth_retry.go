package llm

import (
	"encoding/json"
	"net/http"
	"strings"
)

// apiKeyFrom resolves the credential for one request. An empty answer from the
// dynamic getter means "no per-tenant override for this provider", NOT "the
// credential was revoked" — see buildKeyGetter, whose empty return is the
// documented signal to use the adapter's configured key. Treating it as
// revocation sends an empty key for every provider whose credential lives in
// config or the auth file rather than the tenant secret store, which is the
// ordinary case: the daemon answered HTTP 401 on the first turn after such a
// change.
//
// Revocation is enforced where a credential actually lives: the auth manager
// drops a cached token whose source is gone, so a logout stops the token being
// served rather than being inferred from an empty override.
func apiKeyFrom(static string, getter func() string) string {
	if getter != nil {
		if key := strings.TrimSpace(getter()); key != "" {
			return key
		}
	}
	return strings.TrimSpace(static)
}

func refreshedAPIKey(refresher func() string) (string, bool) {
	if refresher == nil {
		return "", false
	}
	key := strings.TrimSpace(refresher())
	return key, key != ""
}

func isAuthFailureStatus(status int, body []byte) bool {
	code, message := authErrorDetails(body)
	text := strings.ToLower(strings.TrimSpace(code + " " + message + " " + string(body)))
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	for _, marker := range []string{
		"token_expired",
		"invalid_token",
		"expired",
		"unauthorized",
		"authentication",
		"auth",
		"signing in again",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func authErrorDetails(body []byte) (code string, message string) {
	var payload interface{}
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return "", ""
	}
	var walk func(interface{})
	walk = func(value interface{}) {
		switch item := value.(type) {
		case map[string]interface{}:
			for key, child := range item {
				lower := strings.ToLower(strings.TrimSpace(key))
				if code == "" && (lower == "code" || lower == "error_code" || lower == "type") {
					code = stringValue(child)
				}
				if message == "" && (lower == "message" || lower == "error_description" || lower == "error" || lower == "detail") {
					if s := stringValue(child); s != "" {
						message = s
					}
				}
			}
			for _, child := range item {
				if code != "" && message != "" {
					return
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range item {
				if code != "" && message != "" {
					return
				}
				walk(child)
			}
		case string:
			if message == "" {
				message = strings.TrimSpace(item)
			}
		}
	}
	walk(payload)
	return code, message
}

func providerAPIError(provider string, status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	code, message := authErrorDetails(body)
	class := classifyProviderAPIError(status, code, message, text)
	if isAuthFailureStatus(status, body) {
		return &ProviderError{Provider: provider, StatusCode: status, Class: ProviderErrorAuth,
			Code: code, Message: firstNonEmptyString(message, code, http.StatusText(status))}
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return &ProviderError{Provider: provider, StatusCode: status, Class: class,
		Code: code, Message: firstNonEmptyString(message, text)}
}

func classifyProviderAPIError(status int, values ...string) ProviderErrorClass {
	text := strings.ToLower(strings.Join(values, " "))
	for _, marker := range []string{"quota", "insufficient_quota", "usage limit", "usage_limit", "quota exhausted"} {
		if strings.Contains(text, marker) {
			return ProviderErrorQuota
		}
	}
	for _, marker := range []string{"billing", "payment", "credit balance"} {
		if strings.Contains(text, marker) {
			return ProviderErrorBilling
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ProviderErrorRateLimit
	case status == http.StatusUnauthorized:
		return ProviderErrorAuth
	case status >= 500 || status == http.StatusRequestTimeout:
		return ProviderErrorTransient
	case status >= 400:
		return ProviderErrorInvalidRequest
	default:
		return ProviderErrorUnknown
	}
}
