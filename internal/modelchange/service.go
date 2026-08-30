package modelchange

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"selfmind/internal/platform/config"
)

const (
	stateSchemaVersion = 5
	defaultHistorySize = 10
	defaultConfirmTTL  = 10 * time.Minute
)

type Route string

const (
	RoutePrimary          Route = "primary"
	RouteAuxiliary        Route = "auxiliary"
	RouteFastClassifier   Route = "fast_classifier"
	RouteMemoryExtract    Route = "memory_extract"
	RouteBackgroundReview Route = "background_review"
	RouteSkillCurator     Route = "skill_curator"
	RouteSemanticRecall   Route = "semantic_recall"
	RouteSummarizer       Route = "summarizer"
)

// RoleSelections is deliberately fixed to the six supported background roles.
// Keeping this value comparable makes drift and generation checks exact while
// preventing arbitrary role names from becoming part of the personal-edition
// model manager contract.
type RoleSelections struct {
	FastClassifier   config.ModelSelectionConfig `json:"fast_classifier"`
	MemoryExtract    config.ModelSelectionConfig `json:"memory_extract"`
	BackgroundReview config.ModelSelectionConfig `json:"background_review"`
	SkillCurator     config.ModelSelectionConfig `json:"skill_curator"`
	SemanticRecall   config.ModelSelectionConfig `json:"semantic_recall"`
	Summarizer       config.ModelSelectionConfig `json:"summarizer"`
}

type ChangeStatus string

const (
	StatusAwaitingConfirmation ChangeStatus = "awaiting_confirmation"
	StatusValidating           ChangeStatus = "validating"
	StatusCommitting           ChangeStatus = "committing"
	StatusAwaitingSafeBoundary ChangeStatus = "awaiting_safe_boundary"
	// StatusAwaitingRestart is kept as a source-compatible name while schema-v1
	// files are migrated to the more precise awaiting_safe_boundary value.
	StatusAwaitingRestart  ChangeStatus = StatusAwaitingSafeBoundary
	StatusDraining         ChangeStatus = "draining"
	StatusRestarting       ChangeStatus = "restarting"
	StatusStarting         ChangeStatus = "starting"
	StatusApplied          ChangeStatus = "applied"
	StatusRolledBack       ChangeStatus = "rolled_back"
	StatusRecoveryRequired ChangeStatus = "recovery_required"
	StatusSuperseded       ChangeStatus = "superseded"
	StatusConflict         ChangeStatus = "conflict"
	StatusFailed           ChangeStatus = "failed"
	StatusCancelled        ChangeStatus = "cancelled"
)

type FailureClass string

const (
	FailureModel          FailureClass = "model"
	FailureInfrastructure FailureClass = "infrastructure"
	FailureConflict       FailureClass = "conflict"
)

type Transition struct {
	Status ChangeStatus `json:"status"`
	At     time.Time    `json:"at"`
}

// Snapshot intentionally contains only model route selection. Non-secret
// provider connection changes have their own typed snapshots; raw credentials
// never enter model state or history.
type Snapshot struct {
	Primary   config.ModelSelectionConfig `json:"primary"`
	Auxiliary config.ModelSelectionConfig `json:"auxiliary"`
	Roles     RoleSelections              `json:"roles"`
}

var disabledAuxiliary = false

