package llm

import (
	"context"
	"time"
)

// RequestAdmission records local capacity separately from provider failures.
// Occupants are role counts, never other people's Run IDs or credentials.
type RequestAdmission struct {
	Kind        string         `json:"kind"`
	Reason      string         `json:"reason,omitempty"`
	Capacity    int            `json:"capacity"`
	Active      int            `json:"active"`
	ActiveRoles map[string]int `json:"active_roles"`
	DurationMS  int64          `json:"duration_ms"`
	NotBefore   time.Time      `json:"not_before,omitempty"`
}

type RequestAdmissionObserver func(context.Context, string, RequestAdmission)

func (g *RequestGate) SetAdmissionObserver(observer RequestAdmissionObserver) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.observeAdmission = observer
	g.mu.Unlock()
}

func admissionRole(ctx context.Context) string {
	role := string(ModelContextFrom(ctx).Role)
	if role == "" {
		return "unattributed"
	}
	return role
}

func (p *gatedProvider) admitted(ctx context.Context) {
	p.route.mu.Lock()
	if p.route.activeRoles == nil {
		p.route.activeRoles = make(map[string]int)
	}
	p.route.activeRoles[admissionRole(ctx)]++
	p.route.mu.Unlock()
	p.observeAdmission(ctx, "acquired", "", 0, time.Time{})
}

func (p *gatedProvider) release(ctx context.Context) {
	p.route.mu.Lock()
	role := admissionRole(ctx)
	if p.route.activeRoles[role] <= 1 {
		delete(p.route.activeRoles, role)
	} else {
		p.route.activeRoles[role]--
	}
	p.route.release()
	p.route.mu.Unlock()
	p.observeAdmission(ctx, "released", "", 0, time.Time{})
}

func (p *gatedProvider) observeAdmission(ctx context.Context, kind, reason string, elapsed time.Duration, notBefore time.Time) {
	p.gate.mu.Lock()
	observer := p.gate.observeAdmission
	p.gate.mu.Unlock()
	if observer == nil {
		return
	}
	observation := RequestAdmission{Kind: kind, Reason: reason, Capacity: cap(p.route.sem),
		DurationMS: elapsed.Milliseconds(), NotBefore: notBefore, ActiveRoles: map[string]int{}}
	p.route.mu.Lock()
	observation.Active = len(p.route.sem)
	attributed := 0
	for role, count := range p.route.activeRoles {
		observation.ActiveRoles[role] = count
		attributed += count
	}
	if missing := observation.Active - attributed; missing > 0 {
		observation.ActiveRoles["unattributed"] += missing
	}
	p.route.mu.Unlock()
	observer(ctx, p.routeID, observation)
}
