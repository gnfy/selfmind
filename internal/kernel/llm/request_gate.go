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
}

type requestRoute struct {
	sem           chan struct{}
	mu            sync.Mutex
	cooldownUntil time.Time
	rateFailures  int
}

func NewRequestGate(maxConcurrent int) *RequestGate {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &RequestGate{routes: make(map[string]*requestRoute), maxConcurrent: maxConcurrent}
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
	return &gatedProvider{inner: provider, route: route}
}

type gatedProvider struct {
	inner Provider
	route *requestRoute
}

func (p *gatedProvider) Unwrap() Provider { return p.inner }

func (p *gatedProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	if err := p.route.acquire(ctx); err != nil {
		return "", err
	}
	defer p.route.release()
	content, err := p.inner.ChatCompletion(ctx, messages)
	p.route.observe(err)
	return content, err
}

func (p *gatedProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := p.route.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.route.release()
	response, err := p.inner.Chat(ctx, req)
	p.route.observe(err)
	return response, err
}

func (p *gatedProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if err := p.route.acquire(ctx); err != nil {
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

func (r *requestRoute) acquire(ctx context.Context) error {
	for {
		r.mu.Lock()
		wait := time.Until(r.cooldownUntil)
		r.mu.Unlock()
		if wait <= 0 {
			select {
			case r.sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			r.mu.Lock()
			cooling := time.Until(r.cooldownUntil) > 0
			r.mu.Unlock()
			if !cooling {
				if err := ctx.Err(); err != nil {
					r.release()
					return err
				}
				return nil
			}
			r.release()
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
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