// Fingerprint identifies effective non-secret model routing. It is safe to
// expose through Gateway status so a CLI can reject a stale live runtime
// without publishing credentials or provider endpoint configuration.
func (s Snapshot) Fingerprint() string {
	data, err := json.Marshal(normalizeSnapshot(s))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func providerConfigFingerprint(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	withoutSecret := func(endpoint config.ProviderEndpoint) config.ProviderEndpoint {
		endpoint.APIKey = ""
		return endpoint
	}
	builtins := make(map[string]config.ProviderEndpoint, len(cfg.Providers.Builtins))
	for id, endpoint := range cfg.Providers.Builtins {
		builtins[normalizeProviderConnectionID(id)] = withoutSecret(endpoint)
	}
	custom := append([]config.CustomProvider(nil), cfg.Providers.Custom...)
	for index := range custom {
		custom[index].APIKey = ""
		custom[index].Name = normalizeProviderConnectionID(custom[index].Name)
	}
	sort.Slice(custom, func(i, j int) bool { return custom[i].Name < custom[j].Name })
	legacy := make(map[string]config.ProviderEndpoint, len(cfg.ProviderProfiles))
	for id, endpoint := range cfg.ProviderProfiles {
		legacy[normalizeProviderConnectionID(id)] = withoutSecret(endpoint)
	}
	payload := struct {
		OpenAI    config.ProviderEndpoint            `json:"openai,omitempty"`
		Anthropic config.ProviderEndpoint            `json:"anthropic,omitempty"`
		Google    config.ProviderEndpoint            `json:"google,omitempty"`
		Builtins  map[string]config.ProviderEndpoint `json:"builtins,omitempty"`
		Custom    []config.CustomProvider            `json:"custom,omitempty"`
		Legacy    map[string]config.ProviderEndpoint `json:"legacy,omitempty"`
	}{
		OpenAI: withoutSecret(cfg.Providers.OpenAI), Anthropic: withoutSecret(cfg.Providers.Anthropic),
		Google: withoutSecret(cfg.Providers.Google), Builtins: builtins, Custom: custom, Legacy: legacy,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

type ProbeResult struct {
	Route        Route        `json:"route"`
	OK           bool         `json:"ok"`
	Provider     string       `json:"provider,omitempty"`
	Model        string       `json:"model,omitempty"`
	LatencyMS    int64        `json:"latency_ms,omitempty"`
	Error        string       `json:"error,omitempty"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
}

type Change struct {
	ID                 string           `json:"id"`
	Source             string           `json:"source"`
	Status             ChangeStatus     `json:"status"`
	ExpectedGeneration int64            `json:"expected_generation"`
	Previous           Snapshot         `json:"previous"`
	Candidate          Snapshot         `json:"candidate"`
	ChangedRoutes      []Route          `json:"changed_routes"`
	CreatedAt          time.Time        `json:"created_at"`
	ConfirmBy          time.Time        `json:"confirm_by,omitempty"`
	ConfirmedAt        time.Time        `json:"confirmed_at,omitempty"`
	FinishedAt         time.Time        `json:"finished_at,omitempty"`
	Failure            string           `json:"failure,omitempty"`
	FailureClass       FailureClass     `json:"failure_class,omitempty"`
	PhaseStartedAt     time.Time        `json:"phase_started_at,omitempty"`
	RestartAttempts    int              `json:"restart_attempts,omitempty"`
	LastAttemptAt      time.Time        `json:"last_attempt_at,omitempty"`
	RestartScheduledAt time.Time        `json:"restart_scheduled_at,omitempty"`
	ServiceManager     string           `json:"service_manager,omitempty"`
	CredentialStage    string           `json:"credential_stage,omitempty"`
	ProviderChanges    []ProviderChange `json:"provider_changes,omitempty"`
	Probes             []ProbeResult    `json:"probes,omitempty"`
	Transitions        []Transition     `json:"transitions,omitempty"`
}

type State struct {
	SchemaVersion              int                      `json:"schema_version"`
	Generation                 int64                    `json:"generation"`
	Running                    Snapshot                 `json:"running"`
	RunningProviderFingerprint string                   `json:"running_provider_fingerprint,omitempty"`
	RunningVerifiedAt          time.Time                `json:"running_verified_at,omitempty"`
	ForegroundVerifiedAt       time.Time                `json:"foreground_verified_at,omitempty"`
	BackgroundVerifiedAt       time.Time                `json:"background_verified_at,omitempty"`
	RouteReadiness             map[Route]RouteReadiness `json:"route_readiness,omitempty"`
	Pending                    *Change                  `json:"pending,omitempty"`
	History                    []Change                 `json:"history,omitempty"`
	UpdatedAt                  time.Time                `json:"updated_at"`
}

type Status struct {
	Generation        int64     `json:"generation"`
	Running           Snapshot  `json:"running"`
	RunningVerifiedAt time.Time `json:"running_verified_at,omitempty"`
	Configured        Snapshot  `json:"configured"`
	Pending           *Change   `json:"pending,omitempty"`
	History           []Change  `json:"history,omitempty"`
	Readiness         Readiness `json:"readiness"`
}

type Readiness struct {
	Foreground        bool                     `json:"foreground"`
	Background        bool                     `json:"background"`
	BackgroundEnabled bool                     `json:"background_enabled"`
	Degraded          bool                     `json:"degraded"`
	ForegroundReason  string                   `json:"foreground_reason,omitempty"`
	BackgroundReason  string                   `json:"background_reason,omitempty"`
	Routes            map[Route]RouteReadiness `json:"routes,omitempty"`
}

// RouteReadiness is the durable health boundary for one background role. A
// failed semantic_recall route therefore cannot erase proof for auxiliary or
// unrelated maintenance roles.
type RouteReadiness struct {
	Ready        bool         `json:"ready"`
	VerifiedAt   time.Time    `json:"verified_at,omitempty"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	Reason       string       `json:"reason,omitempty"`
}

// ModelReady reports whether the effective routes are the same routes that
// reached a verified running boundary and no model transaction remains open.
func (s Status) ModelReady() bool {
	return s.ForegroundReady()
}

func (s Status) ForegroundReady() bool {
	return s.Readiness.Foreground
}

func (s Status) BackgroundReady() bool {
	return s.Readiness.Background
}

// RouteReady reports role-scoped readiness. Foreground callers continue to use
// ForegroundReady; background workers use their own role or the auxiliary
// fallback floor instead of the all-background aggregate.
func (s Status) RouteReady(route Route) bool {
	if route == RoutePrimary {
		return s.ForegroundReady()
	}
	state, ok := s.Readiness.Routes[route]
	return ok && state.Ready
}

func (s Status) RouteReadiness(route Route) RouteReadiness {
	return s.Readiness.Routes[route]
}

type PrepareRequest struct {
	Candidate           Snapshot
	Source              string
	ExpectedGeneration  int64
	RequireConfirmation bool
	ReplacePending      bool
	ForceRevalidate     bool
	CredentialStage     string
	ProviderChanges     []ProviderChange
	ValidateRoutes      []Route
}

type PrepareResult struct {
	Change       Change
	Generation   int64
	NeedsConfirm bool
	NeedsRestart bool
}

// Validator runs live role-aware contract probes against a candidate config.
// The service never fabricates success when no validator is installed.
type Validator func(context.Context, *config.Config, []Route) []ProbeResult

// CredentialTransaction keeps secrets out of model state while allowing their
// activation to share the route transaction's validation, commit, rollback,
// and healthy-finalization boundaries.
type CredentialTransaction interface {
	OverlayStage(string, *config.Config) error
	CommitStage(string) error
	RollbackStage(string) error
	FinalizeStage(string) error
	DiscardStage(string) error
}

type Service struct {
	ConfigPath  string
	Validate    Validator
	Credentials CredentialTransaction
	Now         func() time.Time
	ConfirmTTL  time.Duration
	HistoryMax  int
	mu          sync.Mutex
}

var ErrGenerationConflict = errors.New("model configuration generation changed")
var ErrRecoveryRequired = errors.New("model change requires recovery")

func SnapshotFromConfig(cfg *config.Config) Snapshot {
	if cfg == nil {
		return Snapshot{}
	}
	return normalizeSnapshot(Snapshot{
		Primary:   cfg.EffectivePrimary(),
		Auxiliary: cfg.EffectiveAuxiliary(),
		Roles: RoleSelections{
			FastClassifier:   roleSelection(cfg.Models.Roles[string(RouteFastClassifier)]),
			MemoryExtract:    roleSelection(cfg.Models.Roles[string(RouteMemoryExtract)]),
			BackgroundReview: roleSelection(cfg.Models.Roles[string(RouteBackgroundReview)]),
			SkillCurator:     roleSelection(cfg.Models.Roles[string(RouteSkillCurator)]),
			SemanticRecall:   roleSelection(cfg.Models.Roles[string(RouteSemanticRecall)]),
			Summarizer:       roleSelection(cfg.Models.Roles[string(RouteSummarizer)]),
		},
	})
}

func ApplySnapshot(cfg *config.Config, snapshot Snapshot) {
	if cfg == nil {
		return
	}
	snapshot = normalizeSnapshot(snapshot)
	cfg.Models.Primary = snapshot.Primary
	cfg.Models.Auxiliary = snapshot.Auxiliary
	if cfg.Models.Roles == nil {
		cfg.Models.Roles = make(map[string]config.ModelRoleConfig)
	}
	for _, route := range ManagedRoleRoutes() {
		applyRoleSelection(cfg.Models.Roles, route, selectionForRoute(snapshot, route))
	}
	// coding_agent was an early foreground override that could make the model
	// shown as Main differ from the model actually answering. Main is now the
	// sole foreground authority. Remove a selection-only legacy entry; retain
	// any YAML-only transport details as inert data so the manager never destroys
	// advanced user configuration.
	if legacy, ok := cfg.Models.Roles["coding_agent"]; ok {
		if roleAdvancedConfigEmpty(legacy) {
			delete(cfg.Models.Roles, "coding_agent")
		} else {
			legacy.Provider = ""
			legacy.Model = ""
			legacy.Reasoning = ""
			legacy.ReasoningEffort = ""
			legacy.ServiceTier = ""
			legacy.ContextLength = 0
			cfg.Models.Roles["coding_agent"] = legacy
		}
	}
	// New writes converge on models.* and cannot be shadowed by legacy fields.
	cfg.Model.Provider = ""
	cfg.Model.Default = ""
	cfg.Agent.Provider = ""
	cfg.Agent.Model = ""
}

func ChangedRoutes(before, after Snapshot) []Route {
	before = normalizeSnapshot(before)
	after = normalizeSnapshot(after)
	var routes []Route
	if before.Primary != after.Primary {
		routes = append(routes, RoutePrimary)
	}
	if before.Auxiliary != after.Auxiliary {
		routes = append(routes, RouteAuxiliary)
	}
	for _, route := range ManagedRoleRoutes() {
		if selectionForRoute(before, route) != selectionForRoute(after, route) {
			routes = append(routes, route)
		}
	}
	return routes
}

func (s *Service) Inspect() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	return s.inspectLocked()
}

// ValidateCandidate probes an in-memory draft without creating a pending
// change, advancing the generation, or writing config.yaml. Interactive model
// managers use it after each completed selection so invalid credentials or
// contracts are reported before the final review.
func (s *Service) ValidateCandidate(ctx context.Context, candidate Snapshot, routes []Route) ([]ProbeResult, error) {
	return s.ValidateCandidateWithConfig(ctx, candidate, routes, nil)
}

// ValidateCandidateWithConfig applies a transient, in-memory config overlay
// before probing. It exists for secrets entered by the interactive manager:
// credentials can be tested without entering snapshots, history, or YAML.
func (s *Service) ValidateCandidateWithConfig(ctx context.Context, candidate Snapshot, routes []Route, overlay func(*config.Config) error) ([]ProbeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return nil, err
	}
	defer unlock()
	candidate = normalizeSnapshot(candidate)
	if len(routes) == 0 {
		cfg, loadErr := config.LoadConfig(config.Options{Path: s.ConfigPath})
		if loadErr != nil {
			return nil, loadErr
		}
		routes = ChangedRoutes(SnapshotFromConfig(cfg), candidate)
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("model selection is unchanged")
	}
	if err := validateSnapshot(candidate, routes); err != nil {
		return nil, err
	}
	candidateCfg, err := configWithSnapshot(s.ConfigPath, candidate)
	if err != nil {
		return nil, err
	}
	if overlay != nil {
		if err := overlay(candidateCfg); err != nil {
			return nil, err
		}
	}
	return s.validate(ctx, candidateCfg, routes), nil
}

func (s *Service) inspectLocked() (Status, error) {
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	if s.expireConfirmation(&state) {
		if err := s.save(state); err != nil {
			return Status{}, err
		}
	}
	return Status{
		Generation: state.Generation, Running: state.Running, RunningVerifiedAt: state.RunningVerifiedAt,
		Configured: logicalConfigured(cfg, state.Pending), Pending: cloneChange(state.Pending),
		History:   append([]Change(nil), state.History...),
		Readiness: readinessFor(cfg, state),
	}, nil
}

func (s *Service) Prepare(ctx context.Context, req PrepareRequest) (PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return PrepareResult{}, err
	}
	defer unlock()
	return s.prepareLocked(ctx, req)
}

func (s *Service) prepareLocked(ctx context.Context, req PrepareRequest) (PrepareResult, error) {
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return PrepareResult{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return PrepareResult{}, err
	}
	if s.expireConfirmation(&state) {
		if err := s.save(state); err != nil {
			return PrepareResult{}, err
		}
	}
	if req.ExpectedGeneration > 0 && req.ExpectedGeneration != state.Generation {
		return PrepareResult{}, fmt.Errorf("%w: expected %d, current %d", ErrGenerationConflict, req.ExpectedGeneration, state.Generation)
	}
	if state.Pending != nil {
		if !req.ReplacePending {
			return PrepareResult{}, fmt.Errorf("model change %s is already %s; cancel or explicitly replace it", state.Pending.ID, state.Pending.Status)
		}
		if err := s.cancelPending(cfg, &state, StatusSuperseded, "replaced by a newer explicit request"); err != nil {
			return PrepareResult{}, err
		}
	}
	configured := SnapshotFromConfig(cfg)
	if configured != state.Running {
		return PrepareResult{}, fmt.Errorf("config.yaml differs from the running model routes; restart SelfMind to validate that manual edit before creating another change")
	}
	candidate := normalizeSnapshot(req.Candidate)
	changed := ChangedRoutes(configured, candidate)
	if len(req.ProviderChanges) > 0 {
		changed = mergeRoutes(changed, req.ValidateRoutes)
		for _, providerChange := range req.ProviderChanges {
			changed = mergeRoutes(changed, routesUsingProvider(candidate, providerChange.ID))
		}
	}
	if len(changed) == 0 {
		if !req.ForceRevalidate {
			return PrepareResult{}, fmt.Errorf("model selection is unchanged")
		}
		// Credentials are deliberately absent from Snapshot. After a
		// credential-only repair, revalidate every route and restart the same
		// selection so the verified running boundary can be established.
		changed = mergeRoutes([]Route{RoutePrimary, RouteAuxiliary}, ManagedRoleRoutes())
	}
	if err := validateSnapshot(candidate, changed); err != nil {
		return PrepareResult{}, err
	}
	now := s.now()
	change := Change{
		ID: newChangeID(), Source: normalizeSource(req.Source), Status: StatusAwaitingConfirmation,
		ExpectedGeneration: state.Generation, Previous: configured, Candidate: candidate,
		ChangedRoutes: changed, CreatedAt: now, ConfirmBy: now.Add(s.confirmTTL()), PhaseStartedAt: now,
		CredentialStage: strings.TrimSpace(req.CredentialStage),
		ProviderChanges: append([]ProviderChange(nil), req.ProviderChanges...),
		Transitions:     []Transition{{Status: StatusAwaitingConfirmation, At: now}},
	}
	state.Pending = &change
	state.Generation++
	if err := s.save(state); err != nil {
		return PrepareResult{}, err
	}
	result := PrepareResult{Change: change, Generation: state.Generation, NeedsConfirm: true}
	if req.RequireConfirmation {
		return result, nil
	}
	confirmed, err := s.confirmLocked(ctx, change.ID)
	if err != nil {
		return PrepareResult{}, err
	}
	return confirmed, nil
}

