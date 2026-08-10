package httpapi

import (
	"sync"
	"time"
)

// Presence timing contract (docs/identity-continuity.md "Runtime attachment
// model"): presence is DERIVED state — heartbeats from live endpoints — and is
// never persisted as authority. The TTL must comfortably cover the slowest
// heartbeat source (the idle TUI's 30s presence ping) plus network jitter, so
// a healthy endpoint never flaps to detached between beats; a crashed terminal
// is an implicit detach once the TTL lapses.
const (
	presenceTTL = 90 * time.Second
	// presencePersistInterval throttles the durable accounts.last_seen_at
	// write that backs preferred-endpoint selection: mid-turn event polls
	// arrive every ~350ms and must not turn into per-request DB writes.
	presencePersistInterval = 60 * time.Second
)

type presenceKey struct {
	personID string
	platform string
}

// presenceRegistry is the in-memory per-person, per-platform attachment map.
// It answers exactly one routing question — "is an interactive endpoint of
// this platform alive right now?" — for the conversation-layer rules 3/4
// (CLI attached → inline surface only; CLI detached → single preferred IM
// endpoint). It lives and dies with the daemon process on purpose: rebooting
// the gateway means every endpoint re-establishes presence by heartbeating.
type presenceRegistry struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[presenceKey]time.Time
	persisted map[presenceKey]time.Time
	now       func() time.Time // injectable for tests
}

func newPresenceRegistry(ttl time.Duration) *presenceRegistry {
	return &presenceRegistry{
		ttl:       ttl,
		seen:      make(map[presenceKey]time.Time),
		persisted: make(map[presenceKey]time.Time),
		now:       time.Now,
	}
}

// Touch records a liveness beat for the person's endpoint on that platform.
// The boolean return tells the caller whether the throttled durable
// accounts.last_seen_at write is due (first beat, or the last persist is
// older than presencePersistInterval); the registry itself never touches the
// database — presence stays derived, recency persistence stays the store's.
func (p *presenceRegistry) Touch(personID, platform string) bool {
	if p == nil || personID == "" || platform == "" {
		return false
	}
	key := presenceKey{personID: personID, platform: platform}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[key] = now
	p.pruneLocked(now)
	if last, ok := p.persisted[key]; ok && now.Sub(last) < presencePersistInterval {
		return false
	}
	p.persisted[key] = now
	return true
}

// IsAttached reports whether the person has a live endpoint on the platform:
// a beat newer than the TTL. Expiry is passive (checked on read), so a crash
// or closed terminal detaches by silence, identical to a graceful close.
func (p *presenceRegistry) IsAttached(personID, platform string) bool {
	if p == nil {
		return false
	}
	key := presenceKey{personID: personID, platform: platform}
	p.mu.Lock()
	defer p.mu.Unlock()
	last, ok := p.seen[key]
	return ok && p.now().Sub(last) < p.ttl
}

// AnyAttached reports whether ANY of the person's endpoints is alive right
// now, without naming a platform. Approval waiting needs "could someone answer
// this at all", not "which surface do I render on".
func (p *presenceRegistry) AnyAttached(personID string) bool {
	if p == nil || personID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key, last := range p.seen {
		if key.personID == personID && now.Sub(last) < p.ttl {
			return true
		}
	}
	return false
}

// pruneLocked drops long-expired entries so the maps stay bounded on a
// long-lived daemon. Caller holds p.mu.
func (p *presenceRegistry) pruneLocked(now time.Time) {
	if len(p.seen) <= 64 {
		return
	}
	for key, last := range p.seen {
		if now.Sub(last) >= p.ttl {
			delete(p.seen, key)
			delete(p.persisted, key)
		}
	}
}
