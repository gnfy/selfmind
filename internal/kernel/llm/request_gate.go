package llm

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// RequestGate bounds requests to the same physical provider route across all
// providers constructed by one daemon. A route is an opaque digest supplied by
// the runtime resolver; model names and logical roles are not quota identities.
// It is deliberately instance-owned rather than process-global.
type RequestGate struct {
	mu               sync.Mutex
	routes           map[string]*requestRoute
	maxConcurrent    int
	observeWait      RequestWaitObserver
	observeAdmission RequestAdmissionObserver
}

// RequestWaitObserver receives a completed admission wait, including canceled
// waits. The route is an opaque digest; observers may use ModelContextFrom(ctx)
// to attribute it to a Run without seeing prompts or credentials.
type RequestWaitObserver func(ctx context.Context, routeID, reason string, duration time.Duration)

type requestRoute struct {
	sem           chan struct{}
	mu            sync.Mutex
	cooldownUntil time.Time
	rateFailures  int
	activeRoles   map[string]int
	changed       chan struct{}
}

func NewRequestGate(maxConcurrent int, observers ...RequestWaitObserver) *RequestGate {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	gate := &RequestGate{routes: make(map[string]*requestRoute), maxConcurrent: maxConcurrent}
	if len(observers) > 0 {
		gate.observeWait = observers[0]
	}
	return gate
}

// SetWaitObserver is used when the daemon's durable event store becomes
// available after model-transition startup checks have constructed the gate.
func (g *RequestGate) SetWaitObserver(observer RequestWaitObserver) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.observeWait = observer
	g.mu.Unlock()
}

// Wrap returns a transparent provider wrapper. An absent gate or route means
// there is no physical provider request to coordinate (for example, the setup
// diagnostic provider).
func (g *RequestGate) Wrap(provider Provider, routeID string) Provider {
	if g == nil || provider == nil || routeID == "" {
		return provider
	}
	g.mu.Lock()
	route := g.routes[routeID]
	if route == nil {
		route = &requestRoute{sem: make(chan struct{}, g.maxConcurrent), changed: make(chan struct{}), activeRoles: make(map[string]int)}
		g.routes[routeID] = route
	}
	g.mu.Unlock()
	return &gatedProvider{inner: provider, route: route, routeID: routeID, gate: g}
}

type gatedProvider struct {
	inner   Provider
	route   *requestRoute
	routeID string
	gate    *RequestGate
}

func (p *gatedProvider) Unwrap() Provider { return p.inner }

func (p *gatedProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	if err := p.acquire(ctx); err != nil {
		return "", err
	}
	defer p.release(ctx)
	content, err := p.inner.ChatCompletion(ctx, messages)
	p.route.observe(err)
	return content, err
}

func (p *gatedProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release(ctx)
	response, err := p.inner.Chat(ctx, req)
	p.route.observe(err)
	return response, err
}

func (p *gatedProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	stream, err := p.inner.StreamChat(ctx, req)
	if err != nil || stream == nil {
		p.route.observe(err)
		p.release(ctx)
		return stream, err
	}
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		defer p.release(ctx)
		failed := false
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-stream:
				if !ok {
					if !failed {
						p.route.observe(nil)
					}
					return
				}
				if event.Err != nil {
					failed = true
					p.route.observe(event.Err)
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Brief contention stays in the current Run. Longer waits still use the
// durable checkpoint path; rate-limit cooldowns never consume this grace.
const providerCapacityGrace = 250 * time.Millisecond

func (p *gatedProvider) acquire(ctx context.Context) error {
	started := time.Now()
	reason, err := p.route.acquire(ctx, admissionRole(ctx), providerWaitDeferrable(ctx))
	if wait, ok := err.(*ProviderWait); ok {
		p.observeAdmission(ctx, "deferred", wait.Reason, time.Since(started), wait.NotBefore)
		return wait
	}
	if reason != "" {
		p.gate.mu.Lock()
		observe := p.gate.observeWait
		p.gate.mu.Unlock()
		if observe != nil {
			observe(ctx, p.routeID, reason, time.Since(started))
		}
	}
	if err == nil && ctx.Err() != nil {
		p.release(ctx)
		err = ctx.Err()
	}
	if err == nil {
		p.observeAdmission(ctx, "acquired", reason, time.Since(started), time.Time{})
	} else {
		p.observeAdmission(ctx, "canceled", reason, time.Since(started), time.Time{})
	}
	return err
}

// Permit ownership and role attribution change under the same mutex. Waiters
// wake on release or cooldown changes, then recheck both before admission.
func (r *requestRoute) acquire(ctx context.Context, role string, deferrable bool) (string, error) {
	reason := ""
	deadline := time.Now().Add(providerCapacityGrace)
	for {
		if err := ctx.Err(); err != nil {
			return reason, err
		}
		r.mu.Lock()
		until := r.cooldownUntil
		changed := r.changed
		cooling := time.Now().Before(until)
		if !cooling {
			select {
			case r.sem <- struct{}{}:
				r.activeRoles[role]++
				r.mu.Unlock()
				return reason, nil
			default:
			}
		}
		r.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if cooling {
			reason = "rate_limit"
			if deferrable {
				return reason, &ProviderWait{Reason: reason, NotBefore: until}
			}
			timer = time.NewTimer(time.Until(until))
		} else {
			reason = "capacity"
			if deferrable {
				if time.Now().After(deadline) {
					return reason, &ProviderWait{Reason: reason, NotBefore: time.Now().Add(time.Second)}
				}
				timer = time.NewTimer(time.Until(deadline))
			}
		}
		if timer != nil {
			timeout = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return reason, ctx.Err()
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
		case <-timeout:
		}
	}
}

func (r *requestRoute) observe(err error) {
	if err == nil {
		r.mu.Lock()
		r.rateFailures = 0
		r.mu.Unlock()
		return
	}
	info, ok := ProviderErrorInfo(err)
	if !ok || (info.Class != ProviderErrorRateLimit && info.StatusCode != http.StatusTooManyRequests) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateFailures++
	delay := time.Second << min(r.rateFailures-1, 5)
	if advertised, ok := RetryAfterFromError(err); ok && advertised > delay {
		delay = advertised
	}
	if delay > maxRetryAfter {
		delay = maxRetryAfter
	}
	until := time.Now().Add(delay)
	if until.After(r.cooldownUntil) {
		r.cooldownUntil = until
		close(r.changed)
		r.changed = make(chan struct{})
	}
}