func (s *Service) Confirm(ctx context.Context, id string) (PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return PrepareResult{}, err
	}
	defer unlock()
	return s.confirmLocked(ctx, id)
}

func (s *Service) confirmLocked(ctx context.Context, id string) (PrepareResult, error) {
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return PrepareResult{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return PrepareResult{}, err
	}
	id = strings.TrimSpace(id)
	pending := state.Pending
	if pending == nil || !strings.EqualFold(id, pending.ID) {
		if historical, ok := findChange(state.History, id); ok {
			return PrepareResult{Change: historical, Generation: state.Generation}, nil
		}
		return PrepareResult{}, fmt.Errorf("pending model change %q was not found", id)
	}
	if pending.Status != StatusAwaitingConfirmation && pending.Status != StatusValidating {
		return PrepareResult{
			Change: *cloneChange(pending), Generation: state.Generation,
			NeedsRestart: pendingRestartStatus(pending.Status),
		}, nil
	}
	now := s.now()
	if pending.Status == StatusAwaitingConfirmation && !pending.ConfirmBy.IsZero() && now.After(pending.ConfirmBy) {
		s.finishPending(&state, StatusFailed, "confirmation expired", nil)
		if saveErr := s.save(state); saveErr != nil {
			return PrepareResult{}, saveErr
		}
		return PrepareResult{}, fmt.Errorf("model change %s expired; create a new request", pending.ID)
	}
	if SnapshotFromConfig(cfg) != pending.Previous || !ProviderChangesMatch(cfg, pending.ProviderChanges, false) {
		return PrepareResult{}, fmt.Errorf("%w: config.yaml changed after the preview was created", ErrGenerationConflict)
	}
	if pending.ConfirmedAt.IsZero() {
		pending.ConfirmedAt = now
	}
	s.transition(pending, StatusValidating, now)
	state.Generation++
	if err := s.save(state); err != nil {
		return PrepareResult{}, err
	}
	candidateCfg, err := configWithChange(s.ConfigPath, pending.Candidate, pending.ProviderChanges, true)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := s.overlayCredentialStage(pending.CredentialStage, candidateCfg); err != nil {
		s.finishPendingClass(&state, StatusFailed, FailureInfrastructure, err.Error(), nil)
		_ = s.discardCredentialStage(pending.CredentialStage)
		if saveErr := s.save(state); saveErr != nil {
			return PrepareResult{}, saveErr
		}
		return PrepareResult{}, err
	}
	probes := s.validate(ctx, candidateCfg, pending.ChangedRoutes)
	if failureClass, probeErr := failedProbes(probes); probeErr != nil {
		_ = s.discardCredentialStage(pending.CredentialStage)
		s.finishPendingClass(&state, StatusFailed, failureClass, probeErr.Error(), probes)
		if saveErr := s.save(state); saveErr != nil {
			return PrepareResult{}, saveErr
		}
		return PrepareResult{}, probeErr
	}
	pending.Probes = probes
	s.transition(pending, StatusAwaitingSafeBoundary, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{Change: *cloneChange(state.Pending), Generation: state.Generation, NeedsRestart: true}, nil
}

func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return err
	}
	if s.expireConfirmation(&state) {
		if err := s.save(state); err != nil {
			return err
		}
	}
	if state.Pending == nil {
		return fmt.Errorf("there is no pending model change")
	}
	if strings.TrimSpace(id) != "" && !strings.EqualFold(strings.TrimSpace(id), state.Pending.ID) {
		return fmt.Errorf("pending model change %q was not found", strings.TrimSpace(id))
	}
	return s.cancelPending(cfg, &state, StatusCancelled, "cancelled by the user")
}

func (s *Service) PrepareRollback(ctx context.Context, source string, requireConfirmation bool) (PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return PrepareResult{}, err
	}
	defer unlock()
	status, err := s.inspectLocked()
	if err != nil {
		return PrepareResult{}, err
	}
	if status.Pending != nil {
		return PrepareResult{}, fmt.Errorf("model change %s is already %s", status.Pending.ID, status.Pending.Status)
	}
	for i := len(status.History) - 1; i >= 0; i-- {
		record := status.History[i]
		if record.Status != StatusApplied || len(ChangedRoutes(record.BeforeSnapshot(), status.Running)) == 0 {
			continue
		}
		return s.prepareLocked(ctx, PrepareRequest{
			Candidate: record.Previous, Source: normalizeSource(source) + ":rollback",
			ExpectedGeneration: status.Generation, RequireConfirmation: requireConfirmation,
		})
	}
	return PrepareResult{}, fmt.Errorf("no previous successful model snapshot is available")
}

