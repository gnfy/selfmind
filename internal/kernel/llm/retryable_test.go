package llm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o operation" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

var _ net.Error = fakeTimeoutErr{}

func TestIsRetryableErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected-eof", io.ErrUnexpectedEOF, true},
		{"post-eof", fmt.Errorf("Post \"https://chatgpt.com/backend-api/codex/responses\": EOF"), true},
		{"connection-reset", errors.New("read tcp: connection reset by peer"), true},
		{"connection-refused", errors.New("dial tcp: connection refused"), true},
		{"net-timeout", fakeTimeoutErr{}, true},
		{"incomplete-chunked", errors.New("unexpected EOF: incomplete chunked read"), true},
		{"stream-idle", errors.New("codex stream idle for 3m0s without data; aborting"), true},
		{"http-500", errors.New("responses API error 500: internal error"), true},
		{"http-503", errors.New("responses API error 503: service unavailable"), true},
		{"http-429", errors.New("responses API error 429: rate limited"), true},
		{"overloaded", errors.New("anthropic API error 529: overloaded_error"), true},
		// Fatal — must NOT retry.
		{"context-window", errors.New("responses API error 400: context length exceeded"), false},
		{"context_length_exceeded", errors.New("openai API error 400: context_length_exceeded"), false},
		{"quota", errors.New("openai API error 429: insufficient_quota"), false},
		{"usage-limit", errors.New("provider usage limit reached"), false},
		{"unauthorized-401", errors.New("responses API error 401: unauthorized"), false},
		{"invalid-api-key", errors.New("openai API error 401: invalid_api_key"), false},
		{"codex-login", errors.New("Codex login expired (responses API 401). Run `codex login`"), false},
		{"invalid-request-400", errors.New("responses API error 400: invalid request"), false},
		{"model-not-found", errors.New("openai API error 404: model_not_found"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableError(tc.err); got != tc.want {
				t.Fatalf("IsRetryableError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBackoffMonotonicAndCapped(t *testing.T) {
	base := 100 * time.Millisecond
	cap := 2 * time.Second
	// No jitter (factor 1.0): base*2^(attempt-1), capped, monotonic non-decreasing.
	var prev time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		d := Backoff(attempt, base, cap, nil)
		if d > cap {
			t.Fatalf("attempt %d: %v exceeds cap %v", attempt, d, cap)
		}
		if d < prev {
			t.Fatalf("attempt %d: %v < previous %v (not monotonic)", attempt, d, prev)
		}
		prev = d
	}
	if got := Backoff(1, base, cap, nil); got != base {
		t.Fatalf("attempt 1 should equal base %v, got %v", base, got)
	}
	if got := Backoff(2, base, cap, nil); got != 2*base {
		t.Fatalf("attempt 2 should equal 2*base %v, got %v", 2*base, got)
	}
	// Deep attempts saturate at cap.
	if got := Backoff(20, base, cap, nil); got != cap {
		t.Fatalf("attempt 20 should saturate at cap %v, got %v", cap, got)
	}
}

func TestBackoffJitterWithinBounds(t *testing.T) {
	base := 100 * time.Millisecond
	cap := 10 * time.Second
	attempt := 3 // pre-jitter = 400ms, below cap
	raw := Backoff(attempt, base, cap, nil)
	lo := Backoff(attempt, base, cap, func() float64 { return 0 })    // factor 0.9
	hi := Backoff(attempt, base, cap, func() float64 { return 0.99 }) // factor ~1.098
	wantLo := time.Duration(float64(raw) * 0.9)
	if lo != wantLo {
		t.Fatalf("jitter low = %v, want %v", lo, wantLo)
	}
	if hi <= raw {
		t.Fatalf("jitter high %v should exceed un-jittered %v", hi, raw)
	}
	if hi >= time.Duration(float64(raw)*1.1)+time.Millisecond {
		t.Fatalf("jitter high %v should be under 1.1x %v", hi, raw)
	}
	// Jitter never pushes above cap.
	capped := Backoff(30, base, cap, func() float64 { return 0.99 })
	if capped > cap {
		t.Fatalf("jittered value %v exceeds cap %v", capped, cap)
	}
}

func TestRetryAfterFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
		ok   bool
	}{
		{"none", errors.New("plain error"), 0, false},
		{"header-seconds", errors.New("responses API error 429: rate limited (retry-after: 30)"), 30 * time.Second, true},
		{"header-seconds-suffix", errors.New("rate limited (retry-after: 12s)"), 12 * time.Second, true},
		{"header-ms", errors.New("rate limited (retry-after: 500ms)"), 500 * time.Millisecond, true},
		{"body-seconds", errors.New("openai API error 429: Rate limit reached. Please try again in 20s"), 20 * time.Second, true},
		{"body-ms", errors.New("Rate limit reached. Please try again in 200ms"), 200 * time.Millisecond, true},
		{"body-words", errors.New("please try again in 5 seconds"), 5 * time.Second, true},
		{"cap", errors.New("retry-after: 9999"), maxRetryAfter, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RetryAfterFromError(tc.err)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFoldRetryAfterHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "42")
	err := foldRetryAfter(errors.New("responses API error 429: slow down"), h)
	d, ok := RetryAfterFromError(err)
	if !ok || d != 42*time.Second {
		t.Fatalf("expected folded retry-after 42s, got %v (ok=%v) from %q", d, ok, err)
	}
	// No header: unchanged.
	if got := foldRetryAfter(err, http.Header{}); got.Error() != err.Error() {
		t.Fatalf("empty header should not alter error")
	}
}
