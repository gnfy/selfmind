package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

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
	if isAuthFailureStatus(status, body) {
		detail := firstNonEmptyString(message, code, http.StatusText(status))
		return fmt.Errorf("%s credential expired or invalid (HTTP %d): %s", provider, status, detail)
	}
	if message != "" {
		return fmt.Errorf("%s API error %d: %s", provider, status, message)
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return fmt.Errorf("%s API error %d: %s", provider, status, text)
}