// BeginDraining commits the validated candidate only after the detached
// orchestrator is ready to ask the old daemon for a safe shutdown. Until this
// transition config.yaml remains the last healthy running configuration.
func (s *Service) BeginDraining(id string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return Status{}, err
	}
	switch change.Status {
	case StatusDraining, StatusRestarting, StatusStarting:
		return s.InspectWithState(cfg, state), nil
	case StatusRecoveryRequired:
		return s.InspectWithState(cfg, state), fmt.Errorf("%w: %s", ErrRecoveryRequired, change.Failure)
	case StatusAwaitingSafeBoundary, StatusCommitting:
		// continue
	default:
		return Status{}, fmt.Errorf("model change %s is %s and cannot begin draining", change.ID, change.Status)
	}
	configured := SnapshotFromConfig(cfg)
	isPrevious := configured == change.Previous && ProviderChangesMatch(cfg, change.ProviderChanges, false)
	isCandidate := configured == change.Candidate && ProviderChangesMatch(cfg, change.ProviderChanges, true)
	if !isPrevious && !isCandidate {
		s.finishPendingClass(&state, StatusConflict, FailureConflict, "config.yaml changed before the safe restart boundary", change.Probes)
		if saveErr := s.save(state); saveErr != nil {
			return Status{}, saveErr
		}
		return s.InspectWithState(cfg, state), fmt.Errorf("%w: config.yaml changed before model change %s could start", ErrGenerationConflict, change.ID)
	}
	if isPrevious {
		s.transition(change, StatusCommitting, s.now())
		state.Generation++
		if err := s.save(state); err != nil {
			return Status{}, err
		}
		ApplySnapshot(cfg, change.Candidate)
		ApplyProviderChanges(cfg, change.ProviderChanges, true)
		if err := config.SaveConfig(s.ConfigPath, cfg); err != nil {
			change.Failure = "write config at safe boundary: " + err.Error()
			change.FailureClass = FailureInfrastructure
			s.transition(change, StatusRecoveryRequired, s.now())
			state.Generation++
			_ = s.save(state)
			return s.InspectWithState(cfg, state), err
		}
	}
	s.transition(change, StatusDraining, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// MarkRestarting records that the old owner has released gateway.lock and the
// platform adapter is about to start the replacement process.
func (s *Service) MarkRestarting(id, serviceManager string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return Status{}, err
	}
	if change.Status == StatusRestarting || change.Status == StatusStarting {
		return s.InspectWithState(cfg, state), nil
	}
	if change.Status != StatusDraining && change.Status != StatusCommitting {
		return Status{}, fmt.Errorf("model change %s is %s and cannot restart", change.ID, change.Status)
	}
	if SnapshotFromConfig(cfg) != change.Candidate || !ProviderChangesMatch(cfg, change.ProviderChanges, true) {
		return Status{}, fmt.Errorf("%w: candidate config is not committed for %s", ErrGenerationConflict, change.ID)
	}
	if err := s.commitCredentialStage(change.CredentialStage); err != nil {
		change.Failure = "commit staged provider credentials: " + err.Error()
		change.FailureClass = FailureInfrastructure
		s.transition(change, StatusRecoveryRequired, s.now())
		state.Generation++
		_ = s.save(state)
		return s.InspectWithState(cfg, state), fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
	}
	change.ServiceManager = strings.TrimSpace(serviceManager)
	s.transition(change, StatusRestarting, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// MarkRecoveryRequired parks an infrastructure failure without blaming or
// silently rolling back a candidate that already passed both route probes.
func (s *Service) MarkRecoveryRequired(id string, cause error) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return Status{}, err
	}
	if change.Status == StatusApplied || change.Status == StatusRolledBack {
		return s.InspectWithState(cfg, state), nil
	}
	message := "gateway restart did not become healthy"
	if cause != nil && strings.TrimSpace(cause.Error()) != "" {
		message = cause.Error()
	}
	change.Failure = message
	change.FailureClass = FailureInfrastructure
	s.transition(change, StatusRecoveryRequired, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// ClaimRestart makes scheduling idempotent across a lost HTTP response or two
// clients confirming the same transaction. The returned bool is true only for
// the caller that owns spawning the detached helper.
func (s *Service) ClaimRestart(id string) (bool, Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return false, Change{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return false, Change{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return false, Change{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		if historical, ok := findChange(state.History, strings.TrimSpace(id)); ok {
			return false, historical, nil
		}
		return false, Change{}, err
	}
	if !pendingRestartStatus(change.Status) {
		return false, *cloneChange(change), fmt.Errorf("model change %s is %s and cannot schedule a restart", change.ID, change.Status)
	}
	if !change.RestartScheduledAt.IsZero() {
		return false, *cloneChange(change), nil
	}
	change.RestartScheduledAt = s.now()
	state.Generation++
	if err := s.save(state); err != nil {
		return false, Change{}, err
	}
	return true, *cloneChange(change), nil
}

func (s *Service) ReleaseRestartClaim(id string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return err
	}
	change.RestartScheduledAt = time.Time{}
	if cause != nil {
		change.Failure = "automatic restart was not scheduled: " + cause.Error()
		change.FailureClass = FailureInfrastructure
	}
	state.Generation++
	return s.save(state)
}

// RetryRecovery is an explicit user action. It resets the automatic-attempt
// circuit and lets the ordinary platform adapter start the committed candidate
// again; it never starts a process itself.
func (s *Service) RetryRecovery(id string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return Status{}, err
	}
	if change.Status != StatusRecoveryRequired {
		return Status{}, fmt.Errorf("model change %s is %s, not recovery_required", change.ID, change.Status)
	}
	if SnapshotFromConfig(cfg) != change.Candidate || !ProviderChangesMatch(cfg, change.ProviderChanges, true) {
		return Status{}, fmt.Errorf("%w: candidate config changed before retry", ErrGenerationConflict)
	}
	change.Failure = ""
	change.FailureClass = ""
	change.RestartAttempts = 0
	change.LastAttemptAt = time.Time{}
	change.RestartScheduledAt = s.now()
	s.transition(change, StatusRestarting, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// RestorePrevious is the explicit recovery escape hatch. It refuses to
// overwrite unrelated manual edits and records a terminal rolled_back receipt.
func (s *Service) RestorePrevious(id string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	change, err := pendingChange(&state, id)
	if err != nil {
		return Status{}, err
	}
	if change.Status != StatusRecoveryRequired {
		return Status{}, fmt.Errorf("model change %s is %s, not recovery_required", change.ID, change.Status)
	}
	configured := SnapshotFromConfig(cfg)
	isCandidate := configured == change.Candidate && ProviderChangesMatch(cfg, change.ProviderChanges, true)
	isPrevious := configured == change.Previous && ProviderChangesMatch(cfg, change.ProviderChanges, false)
	if !isCandidate && !isPrevious {
		return Status{}, fmt.Errorf("%w: config.yaml changed before recovery", ErrGenerationConflict)
	}
	if isCandidate {
		ApplySnapshot(cfg, change.Previous)
		ApplyProviderChanges(cfg, change.ProviderChanges, false)
		if err := config.SaveConfig(s.ConfigPath, cfg); err != nil {
			return Status{}, err
		}
	}
	if err := s.rollbackCredentialStage(change.CredentialStage); err != nil {
		return Status{}, fmt.Errorf("restore provider credentials: %w", err)
	}
	finished := *change
	s.transition(&finished, StatusRolledBack, s.now())
	finished.FinishedAt = s.now()
	if strings.TrimSpace(finished.Failure) == "" {
		finished.Failure = "restored the last healthy model by explicit user request"
	}
	state.Pending = nil
	state.History = appendBounded(state.History, finished, s.historyMax())
	// The previous selection is known-good historically, but the process that
	// currently owns the listener was built from the failed candidate. Clear
	// the running proof so no request can use the restored config until a new
	// process probes it and crosses MarkStartupHealthy.
	state.RunningVerifiedAt = time.Time{}
	state.ForegroundVerifiedAt = time.Time{}
	state.BackgroundVerifiedAt = time.Time{}
	state.RouteReadiness = nil
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// ReconcileStartup validates a committed or manually edited route snapshot in
// the daemon environment before Agent construction. A deterministic failure
// restores the last running selection in config.yaml and lets the same process
// continue booting with that known-good route.
func (s *Service) ReconcileStartup(ctx context.Context) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, false, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, false, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, false, err
	}
	if s.expireConfirmation(&state) {
		if err := s.save(state); err != nil {
			return Status{}, false, err
		}
	}
	configured := SnapshotFromConfig(cfg)
	change := state.Pending
	if change != nil && change.Status == StatusAwaitingConfirmation {
		if configured == change.Previous && ProviderChangesMatch(cfg, change.ProviderChanges, false) {
			return s.InspectWithState(cfg, state), false, nil
		}
		s.finishPendingClass(&state, StatusConflict, FailureConflict, "config.yaml changed while confirmation was pending", change.Probes)
		change = nil
	}
	if change != nil && change.Status == StatusRecoveryRequired {
		// Keep the control plane and sole Model Manager reachable. Work remains
		// parked because ModelReady is false until the user retries or restores.
		return s.InspectWithState(cfg, state), false, nil
	}
	if change != nil && (change.Status == StatusAwaitingSafeBoundary || change.Status == StatusValidating) && configured == change.Previous && ProviderChangesMatch(cfg, change.ProviderChanges, false) {
		// The old daemon may have crashed before the detached helper reached the
		// safe boundary. Boot the last healthy route; the helper (or a later
		// explicit retry) will commit the candidate before another restart.
		return s.InspectWithState(cfg, state), false, nil
	}
	if change != nil {
		matchesCandidateProviders := ProviderChangesMatch(cfg, change.ProviderChanges, true)
		matchesPreviousProviders := ProviderChangesMatch(cfg, change.ProviderChanges, false)
		isCandidate := configured == change.Candidate && matchesCandidateProviders
		isPrevious := configured == change.Previous && matchesPreviousProviders
		switch {
		case isCandidate:
			// The candidate was atomically committed at the drain boundary.
		case isPrevious:
			if change.Status != StatusCommitting && change.Status != StatusDraining && change.Status != StatusRestarting {
				return s.InspectWithState(cfg, state), false, nil
			}
			// Crash recovery for the narrow window between recording committing
			// and replacing config.yaml. This process owns the gateway lock before
			// ReconcileStartup is called, so completing the transaction is safe.
			ApplySnapshot(cfg, change.Candidate)
			ApplyProviderChanges(cfg, change.ProviderChanges, true)
			if err := config.SaveConfig(s.ConfigPath, cfg); err != nil {
				change.Failure = "complete candidate config after restart: " + err.Error()
				change.FailureClass = FailureInfrastructure
				s.transition(change, StatusRecoveryRequired, s.now())
				state.Generation++
				_ = s.save(state)
				return s.InspectWithState(cfg, state), false, fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
			}
			configured = change.Candidate
		default:
			s.finishPendingClass(&state, StatusConflict, FailureConflict, "config.yaml changed while the candidate was pending", change.Probes)
			if err := s.save(state); err != nil {
				return Status{}, false, err
			}
			change = nil
		}
		if change != nil && (change.Status == StatusCommitting || change.Status == StatusDraining || change.Status == StatusRestarting || change.Status == StatusStarting) {
			if err := s.commitCredentialStage(change.CredentialStage); err != nil {
				change.Failure = "commit staged provider credentials during startup recovery: " + err.Error()
				change.FailureClass = FailureInfrastructure
				s.transition(change, StatusRecoveryRequired, s.now())
				state.Generation++
				_ = s.save(state)
				return s.InspectWithState(cfg, state), false, fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
			}
		}
	}
	providerFingerprint := providerConfigFingerprint(cfg)
	providerConfigMatches := providerFingerprint == state.RunningProviderFingerprint
	if change == nil && configured == state.Running && providerConfigMatches && !state.RunningVerifiedAt.IsZero() {
		return s.InspectWithState(cfg, state), false, nil
	}
	if change == nil {
		now := s.now()
		changedRoutes := ChangedRoutes(state.Running, configured)
		source := "manual-config"
		if state.RunningVerifiedAt.IsZero() {
			// A newly created state file records the configured snapshot for drift
			// comparison, not as proof that it ever ran. The first daemon startup
			// must validate every effective route before /health may establish
			// Model Readiness.
			source = "initial-startup"
			changedRoutes = append([]Route{RoutePrimary, RouteAuxiliary}, ManagedRoleRoutes()...)
		} else if !providerConfigMatches {
			source = "manual-provider-config"
			changedRoutes = append([]Route{RoutePrimary, RouteAuxiliary}, ManagedRoleRoutes()...)
		}
		manual := Change{
			ID: newChangeID(), Source: source, Status: StatusStarting,
			ExpectedGeneration: state.Generation, Previous: state.Running, Candidate: configured,
			ChangedRoutes: changedRoutes, CreatedAt: now, ConfirmedAt: now,
			PhaseStartedAt: now, Transitions: []Transition{{Status: StatusStarting, At: now}},
		}
		if err := validateSnapshot(configured, manual.ChangedRoutes); err != nil {
			return s.rollbackStartup(cfg, state, manual, nil, err)
		}
		state.Pending = &manual
		change = state.Pending
	}
	now := s.now()
	if !change.LastAttemptAt.IsZero() && now.Sub(change.LastAttemptAt) > 5*time.Minute {
		change.RestartAttempts = 0
	}
	if change.RestartAttempts >= 3 && !change.LastAttemptAt.IsZero() && now.Sub(change.LastAttemptAt) <= 5*time.Minute {
		change.Failure = "gateway startup failed three times within five minutes"
		change.FailureClass = FailureInfrastructure
		s.transition(change, StatusRecoveryRequired, now)
		state.Generation++
		if err := s.save(state); err != nil {
			return Status{}, false, err
		}
		return s.InspectWithState(cfg, state), false, nil
	}
	change.RestartAttempts++
	change.LastAttemptAt = now
	s.transition(change, StatusStarting, now)
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, false, err
	}
	candidateCfg, err := configWithChange(s.ConfigPath, change.Candidate, change.ProviderChanges, true)
	if err != nil {
		return Status{}, false, err
	}
	if err := s.overlayCredentialStage(change.CredentialStage, candidateCfg); err != nil {
		return Status{}, false, err
	}
	probes := s.validate(ctx, candidateCfg, change.ChangedRoutes)
	if failureClass, probeErr := failedProbes(probes); probeErr != nil {
		if (state.RunningVerifiedAt.IsZero() && change.Source == "initial-startup") || change.Source == "manual-provider-config" {
			// With no known-good baseline there is nothing safe to roll back to.
			// Preserve the failed evidence, keep readiness false, and allow the
			// daemon's mock provider to serve the sole Model Manager repair UI.
			if routeProbePassed(probes, RoutePrimary) {
				state.Running.Primary = change.Candidate.Primary
				state.ForegroundVerifiedAt = s.now()
				state.RunningVerifiedAt = state.ForegroundVerifiedAt
				state.RunningProviderFingerprint = providerConfigFingerprint(candidateCfg)
				state.BackgroundVerifiedAt = time.Time{}
			}
			recordRouteProbeEvidence(&state, probes, s.now())
			s.finishPendingClass(&state, StatusFailed, failureClass, probeErr.Error(), probes)
			if err := s.save(state); err != nil {
				return Status{}, false, err
			}
			return s.InspectWithState(cfg, state), false, nil
		}
		if failureClass == FailureInfrastructure || state.RunningVerifiedAt.IsZero() {
			change.Probes = probes
			change.Failure = probeErr.Error()
			change.FailureClass = failureClass
			s.transition(change, StatusRecoveryRequired, s.now())
			state.Generation++
			if err := s.save(state); err != nil {
				return Status{}, false, err
			}
			return s.InspectWithState(cfg, state), false, nil
		}
		return s.rollbackStartup(cfg, state, *change, probes, probeErr)
	}
	change.Probes = probes
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, false, err
	}
	return s.InspectWithState(candidateCfg, state), false, nil
}

// MarkStartupHealthy commits a startup-validated candidate only after the
// daemon has built all runtime dependencies and bound its listener.
func (s *Service) MarkStartupHealthy() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	if state.Pending == nil || state.Pending.Status != StatusStarting {
		if SnapshotFromConfig(cfg) != state.Running {
			return s.InspectWithState(cfg, state), nil
		}
		return s.InspectWithState(cfg, state), nil
	}
	if SnapshotFromConfig(cfg) != state.Pending.Candidate || !ProviderChangesMatch(cfg, state.Pending.ProviderChanges, true) {
		return Status{}, fmt.Errorf("%w: startup config no longer matches candidate %s", ErrGenerationConflict, state.Pending.ID)
	}
	if err := s.finalizeCredentialStage(state.Pending.CredentialStage); err != nil {
		return Status{}, fmt.Errorf("finalize provider credentials: %w", err)
	}
	finished := *state.Pending
	s.transition(&finished, StatusApplied, s.now())
	finished.FinishedAt = s.now()
	state.Running = finished.Candidate
	state.RunningProviderFingerprint = providerConfigFingerprint(cfg)
	state.ForegroundVerifiedAt = s.now()
	if auxiliaryEnabled(finished.Candidate) {
		state.BackgroundVerifiedAt = s.now()
		markBackgroundRoutesReady(&state, state.BackgroundVerifiedAt)
	} else {
		state.BackgroundVerifiedAt = time.Time{}
		state.RouteReadiness = nil
	}
	state.RunningVerifiedAt = state.ForegroundVerifiedAt
	state.Pending = nil
	state.History = appendBounded(state.History, finished, s.historyMax())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

// RecordRouteProbe persists one runtime health transition without opening a
// model-change transaction. It is used by bounded role recovery monitors; it
// never rewrites routing or credentials.
func (s *Service) RecordRouteProbe(result ProbeResult) (Status, error) {
	if result.Route == RoutePrimary || (result.Route != RouteAuxiliary && !IsManagedRoleRoute(result.Route)) {
		return Status{}, fmt.Errorf("unsupported background route %q", result.Route)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	if SnapshotFromConfig(cfg) != state.Running || providerConfigFingerprint(cfg) != state.RunningProviderFingerprint {
		return s.InspectWithState(cfg, state), fmt.Errorf("model route changed before runtime probe evidence could be recorded")
	}
	recordRouteProbeEvidence(&state, []ProbeResult{result}, s.now())
	if allBackgroundEvidenceReady(state) {
		state.BackgroundVerifiedAt = s.now()
	} else {
		state.BackgroundVerifiedAt = time.Time{}
	}
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, err
	}
	return s.InspectWithState(cfg, state), nil
}

func recordRouteProbeEvidence(state *State, probes []ProbeResult, now time.Time) {
	if state == nil {
		return
	}
	if state.RouteReadiness == nil {
		state.RouteReadiness = make(map[Route]RouteReadiness)
	}
	for _, probe := range probes {
		if probe.Route == RoutePrimary || (probe.Route != RouteAuxiliary && !IsManagedRoleRoute(probe.Route)) {
			continue
		}
		evidence := RouteReadiness{Ready: probe.OK, FailureClass: probe.FailureClass}
		if probe.OK {
			evidence.VerifiedAt = now.UTC()
		} else {
			evidence.Reason = strings.TrimSpace(probe.Error)
			if evidence.Reason == "" {
				evidence.Reason = "route probe failed"
			}
		}
		state.RouteReadiness[probe.Route] = evidence
	}
}

func markBackgroundRoutesReady(state *State, verifiedAt time.Time) {
	if state == nil {
		return
	}
	if state.RouteReadiness == nil {
		state.RouteReadiness = make(map[Route]RouteReadiness)
	}
	for _, route := range backgroundRoutes() {
		state.RouteReadiness[route] = RouteReadiness{Ready: true, VerifiedAt: verifiedAt.UTC()}
	}
}

func allBackgroundEvidenceReady(state State) bool {
	for _, route := range backgroundRoutes() {
		if evidence, ok := state.RouteReadiness[route]; !ok || !evidence.Ready {
			return false
		}
	}
	return true
}

// AcceptMigrationReadiness records narrowly scoped migration evidence from a
// matching legacy onboarding receipt. Normal daemon startup must use
// ReconcileStartup followed by MarkStartupHealthy instead.
func (s *Service) AcceptMigrationReadiness() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, err
	}
	if state.Pending != nil || SnapshotFromConfig(cfg) != state.Running {
		return s.InspectWithState(cfg, state), fmt.Errorf("model readiness migration evidence does not match the configured running snapshot")
	}
	if state.RunningVerifiedAt.IsZero() {
		state.RunningVerifiedAt = s.now()
		state.RunningProviderFingerprint = providerConfigFingerprint(cfg)
		state.ForegroundVerifiedAt = state.RunningVerifiedAt
		if auxiliaryEnabled(state.Running) {
			state.BackgroundVerifiedAt = state.RunningVerifiedAt
			markBackgroundRoutesReady(&state, state.BackgroundVerifiedAt)
		}
		state.Generation++
		if err := s.save(state); err != nil {
			return Status{}, err
		}
	}
	return s.InspectWithState(cfg, state), nil
}

