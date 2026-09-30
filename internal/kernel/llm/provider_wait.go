package llm

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ProviderWait is a clean model-call boundary that may be resumed after the
// physical route becomes available. It is never a provider response or proof
// that a tool effect did not happen; the caller must persist its model ledger
// before enabling this non-blocking admission path.
type ProviderWait struct {
	Reason    string
	NotBefore time.Time
}

func (w *ProviderWait) Error() string {
	return fmt.Sprintf("provider %s until %s", w.Reason, w.NotBefore.UTC().Format(time.RFC3339))
}

type providerWaitDeferralKey struct{}

// WithProviderWaitDeferral is installed only after a synchronous checkpoint.
// Background calls and uncheckpointed turns retain cancellable in-place waits.
func WithProviderWaitDeferral(ctx context.Context) context.Context {
	return context.WithValue(ctx, providerWaitDeferralKey{}, true)
}

func providerWaitDeferrable(ctx context.Context) bool {
	return ctx != nil && ctx.Value(providerWaitDeferralKey{}) == true && ModelContextFrom(ctx).RunID != ""
}

// DeferRateLimit returns a durable wake deadline for a rate-limited foreground
// call. Quota and other provider failures are never reclassified as a wait.
func DeferRateLimit(ctx context.Context, err error, backoff time.Duration) *ProviderWait {
	if !providerWaitDeferrable(ctx) {
		return nil
	}
	info, ok := ProviderErrorInfo(err)
	if !ok || (info.Class != ProviderErrorRateLimit && info.StatusCode != http.StatusTooManyRequests) {
		return nil
	}
	if advertised, ok := RetryAfterFromError(err); ok && advertised > backoff {
		backoff = advertised
	}
	if backoff < time.Second {
		backoff = time.Second
	}
	if backoff > maxRetryAfter {
		backoff = maxRetryAfter
	}
	return &ProviderWait{Reason: "rate_limit", NotBefore: time.Now().Add(backoff)}
}
