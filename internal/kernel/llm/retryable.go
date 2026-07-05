package llm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Transport resilience defaults. These bound how the agent retry loop and the
// streaming read path behave against transient provider failures (notably the
// codex Responses `Post .../responses: EOF` connection drops). They are process
// defaults, overridable per deployment via config knobs (see AgentConfig
// llm_retry_base / llm_retry_cap / llm_stream_idle_timeout).
const (
	DefaultRetryBase  = 300 * time.Millisecond
	DefaultRetryCap   = 30 * time.Second
	DefaultStreamIdle = 180 * time.Second
	maxRetryAfter     = 600 * time.Second
)

// IsRetryableError reports whether an LLM transport error is worth re-sending
// with backoff. Retryable = transient network/stream/server conditions that a
// full re-send can recover from (EOF, connection reset/refused, timeouts,
// 5xx, 429, mid-stream idle aborts). Fatal = errors a re-send cannot fix and
// must fail fast on (context-window exceeded, quota/usage limits, 401/invalid
// auth — the codex-login refresh already happens once inside the adapter — and
// 400 invalid-request). Unknown errors default to retryable so we do not
// regress resilience for unlisted transient shapes; the fatal set is the guard
// that stops wasting attempts on unfixable calls.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// net.Error timeouts (dial/read deadlines) are transient.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())

	// Fatal patterns win over retryable so a 400/401 that also mentions a
	// generic word is never retried.
	for _, marker := range fatalMarkers {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	// Explicit HTTP status classification: 4xx (except 408/429) is fatal.
	if status, ok := httpStatusFromMessage(msg); ok {
		switch {
		case status == http.StatusTooManyRequests: // 429
			return true
		case status == http.StatusRequestTimeout: // 408
			return true
		case status >= 500: // 5xx
			return true
		case status >= 400: // other 4xx: client error, do not retry
			return false
		}
	}
	for _, marker := range retryableMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	// Default: treat as transient. The fatal set above is the real guard.
	return true
}

var fatalMarkers = []string{
	"context length",
	"context window",
	"maximum context",
	"context_length_exceeded",
	"reduce the length",
	"too many tokens",
	"string too long",
	"quota",
	"insufficient_quota",
	"usage limit",
	"usage_limit",
	"billing",
	"payment",
	"invalid_api_key",
	"invalid api key",
	"incorrect api key",
	"invalid_request_error",
	"invalid request",
	"unsupported",
	"model_not_found",
	"model not found",
	"unauthorized",
	"invalid_token",
	"token_expired",
	"login expired",
	"signing in again",
	"permission",
}

var retryableMarkers = []string{
	"eof",
	"connection reset",
	"connection refused",
	"broken pipe",
	"connection closed",
	"reset by peer",
	"timeout",
	"timed out",
	"deadline exceeded",
	"temporarily unavailable",
	"try again",
	"overloaded",
	"unexpected end",
	"incomplete chunked",
	"stream closed before",
	"stream idle",
	"idle for",
	"server closed",
	"no response",
}

// httpStatusFromMessage extracts an HTTP status code embedded in a provider
// error string, e.g. "responses API error 503: ..." or "HTTP 429".
func httpStatusFromMessage(msg string) (int, bool) {
	m := httpStatusRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

var httpStatusRe = regexp.MustCompile(`(?:error|http|status)[^0-9]{0,6}(\d{3})`)

// Backoff returns the delay before retry number `attempt` (1-based: attempt 1
// is the first backoff, taken after the first failed try). It grows
// exponentially base*2^(attempt-1), is capped at `cap`, and is multiplied by a
// jitter factor in [0.9,1.1) drawn from `jitter` (a func returning [0,1); pass
// nil for no jitter). The result is clamped to `cap` so it never exceeds the
// cap even after jitter.
func Backoff(attempt int, base, cap time.Duration, jitter func() float64) time.Duration {
	if attempt < 1 {
		return 0
	}
	if base <= 0 {
		base = DefaultRetryBase
	}
	if cap <= 0 {
		cap = DefaultRetryCap
	}
	d := base
	for i := 1; i < attempt; i++ {
		if d >= cap {
			d = cap
			break
		}
		d *= 2
	}
	if d > cap {
		d = cap
	}
	factor := 1.0
	if jitter != nil {
		factor = 0.9 + 0.2*jitter()
	}
	d = time.Duration(float64(d) * factor)
	if d > cap {
		d = cap
	}
	if d < 0 {
		d = 0
	}
	return d
}

// RetryAfterFromError extracts a server-advertised retry delay from a 429/503
// error. It recognizes both the Retry-After header folded into the message by
// foldRetryAfter ("retry-after: 30" / "retry-after: 30s") and the codex/OpenAI
// body phrasing ("try again in 1.5s", "try again in 500ms", "please try again
// in 20 seconds"). The result is capped at maxRetryAfter.
func RetryAfterFromError(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	msg := strings.ToLower(err.Error())
	if d, ok := parseRetryAfterHeader(msg); ok {
		return capRetryAfter(d), true
	}
	if d, ok := parseTryAgainPhrase(msg); ok {
		return capRetryAfter(d), true
	}
	return 0, false
}

var retryAfterHeaderRe = regexp.MustCompile(`retry-after:\s*([0-9]+(?:\.[0-9]+)?)\s*(ms|s)?`)
var tryAgainRe = regexp.MustCompile(`try again in\s*([0-9]+(?:\.[0-9]+)?)\s*(ms|milliseconds|s|sec|secs|seconds|m|min|mins|minutes)?`)

func parseRetryAfterHeader(msg string) (time.Duration, bool) {
	m := retryAfterHeaderRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0, false
	}
	return numericDuration(m[1], m[2], time.Second)
}

func parseTryAgainPhrase(msg string) (time.Duration, bool) {
	m := tryAgainRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0, false
	}
	return numericDuration(m[1], m[2], time.Second)
}

func numericDuration(value, unit string, fallback time.Duration) (time.Duration, bool) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	var scale time.Duration
	switch unit {
	case "ms", "milliseconds":
		scale = time.Millisecond
	case "s", "sec", "secs", "seconds":
		scale = time.Second
	case "m", "min", "mins", "minutes":
		scale = time.Minute
	case "":
		scale = fallback
	default:
		scale = fallback
	}
	return time.Duration(f * float64(scale)), true
}

func capRetryAfter(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// foldRetryAfter appends a provider's Retry-After header into an error message
// so the retry loop can honor it via RetryAfterFromError without threading the
// http.Header through the agent. Applied only where the header is present.
func foldRetryAfter(err error, h http.Header) error {
	if err == nil || h == nil {
		return err
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return err
	}
	return fmt.Errorf("%w (retry-after: %s)", err, v)
}