// FailStarting restores the last running snapshot when daemon construction or
// listener binding fails after the post-restart contract probe passed.
func (s *Service) FailStarting(cause error) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockState()
	if err != nil {
		return Status{}, false, err
	}
	defer unlock()
	if cause == nil {
		cause = fmt.Errorf("daemon startup failed")
	}
	cfg, err := config.LoadConfig(config.Options{Path: s.ConfigPath})
	if err != nil {
		return Status{}, false, err
	}
	state, err := s.loadOrInitialize(cfg)
	if err != nil {
		return Status{}, false, err
	}
	if state.Pending == nil || state.Pending.Status != StatusStarting {
		return s.InspectWithState(cfg, state), false, nil
	}
	state.Pending.Failure = cause.Error()
	state.Pending.FailureClass = FailureInfrastructure
	s.transition(state.Pending, StatusRecoveryRequired, s.now())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, false, err
	}
	return s.InspectWithState(cfg, state), false, nil
}

func (s *Service) rollbackStartup(cfg *config.Config, state State, change Change, probes []ProbeResult, cause error) (Status, bool, error) {
	ApplySnapshot(cfg, change.Previous)
	ApplyProviderChanges(cfg, change.ProviderChanges, false)
	if err := config.SaveConfig(s.ConfigPath, cfg); err != nil {
		return Status{}, false, fmt.Errorf("model startup validation failed (%v), then rollback write failed: %w", cause, err)
	}
	if err := s.rollbackCredentialStage(change.CredentialStage); err != nil {
		change.Failure = fmt.Sprintf("model startup validation failed (%v), then credential rollback failed: %v", cause, err)
		change.FailureClass = FailureInfrastructure
		s.transition(&change, StatusRecoveryRequired, s.now())
		state.Pending = &change
		state.Generation++
		if saveErr := s.save(state); saveErr != nil {
			return Status{}, false, saveErr
		}
		return s.InspectWithState(cfg, state), false, fmt.Errorf("%w: %s", ErrRecoveryRequired, change.Failure)
	}
	s.transition(&change, StatusRolledBack, s.now())
	change.Failure = cause.Error()
	change.FailureClass = FailureModel
	change.Probes = probes
	change.FinishedAt = s.now()
	state.Pending = nil
	state.History = appendBounded(state.History, change, s.historyMax())
	state.Generation++
	if err := s.save(state); err != nil {
		return Status{}, false, err
	}
	return s.InspectWithState(cfg, state), true, nil
}

