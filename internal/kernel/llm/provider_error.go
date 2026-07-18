package llm

import (
	"errors"
	"fmt"
	"strings"
)

// ProviderErrorClass is a stable, machine-readable failure category. Provider
// adapters should return ProviderError for HTTP and semantic response failures
// so retry, fallback, and maintenance circuit-breaker policy never has to
// infer quota/auth state from localized error strings.
type ProviderErrorClass string

const (
	ProviderErrorUnknown        ProviderErrorClass = "unknown"
	ProviderErrorQuota          ProviderErrorClass = "quota"
	ProviderErrorRateLimit      ProviderErrorClass = "rate_limit"
	ProviderErrorAuth           ProviderErrorClass = "auth"
	ProviderErrorBilling        ProviderErrorClass = "billing"
	ProviderErrorInvalidRequest ProviderErrorClass = "invalid_request"
	ProviderErrorTransient      ProviderErrorClass = "transient"
	ProviderErrorEmptyResponse  ProviderErrorClass = "empty_response"
)

// ProviderError carries only redacted provider metadata. Never put API keys,
// authorization headers, or raw request bodies in this value: it is persisted
// in maintenance diagnostics and may be shown to the owner.
type ProviderError struct {
	Provider   string
	RouteID    string
	Class      ProviderErrorClass
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	StopReason string
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "provider error"
	}
	provider := strings.TrimSpace(e.Provider)
	if provider == "" {
		provider = "provider"
	}
	parts := []string{provider}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if e.Class != "" && e.Class != ProviderErrorUnknown {
		parts = append(parts, string(e.Class))
	}
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = strings.TrimSpace(e.Code)
	}
	if detail != "" {
		parts = append(parts, detail)
	}
	if e.StopReason != "" {
		parts = append(parts, "stop_reason="+e.StopReason)
	}
	if e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	return strings.Join(parts, ": ")
}

// ProviderErrorInfo extracts a copy safe for policy and diagnostics.
func ProviderErrorInfo(err error) (ProviderError, bool) {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return ProviderError{}, false
	}
	return *providerErr, true
}

// WithProviderRoute adds the resolved physical route without losing the
// adapter's structured class/status/request id.
func WithProviderRoute(err error, provider, routeID string) error {
	if err == nil {
		return nil
	}
	if info, ok := ProviderErrorInfo(err); ok {
		if strings.TrimSpace(provider) != "" {
			info.Provider = strings.TrimSpace(provider)
		}
		info.RouteID = strings.TrimSpace(routeID)
		return &info
	}
	return &ProviderError{
		Provider: strings.TrimSpace(provider), RouteID: strings.TrimSpace(routeID),
		Class: ProviderErrorUnknown, Message: err.Error(),
	}
}

func IsQuotaError(err error) bool {
	info, ok := ProviderErrorInfo(err)
	return ok && info.Class == ProviderErrorQuota
}
