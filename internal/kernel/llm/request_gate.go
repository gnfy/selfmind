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
	mu            sync.Mutex
	routes        map[string]*requestRoute
	maxConcurrent int
	observeWait   RequestWaitObserver
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
		route = &requestRoute{sem: make(chan struct{}, g.maxConcurrent)}
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
	defer p.route.release()
	content, err := p.inner.ChatCompletion(ctx, messages)
	p.route.observe(err)
	return content, err
}

func (p *gatedProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.route.release()
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
		p.route.release()
		return stream, err
	}
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		defer p.route.release()
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

func (p *gatedProvider) acquire(ctx context.Context) error {
	started := time.Now()
	if providerWaitDeferrable(ctx) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if wait := p.route.tryAcquire(); wait != nil {
			return wait
		}
		if err := ctx.Err(); err != nil {
			p.route.release()
			return err
		}
		return nil
	}
	reason, err := p.route.acquire(ctx)
	if reason != "" {
		p.gate.mu.Lock()
		observe := p.gate.observeWait
		p.gate.mu.Unlock()
		if observe != nil {
			observe(ctx, p.routeID, reason, time.Since(started))
		}
	}
	return err
}

// tryAcquire never reserves a worker while another request or a 429 cooldown
// owns this physical route. A short capacity deadline is polled by the durable
// queue; a cooldown uses the actual route deadline.
func (r *requestRoute) tryAcquire() *ProviderWait {
	r.mu.Lock()
	until := r.cooldownUntil
	r.mu.Unlock()
	if time.Now().Before(until) {
		return &ProviderWait{Reason: "rate_limit", NotBefore: until}
	}
	select {
	case r.sem <- struct{}{}:
		r.mu.Lock()
		until = r.cooldownUntil
		r.mu.Unlock()
		if time.Now().Before(until) {
			r.release()
			return &ProviderWait{Reason: "rate_limit", NotBefore: until}
		}
		return nil
	default:
		return &ProviderWait{Reason: "capacity", NotBefore: time.Now().Add(time.Second)}
	}
}

func (r *requestRoute) acquire(ctx context.Context) (string, error) {
	reason := ""
	for {
		r.mu.Lock()
		wait := time.Until(r.cooldownUntil)
		r.mu.Unlock()
		if wait <= 0 {
			select {
			case r.sem <- struct{}{}:
			default:
				reason = "capacity"
				select {
				case r.sem <- struct{}{}:
				case <-ctx.Done():
					return reason, ctx.Err()
				}
			}
			r.mu.Lock()
			cooling := time.Until(r.cooldownUntil) > 0
			r.mu.Unlock()
			if !cooling {
				if err := ctx.Err(); err != nil {
					r.release()
					return reason, err
				}
				return reason, nil
			}
			r.release()
			continue
		}
		reason = "rate_limit"
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return reason, ctx.Err()
		}
	}
}

func (r *requestRoute) release() { <-r.sem }

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
	}
}