func (s *Service) InspectWithState(cfg *config.Config, state State) Status {
	return Status{
		Generation: state.Generation, Running: state.Running, RunningVerifiedAt: state.RunningVerifiedAt,
		Configured: logicalConfigured(cfg, state.Pending), Pending: cloneChange(state.Pending),
		History:   append([]Change(nil), state.History...),
		Readiness: readinessFor(cfg, state),
	}
}

func (s *Service) validate(ctx context.Context, cfg *config.Config, routes []Route) []ProbeResult {
	if s.Validate == nil {
		results := make([]ProbeResult, 0, len(routes))
		for _, route := range routes {
			results = append(results, ProbeResult{
				Route: route, Error: "live model validator is unavailable", FailureClass: FailureInfrastructure,
			})
		}
		return results
	}
	return s.Validate(ctx, cfg, append([]Route(nil), routes...))
}

func (s *Service) overlayCredentialStage(stageID string, cfg *config.Config) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if s.Credentials == nil {
		return fmt.Errorf("provider credential transaction support is unavailable")
	}
	return s.Credentials.OverlayStage(stageID, cfg)
}

func (s *Service) commitCredentialStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if s.Credentials == nil {
		return fmt.Errorf("provider credential transaction support is unavailable")
	}
	return s.Credentials.CommitStage(stageID)
}

func (s *Service) rollbackCredentialStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if s.Credentials == nil {
		return fmt.Errorf("provider credential transaction support is unavailable")
	}
	return s.Credentials.RollbackStage(stageID)
}

func (s *Service) discardCredentialStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if s.Credentials == nil {
		return fmt.Errorf("provider credential transaction support is unavailable")
	}
	return s.Credentials.DiscardStage(stageID)
}

func (s *Service) finalizeCredentialStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if s.Credentials == nil {
		return fmt.Errorf("provider credential transaction support is unavailable")
	}
	return s.Credentials.FinalizeStage(stageID)
}

func (s *Service) cancelPending(cfg *config.Config, state *State, status ChangeStatus, reason string) error {
	if state == nil || state.Pending == nil {
		return fmt.Errorf("there is no pending model change")
	}
	pending := *state.Pending
	if pending.Status != StatusAwaitingConfirmation && pending.Status != StatusValidating && pending.Status != StatusAwaitingSafeBoundary {
		return fmt.Errorf("model change %s is %s and can no longer be cancelled or replaced", pending.ID, pending.Status)
	}
	if SnapshotFromConfig(cfg) == pending.Candidate && ProviderChangesMatch(cfg, pending.ProviderChanges, true) {
		ApplySnapshot(cfg, pending.Previous)
		ApplyProviderChanges(cfg, pending.ProviderChanges, false)
		if err := config.SaveConfig(s.ConfigPath, cfg); err != nil {
			return err
		}
	}
	if err := s.rollbackCredentialStage(pending.CredentialStage); err != nil {
		return fmt.Errorf("cancel provider credential stage: %w", err)
	}
	s.transition(&pending, status, s.now())
	pending.Failure = strings.TrimSpace(reason)
	pending.FinishedAt = s.now()
	state.Pending = nil
	state.History = appendBounded(state.History, pending, s.historyMax())
	state.Generation++
	return s.save(*state)
}

func (s *Service) finishPending(state *State, status ChangeStatus, failure string, probes []ProbeResult) {
	s.finishPendingClass(state, status, "", failure, probes)
}

func (s *Service) finishPendingClass(state *State, status ChangeStatus, class FailureClass, failure string, probes []ProbeResult) {
	if state == nil || state.Pending == nil {
		return
	}
	if err := s.rollbackCredentialStage(state.Pending.CredentialStage); err != nil {
		state.Pending.Failure = "restore provider credentials: " + err.Error()
		state.Pending.FailureClass = FailureInfrastructure
		s.transition(state.Pending, StatusRecoveryRequired, s.now())
		state.Generation++
		return
	}
	finished := *state.Pending
	s.transition(&finished, status, s.now())
	finished.Failure = strings.TrimSpace(failure)
	finished.FailureClass = class
	finished.Probes = append([]ProbeResult(nil), probes...)
	finished.FinishedAt = s.now()
	state.Pending = nil
	state.History = appendBounded(state.History, finished, s.historyMax())
	state.Generation++
}

func (s *Service) expireConfirmation(state *State) bool {
	if state == nil || state.Pending == nil || state.Pending.Status != StatusAwaitingConfirmation || state.Pending.ConfirmBy.IsZero() || !s.now().After(state.Pending.ConfirmBy) {
		return false
	}
	s.finishPending(state, StatusFailed, "confirmation expired", state.Pending.Probes)
	return true
}

func (s *Service) loadOrInitialize(cfg *config.Config) (State, error) {
	state, err := s.load()
	if err == nil {
		if state.SchemaVersion >= 1 && state.SchemaVersion < stateSchemaVersion {
			originalVersion := state.SchemaVersion
			migrated := migrateState(state)
			if migrated.RunningProviderFingerprint == "" && (!migrated.RunningVerifiedAt.IsZero() || !migrated.ForegroundVerifiedAt.IsZero()) {
				migrated.RunningProviderFingerprint = providerConfigFingerprint(cfg)
			}
			if backupErr := s.backupSchema(originalVersion); backupErr != nil {
				return State{}, backupErr
			}
			if saveErr := s.save(migrated); saveErr != nil {
				return State{}, saveErr
			}
			return migrated, nil
		}
		if state.SchemaVersion != stateSchemaVersion {
			return State{}, fmt.Errorf("unsupported model state schema %d", state.SchemaVersion)
		}
		if state.RunningProviderFingerprint == "" && (!state.RunningVerifiedAt.IsZero() || !state.ForegroundVerifiedAt.IsZero()) {
			state.RunningProviderFingerprint = providerConfigFingerprint(cfg)
			if saveErr := s.save(state); saveErr != nil {
				return State{}, saveErr
			}
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	now := s.now()
	state = State{SchemaVersion: stateSchemaVersion, Generation: 1, Running: SnapshotFromConfig(cfg), UpdatedAt: now}
	if err := s.save(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Service) load() (State, error) {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode model state: %w", err)
	}
	state.Running = normalizeSnapshot(state.Running)
	if state.Pending != nil {
		state.Pending.Previous = normalizeSnapshot(state.Pending.Previous)
		state.Pending.Candidate = normalizeSnapshot(state.Pending.Candidate)
	}
	for index := range state.History {
		state.History[index].Previous = normalizeSnapshot(state.History[index].Previous)
		state.History[index].Candidate = normalizeSnapshot(state.History[index].Candidate)
	}
	return state, nil
}

func (s *Service) save(state State) error {
	state.SchemaVersion = stateSchemaVersion
	state.UpdatedAt = s.now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := s.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".model-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Service) statePath() string {
	configPath, _ := config.ResolveConfigPath(s.ConfigPath)
	return filepath.Join(filepath.Dir(configPath), "model-state.json")
}

// StatePath is exposed for read-only diagnostics and installed-client
// observation. Callers must use Service methods for every mutation.
func (s *Service) StatePath() string { return s.statePath() }

func (s *Service) lockState() (func(), error) {
	path := s.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return lockStateFile(path)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) confirmTTL() time.Duration {
	if s.ConfirmTTL > 0 {
		return s.ConfirmTTL
	}
	return defaultConfirmTTL
}

func (s *Service) historyMax() int {
	if s.HistoryMax > 0 {
		return s.HistoryMax
	}
	return defaultHistorySize
}

func configWithSnapshot(path string, snapshot Snapshot) (*config.Config, error) {
	return configWithChange(path, snapshot, nil, true)
}

func configWithChange(path string, snapshot Snapshot, providers []ProviderChange, candidate bool) (*config.Config, error) {
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		return nil, err
	}
	ApplySnapshot(cfg, snapshot)
	ApplyProviderChanges(cfg, providers, candidate)
	return cfg, nil
}

func mergeRoutes(existing []Route, additions []Route) []Route {
	seen := make(map[Route]struct{}, len(existing)+len(additions))
	result := make([]Route, 0, len(existing)+len(additions))
	for _, route := range append(append([]Route(nil), existing...), additions...) {
		if _, ok := seen[route]; ok {
			continue
		}
		seen[route] = struct{}{}
		result = append(result, route)
	}
	return result
}

func routesUsingProvider(snapshot Snapshot, provider string) []Route {
	provider = normalizeProviderConnectionID(provider)
	if provider == "" {
		return nil
	}
	routes := []Route{RoutePrimary, RouteAuxiliary}
	routes = append(routes, ManagedRoleRoutes()...)
	result := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route != RoutePrimary && !auxiliaryEnabled(snapshot) {
			continue
		}
		selection := selectionForRoute(snapshot, route)
		if IsManagedRoleRoute(route) && strings.TrimSpace(selection.Provider) == "" {
			selection = snapshot.Auxiliary
		}
		if normalizeProviderConnectionID(selection.Provider) == provider {
			result = append(result, route)
		}
	}
	return result
}

