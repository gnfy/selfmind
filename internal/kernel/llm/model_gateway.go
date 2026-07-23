package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// ModelRole describes why a model is being called. Policy can route each role
// to a different provider/model for cost, latency, or quality tradeoffs.
type ModelRole string

const (
	RoleDefaultChat      ModelRole = "default_chat"
	RoleCodingAgent      ModelRole = "coding_agent"
	RoleFastClassifier   ModelRole = "fast_classifier"
	RoleMemoryExtract    ModelRole = "memory_extract"
	RoleBackgroundReview ModelRole = "background_review"
	RoleSkillCurator     ModelRole = "skill_curator"
	RoleSummarizer       ModelRole = "summarizer"
	RoleSemanticRecall   ModelRole = "semantic_recall"
	RoleVision           ModelRole = "vision"
)

// ModelContext carries routing/audit metadata for a model call. Personal mode
// can leave most fields empty; SaaS mode should populate all relevant IDs.
type ModelContext struct {
	TenantID    string
	PersonID    string
	WorkspaceID string
	TaskID      string
	RunID       string
	Role        ModelRole
}

type modelContextKey struct{}

// WithModelContext attaches model routing metadata to a context.
func WithModelContext(ctx context.Context, mc ModelContext) context.Context {
	return context.WithValue(ctx, modelContextKey{}, mc)
}

// ModelContextFrom extracts model routing metadata from a context.
func ModelContextFrom(ctx context.Context) ModelContext {
	if ctx == nil {
		return ModelContext{}
	}
	if mc, ok := ctx.Value(modelContextKey{}).(ModelContext); ok {
		return mc
	}
	return ModelContext{}
}

// StablePromptCacheKey returns a Responses-compatible cache namespace that is
// stable across runs for the same task. Run IDs are intentionally excluded so
// a resumed task can reuse its stable prompt prefix.
func StablePromptCacheKey(ctx context.Context) string {
	mc := ModelContextFrom(ctx)
	identity := strings.TrimSpace(mc.TaskID)
	if identity == "" {
		identity = strings.TrimSpace(mc.WorkspaceID)
	}
	if identity == "" {
		identity = strings.TrimSpace(mc.PersonID)
	}
	if identity == "" {
		return ""
	}
	parts := []string{strings.TrimSpace(mc.TenantID), identity, string(mc.Role)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "selfmind-" + hex.EncodeToString(sum[:16])
}

// ProviderProfile is a resolved provider/model choice for one role.
type ProviderProfile struct {
	Name         string
	ProviderName string
	Model        string
	Provider     Provider
}

// UsageEvent is emitted after a model call succeeds.
type UsageEvent struct {
	Context      ModelContext
	ProviderName string
	Model        string
	InputTokens  int
	OutputTokens int
}

// UsageRecorder records model usage for cost/accounting. Implementations may
// persist events, aggregate them, or drop them in personal mode.
type UsageRecorder interface {
	RecordModelUsage(ctx context.Context, event UsageEvent)
}

// PolicyGateway resolves model roles to provider profiles.
type PolicyGateway struct {
	mu       sync.RWMutex
	fallback ProviderProfile
	roles    map[ModelRole]ProviderProfile
	recorder UsageRecorder
}

// NewPolicyGateway creates a gateway with a required fallback profile.
func NewPolicyGateway(fallback ProviderProfile) *PolicyGateway {
	return &PolicyGateway{
		fallback: fallback,
		roles:    make(map[ModelRole]ProviderProfile),
	}
}

// SetUsageRecorder configures optional usage recording.
func (g *PolicyGateway) SetUsageRecorder(recorder UsageRecorder) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recorder = recorder
}

// RegisterRoleProfile assigns a provider profile to a role.
func (g *PolicyGateway) RegisterRoleProfile(role ModelRole, profile ProviderProfile) {
	if role == "" || profile.Provider == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roles[role] = profile
}

// ProviderForRole returns an llm.Provider wrapper that routes calls through the
// policy gateway using the given role.
func (g *PolicyGateway) ProviderForRole(role ModelRole) Provider {
	return &RoleProvider{gateway: g, role: role}
}

func (g *PolicyGateway) resolve(role ModelRole) ProviderProfile {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if profile, ok := g.roles[role]; ok && profile.Provider != nil {
		return profile
	}
	return g.fallback
}

func (g *PolicyGateway) record(ctx context.Context, profile ProviderProfile, role ModelRole, usage UsageStats) {
	g.mu.RLock()
	recorder := g.recorder
	g.mu.RUnlock()
	if recorder == nil {
		return
	}

	mc := ModelContextFrom(ctx)
	if mc.Role == "" {
		mc.Role = role
	}
	model := profile.Model
	if model == "" {
		model = GetModelName(profile.Provider)
	}
	recorder.RecordModelUsage(ctx, UsageEvent{
		Context:      mc,
		ProviderName: profile.ProviderName,
		Model:        model,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	})
}

// RoleProvider is a provider facade for one logical model role.
type RoleProvider struct {
	gateway *PolicyGateway
	role    ModelRole
}

func (p *RoleProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	req := ChatRequest{Messages: messages}
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (p *RoleProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	profile := p.gateway.resolve(p.role)
	ctx = ensureModelRole(ctx, p.role)
	req = withRequestRole(req, p.role)
	resp, err := profile.Provider.Chat(ctx, req)
	if err == nil && resp != nil {
		p.gateway.record(ctx, profile, p.role, resp.Usage)
	}
	return resp, err
}

func (p *RoleProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	profile := p.gateway.resolve(p.role)
	ctx = ensureModelRole(ctx, p.role)
	req = withRequestRole(req, p.role)
	ch, err := profile.Provider.StreamChat(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		for ev := range ch {
			if ev.Usage != nil {
				p.gateway.record(ctx, profile, p.role, *ev.Usage)
			}
			out <- ev
		}
	}()
	return out, nil
}

func (p *RoleProvider) SetModel(model string) {
	profile := p.gateway.resolve(p.role)
	if SetModelName(profile.Provider, model) {
		p.gateway.mu.Lock()
		if existing, ok := p.gateway.roles[p.role]; ok {
			existing.Model = model
			p.gateway.roles[p.role] = existing
		} else {
			p.gateway.fallback.Model = model
		}
		p.gateway.mu.Unlock()
	}
}

func (p *RoleProvider) GetModel() string {
	profile := p.gateway.resolve(p.role)
	if profile.Model != "" {
		return profile.Model
	}
	return GetModelName(profile.Provider)
}

func ensureModelRole(ctx context.Context, role ModelRole) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	mc := ModelContextFrom(ctx)
	if mc.Role == role {
		return ctx
	}
	mc.Role = role
	return WithModelContext(ctx, mc)
}

func withRequestRole(req ChatRequest, role ModelRole) ChatRequest {
	if req.Options == nil {
		req.Options = make(map[string]interface{})
	}
	req.Options["model_role"] = string(role)
	return req
}

// SupportsNativeTools resolves the role's current provider and forwards the
// probe, so prompt assembly reflects the provider actually routed this turn.
func (p *RoleProvider) SupportsNativeTools() bool {
	profile := p.gateway.resolve(p.role)
	if profile.Provider == nil {
		return false
	}
	return ProviderSupportsNativeTools(profile.Provider)
}