func validateSnapshot(snapshot Snapshot, changed []Route) error {
	for _, route := range changed {
		if route != RoutePrimary && !auxiliaryEnabled(snapshot) {
			continue
		}
		selection := selectionForRoute(snapshot, route)
		if strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
			if IsManagedRoleRoute(route) && selection == (config.ModelSelectionConfig{}) {
				// An empty role selection means "inherit models.auxiliary".
				continue
			}
			return fmt.Errorf("%s provider and model are required", route)
		}
	}
	return nil
}

func failedProbes(results []ProbeResult) (FailureClass, error) {
	if len(results) == 0 {
		return FailureInfrastructure, fmt.Errorf("model validation returned no evidence")
	}
	var failures []string
	failureClass := FailureModel
	for _, result := range results {
		if result.OK {
			continue
		}
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "probe failed"
		}
		if result.FailureClass == FailureInfrastructure {
			// Uncertainty wins: a mixed model/infrastructure failure must never
			// auto-rollback and incorrectly blame the candidate route.
			failureClass = FailureInfrastructure
		}
		failures = append(failures, fmt.Sprintf("%s: %s", result.Route, message))
	}
	if len(failures) > 0 {
		return failureClass, fmt.Errorf("model validation failed: %s", strings.Join(failures, "; "))
	}
	return "", nil
}

func routeProbePassed(results []ProbeResult, route Route) bool {
	for _, result := range results {
		if result.Route == route {
			return result.OK
		}
	}
	return false
}

func readinessFor(cfg *config.Config, state State) Readiness {
	configured := normalizeSnapshot(logicalConfigured(cfg, state.Pending))
	running := normalizeSnapshot(state.Running)
	providerConfigMatches := providerConfigFingerprint(cfg) == state.RunningProviderFingerprint
	foregroundVerifiedAt := state.ForegroundVerifiedAt
	if foregroundVerifiedAt.IsZero() {
		foregroundVerifiedAt = state.RunningVerifiedAt
	}
	readiness := Readiness{BackgroundEnabled: auxiliaryEnabled(configured), Routes: make(map[Route]RouteReadiness)}
	readiness.Foreground = !foregroundVerifiedAt.IsZero() && configured.Primary == running.Primary && providerConfigMatches
	if pendingChangesRoute(state.Pending, RoutePrimary) {
		readiness.Foreground = false
		readiness.ForegroundReason = "a main model change is pending"
	}
	if !readiness.Foreground {
		if readiness.ForegroundReason != "" {
			// Preserve the more actionable pending-transaction reason.
		} else if foregroundVerifiedAt.IsZero() {
			readiness.ForegroundReason = "main model has not crossed a verified startup boundary"
		} else if !providerConfigMatches {
			readiness.ForegroundReason = "provider connections differ from the verified running configuration"
		} else {
			readiness.ForegroundReason = "configured main model differs from the verified running model"
		}
	}
	if !readiness.BackgroundEnabled {
		readiness.Background = false
		if pendingChangesBackground(state.Pending) {
			readiness.BackgroundReason = "a background model change is pending"
		}
		return readiness
	}
	readiness.Background = true
	for _, route := range backgroundRoutes() {
		routeState := routeReadinessFor(configured, running, state, route, providerConfigMatches)
		readiness.Routes[route] = routeState
		if routeState.Ready {
			continue
		}
		readiness.Background = false
		if readiness.BackgroundReason == "" {
			reason := strings.TrimSpace(routeState.Reason)
			if reason == "" {
				reason = "not ready"
			}
			readiness.BackgroundReason = string(route) + ": " + reason
		}
	}
	readiness.Degraded = readiness.Foreground && !readiness.Background
	return readiness
}

func backgroundRoutes() []Route {
	return append([]Route{RouteAuxiliary}, ManagedRoleRoutes()...)
}

func routeReadinessFor(configured, running Snapshot, state State, route Route, providerConfigMatches bool) RouteReadiness {
	evidence, ok := state.RouteReadiness[route]
	if !ok {
		verifiedAt := state.BackgroundVerifiedAt
		if verifiedAt.IsZero() && state.ForegroundVerifiedAt.IsZero() {
			verifiedAt = state.RunningVerifiedAt
		}
		if !verifiedAt.IsZero() {
			evidence = RouteReadiness{Ready: true, VerifiedAt: verifiedAt}
		}
	}
	if pendingChangesRoute(state.Pending, route) ||
		(route != RouteAuxiliary && pendingChangesRoute(state.Pending, RouteAuxiliary)) {
		evidence.Ready = false
		evidence.Reason = "a background model change is pending"
		return evidence
	}
	if !providerConfigMatches {
		evidence.Ready = false
		evidence.Reason = "provider connections differ from the verified running configuration"
		return evidence
	}
	if selectionForRoute(configured, route) != selectionForRoute(running, route) {
		evidence.Ready = false
		evidence.Reason = "configured route differs from the verified running route"
		return evidence
	}
	if !evidence.Ready && strings.TrimSpace(evidence.Reason) == "" {
		evidence.Reason = "route has not crossed a verified startup boundary"
	}
	return evidence
}

func pendingChangesRoute(change *Change, route Route) bool {
	if change == nil {
		return false
	}
	for _, changed := range change.ChangedRoutes {
		if changed == route {
			return true
		}
	}
	return false
}

func pendingChangesBackground(change *Change) bool {
	if change == nil {
		return false
	}
	for _, route := range change.ChangedRoutes {
		if route != RoutePrimary {
			return true
		}
	}
	return false
}

func backgroundSelectionsEqual(left, right Snapshot) bool {
	left = normalizeSnapshot(left)
	right = normalizeSnapshot(right)
	if left.Auxiliary != right.Auxiliary {
		return false
	}
	for _, route := range ManagedRoleRoutes() {
		if selectionForRoute(left, route) != selectionForRoute(right, route) {
			return false
		}
	}
	return true
}

func normalizeSnapshot(snapshot Snapshot) Snapshot {
	normalize := func(selection config.ModelSelectionConfig) config.ModelSelectionConfig {
		selection.Provider = strings.TrimSpace(selection.Provider)
		selection.Model = strings.TrimSpace(selection.Model)
		selection.Reasoning = normalizeAuto(selection.Reasoning)
		selection.ServiceTier = normalizeAuto(selection.ServiceTier)
		return selection
	}
	snapshot.Primary = normalize(snapshot.Primary)
	snapshot.Primary.Enabled = nil
	snapshot.Auxiliary = normalize(snapshot.Auxiliary)
	if snapshot.Auxiliary.Enabled != nil {
		if *snapshot.Auxiliary.Enabled {
			snapshot.Auxiliary.Enabled = nil
		} else {
			snapshot.Auxiliary.Enabled = &disabledAuxiliary
		}
	}
	for _, route := range ManagedRoleRoutes() {
		selection := normalize(selectionForRoute(snapshot, route))
		selection.Enabled = nil
		setSelectionForRoute(&snapshot, route, selection)
	}
	return snapshot
}

func auxiliaryEnabled(snapshot Snapshot) bool {
	snapshot = normalizeSnapshot(snapshot)
	return snapshot.Auxiliary.Enabled == nil || *snapshot.Auxiliary.Enabled
}

// ManagedRoleRoutes returns the stable background roles exposed by the model
// manager. Callers may render or validate these roles without maintaining a
// second catalog.
func ManagedRoleRoutes() []Route {
	return []Route{
		RouteFastClassifier,
		RouteMemoryExtract,
		RouteBackgroundReview,
		RouteSkillCurator,
		RouteSemanticRecall,
		RouteSummarizer,
	}
}

func IsManagedRoleRoute(route Route) bool {
	for _, candidate := range ManagedRoleRoutes() {
		if route == candidate {
			return true
		}
	}
	return false
}

func selectionForRoute(snapshot Snapshot, route Route) config.ModelSelectionConfig {
	switch route {
	case RoutePrimary:
		return snapshot.Primary
	case RouteAuxiliary:
		return snapshot.Auxiliary
	case RouteFastClassifier:
		return snapshot.Roles.FastClassifier
	case RouteMemoryExtract:
		return snapshot.Roles.MemoryExtract
	case RouteBackgroundReview:
		return snapshot.Roles.BackgroundReview
	case RouteSkillCurator:
		return snapshot.Roles.SkillCurator
	case RouteSemanticRecall:
		return snapshot.Roles.SemanticRecall
	case RouteSummarizer:
		return snapshot.Roles.Summarizer
	default:
		return config.ModelSelectionConfig{}
	}
}

func setSelectionForRoute(snapshot *Snapshot, route Route, selection config.ModelSelectionConfig) {
	if snapshot == nil {
		return
	}
	switch route {
	case RoutePrimary:
		snapshot.Primary = selection
	case RouteAuxiliary:
		snapshot.Auxiliary = selection
	case RouteFastClassifier:
		snapshot.Roles.FastClassifier = selection
	case RouteMemoryExtract:
		snapshot.Roles.MemoryExtract = selection
	case RouteBackgroundReview:
		snapshot.Roles.BackgroundReview = selection
	case RouteSkillCurator:
		snapshot.Roles.SkillCurator = selection
	case RouteSemanticRecall:
		snapshot.Roles.SemanticRecall = selection
	case RouteSummarizer:
		snapshot.Roles.Summarizer = selection
	}
}

func roleSelection(role config.ModelRoleConfig) config.ModelSelectionConfig {
	return config.ModelSelectionConfig{
		Provider: role.Provider, Model: role.Model, Reasoning: role.EffectiveReasoning(),
		ServiceTier: role.ServiceTier, ContextLength: role.ContextLength,
	}
}

func applyRoleSelection(roles map[string]config.ModelRoleConfig, route Route, selection config.ModelSelectionConfig) {
	name := string(route)
	role := roles[name]
	role.Provider = selection.Provider
	role.Model = selection.Model
	role.Reasoning = selection.Reasoning
	role.ReasoningEffort = ""
	role.ServiceTier = selection.ServiceTier
	role.ContextLength = selection.ContextLength
	if roleSelection(role) == (config.ModelSelectionConfig{}) && roleAdvancedConfigEmpty(role) {
		delete(roles, name)
		return
	}
	roles[name] = role
}

func roleAdvancedConfigEmpty(role config.ModelRoleConfig) bool {
	return strings.TrimSpace(role.BaseURL) == "" && strings.TrimSpace(role.Protocol) == "" && strings.TrimSpace(role.APIKey) == "" &&
		role.MaxTokens <= 0 && len(role.ExtraHeaders) == 0 && len(role.ExtraBody) == 0 && len(role.ExtraQuery) == 0 &&
		len(role.Headers) == 0 && len(role.Thinking) == 0 && role.Quirks == (config.ProviderQuirks{})
}

func normalizeAuto(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		return ""
	}
	return value
}

func normalizeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "unknown"
	}
	if len(source) > 80 {
		return source[:80]
	}
	return source
}

func logicalConfigured(cfg *config.Config, pending *Change) Snapshot {
	if pending == nil {
		return SnapshotFromConfig(cfg)
	}
	switch pending.Status {
	case StatusValidating, StatusAwaitingSafeBoundary, StatusCommitting, StatusDraining,
		StatusRestarting, StatusStarting, StatusRecoveryRequired:
		return pending.Candidate
	default:
		return SnapshotFromConfig(cfg)
	}
}

func pendingRestartStatus(status ChangeStatus) bool {
	switch status {
	case StatusAwaitingSafeBoundary, StatusCommitting, StatusDraining, StatusRestarting, StatusStarting, StatusRecoveryRequired:
		return true
	default:
		return false
	}
}

func pendingChange(state *State, id string) (*Change, error) {
	id = strings.TrimSpace(id)
	if state == nil || state.Pending == nil || (id != "" && !strings.EqualFold(id, state.Pending.ID)) {
		return nil, fmt.Errorf("pending model change %q was not found", id)
	}
	return state.Pending, nil
}

func findChange(history []Change, id string) (Change, bool) {
	id = strings.TrimSpace(id)
	for i := len(history) - 1; i >= 0; i-- {
		if strings.EqualFold(history[i].ID, id) {
			return history[i], true
		}
	}
	return Change{}, false
}

func (s *Service) transition(change *Change, status ChangeStatus, at time.Time) {
	if change == nil {
		return
	}
	at = at.UTC()
	if change.Status == status && !change.PhaseStartedAt.IsZero() {
		return
	}
	change.Status = status
	change.PhaseStartedAt = at
	change.Transitions = append(change.Transitions, Transition{Status: status, At: at})
}

func migrateState(state State) State {
	previousVersion := state.SchemaVersion
	state.SchemaVersion = stateSchemaVersion
	if previousVersion == 1 && state.Pending != nil {
		switch state.Pending.Status {
		case ChangeStatus("awaiting_restart"):
			state.Pending.Status = StatusAwaitingSafeBoundary
		case StatusCommitting:
			// A v1 committing record may have written the candidate already. Boot
			// reconciliation compares config.yaml and completes deterministically.
		case StatusFailed:
			state.Pending.FailureClass = FailureModel
		}
		if state.Pending.PhaseStartedAt.IsZero() {
			state.Pending.PhaseStartedAt = firstNonZeroTime(state.Pending.ConfirmedAt, state.Pending.CreatedAt, state.UpdatedAt)
		}
		if len(state.Pending.Transitions) == 0 {
			state.Pending.Transitions = []Transition{{Status: state.Pending.Status, At: state.Pending.PhaseStartedAt}}
		}
	}
	for i := range state.History {
		if state.History[i].PhaseStartedAt.IsZero() {
			state.History[i].PhaseStartedAt = firstNonZeroTime(state.History[i].FinishedAt, state.History[i].ConfirmedAt, state.History[i].CreatedAt)
		}
		if len(state.History[i].Transitions) == 0 {
			state.History[i].Transitions = []Transition{{Status: state.History[i].Status, At: state.History[i].PhaseStartedAt}}
		}
	}
	if state.RunningVerifiedAt.IsZero() {
		for i := len(state.History) - 1; i >= 0; i-- {
			change := state.History[i]
			if change.Status == StatusApplied && change.Candidate == state.Running {
				state.RunningVerifiedAt = firstNonZeroTime(change.FinishedAt, change.PhaseStartedAt, change.ConfirmedAt)
				break
			}
		}
	}
	if !state.RunningVerifiedAt.IsZero() {
		if state.ForegroundVerifiedAt.IsZero() {
			state.ForegroundVerifiedAt = state.RunningVerifiedAt
		}
		if state.BackgroundVerifiedAt.IsZero() && auxiliaryEnabled(state.Running) {
			state.BackgroundVerifiedAt = state.RunningVerifiedAt
		}
		if auxiliaryEnabled(state.Running) && len(state.RouteReadiness) == 0 {
			markBackgroundRoutesReady(&state, state.BackgroundVerifiedAt)
		}
	}
	return state
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value.UTC()
		}
	}
	return time.Now().UTC()
}

func (s *Service) backupSchema(version int) error {
	path := s.statePath()
	backup := fmt.Sprintf("%s.v%d.backup", path, version)
	if _, err := os.Stat(backup); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(backup), ".model-state-backup-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, backup)
}

func appendBounded(history []Change, change Change, limit int) []Change {
	history = append(history, change)
	if len(history) > limit {
		history = append([]Change(nil), history[len(history)-limit:]...)
	}
	return history
}

func cloneChange(change *Change) *Change {
	if change == nil {
		return nil
	}
	copy := *change
	copy.ChangedRoutes = append([]Route(nil), change.ChangedRoutes...)
	copy.Probes = append([]ProbeResult(nil), change.Probes...)
	copy.Transitions = append([]Transition(nil), change.Transitions...)
	copy.ProviderChanges = make([]ProviderChange, len(change.ProviderChanges))
	for i, providerChange := range change.ProviderChanges {
		providerChange.Previous = cloneProviderConnection(providerChange.Previous)
		providerChange.Candidate = cloneProviderConnection(providerChange.Candidate)
		copy.ProviderChanges[i] = providerChange
	}
	return &copy
}

func cloneProviderConnection(connection ProviderConnection) ProviderConnection {
	connection.ExtraHeaders = cloneStringMap(connection.ExtraHeaders)
	connection.ExtraBody = cloneAnyMap(connection.ExtraBody)
	connection.ExtraQuery = cloneAnyMap(connection.ExtraQuery)
	connection.Thinking = cloneAnyMap(connection.Thinking)
	return connection
}

func newChangeID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err == nil {
		return "model_" + hex.EncodeToString(raw)
	}
	sum := sha256.Sum256([]byte(time.Now().UTC().String()))
	return "model_" + hex.EncodeToString(sum[:6])
}

func (c Change) BeforeSnapshot() Snapshot { return c.Previous }

func SortedRoutes(routes []Route) []Route {
	out := append([]Route(nil), routes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
