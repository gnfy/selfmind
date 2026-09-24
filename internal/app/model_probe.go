package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

const modelRoleProbeTimeout = 30 * time.Second

// modelProbeTimeout gives each contract the bound its real call runs under.
// Approval triage waits inside the person's turn; maintenance runs in the
// background at its route's reasoning level, which a thinking model can spend
// well past the plain probe's bound.
func modelProbeTimeout(contract string) time.Duration {
	switch contract {
	case modelProbeContractApproval:
		return config.DefaultApprovalTriageTimeout
	case modelProbeContractMaintenance:
		return config.DefaultTaskMaintenanceLLMTimeout
	default:
		return modelRoleProbeTimeout
	}
}

const modelProbeToolAttempts = 3

const (
	modelProbeContractPlain       = "foreground"
	modelProbeContractMaintenance = "maintenance_json"
	modelProbeContractApproval    = "approval_json"
)

// ModelRoleProbe reports one live request for one resolved runtime. Roles that
// share the same provider/model endpoint and request contract are intentionally
// grouped so doctor does not spend duplicate quota merely because several
// roles use the same Coding Plan profile.
type ModelRoleProbe struct {
	Roles                     []string
	Provider                  string
	Model                     string
	Latency                   time.Duration
	NativeToolsTested         bool
	ToolLoopTested            bool
	ToolLoopPassed            bool
	MaintenanceContractTested bool
	MaintenanceContractPassed bool
	ApprovalContractTested    bool
	ApprovalContractPassed    bool
	// ApprovalThinkingMode is the disabled-reasoning encoding the approval
	// probe proved for a route that reasons by default; ApprovalNotice states
	// what the probe observed. Both are empty when reasoning was already off.
	ApprovalThinkingMode string
	ApprovalNotice       string
	Err                  error
}

type roleProbeTarget struct {
	roles    []string
	runtime  modelruntime.Runtime
	provider llm.Provider
	contract string
}

// ResolveModelRuntime resolves primary, auxiliary, or one logical role through
// the same precedence used by the daemon.
func ResolveModelRuntime(ctx context.Context, cfg *config.Config, role string) (modelruntime.Runtime, error) {
	if cfg == nil {
		return modelruntime.Runtime{}, fmt.Errorf("config is required")
	}
	role = strings.TrimSpace(role)
	selection := modelruntime.Selection{}
	if role != "" && !strings.EqualFold(role, "primary") {
		if strings.EqualFold(role, "auxiliary") {
			if !cfg.AuxiliaryEnabled() {
				return modelruntime.Runtime{}, fmt.Errorf("background model work is disabled")
			}
			auxiliary := cfg.EffectiveAuxiliary()
			if strings.TrimSpace(auxiliary.Provider) == "" && strings.TrimSpace(auxiliary.Model) == "" {
				return modelruntime.Runtime{}, fmt.Errorf("auxiliary model is not configured")
			}
			selection = modelruntime.Selection{
				Provider: auxiliary.Provider, Model: auxiliary.Model,
				ContextLength: auxiliary.ContextLength, ReasoningEffort: auxiliary.Reasoning,
				ServiceTier: auxiliary.ServiceTier,
			}
		} else {
			var roleCfg config.ModelRoleConfig
			var ok bool
			if isAuxiliaryModelRole(llm.ModelRole(role)) {
				roleCfg, _, ok = cfg.ResolveAuxiliaryRole(role)
			} else {
				roleCfg, ok = cfg.Models.Roles[role]
				ok = ok && !roleConfigEmpty(roleCfg)
			}
			if !ok || roleConfigEmpty(roleCfg) {
				return modelruntime.Runtime{}, fmt.Errorf("model role %q is not configured", role)
			}
			selection = roleProviderSelection(llm.ModelRole(role), firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg)), roleCfg)
		}
	}
	return modelruntime.NewResolver(cfg).Resolve(ctx, selection)
}

// ProbeResolvedModel performs a bounded contract check against a resolved
// runtime. A foreground native-tool route must complete a tool call, accept
// the replayed assistant/tool pair, and then return final text.
func ProbeResolvedModel(ctx context.Context, rt modelruntime.Runtime) ModelRoleProbe {
	return ProbeResolvedModelForRole(ctx, rt, "")
}

// ProbeResolvedModelForRole validates the actual maintenance/approval contract
// for bounded roles and the complete native-tool loop for the foreground
// coding model.
func ProbeResolvedModelForRole(ctx context.Context, rt modelruntime.Runtime, role string) ModelRoleProbe {
	return probeResolvedModelForRole(ctx, rt, role, buildProviderFromRuntime(rt))
}

func probeResolvedModelForRole(ctx context.Context, rt modelruntime.Runtime, role string, provider llm.Provider) ModelRoleProbe {
	probe := ModelRoleProbe{Provider: rt.Provider, Model: rt.Model}
	start := time.Now()
	if provider == nil {
		probe.Err = fmt.Errorf("provider could not be built")
		probe.Latency = time.Since(start)
		return probe
	}
	contract := modelProbeContractForRole(role)
	probe.MaintenanceContractTested = contract == modelProbeContractMaintenance
	probe.ApprovalContractTested = contract == modelProbeContractApproval
	// A maintenance probe must mirror the real analyzer request, which never
	// carries agent tools. Combining an optional tool schema with the JSON
	// contract lets a provider legitimately return a tool-call-only response
	// and turns a healthy maintenance route into a false negative.
	probe.NativeToolsTested = llm.ProviderSupportsNativeTools(provider) && contract == modelProbeContractPlain
	probeCtx, cancel := context.WithTimeout(ctx, modelProbeTimeout(contract))
	defer cancel()
	if shouldProbeForegroundToolLoop(role, provider, contract) {
		probe.ToolLoopTested = true
		if err := probeNativeToolLoop(probeCtx, provider, rt); err != nil {
			probe.Err = fmt.Errorf("native tool loop failed: %w", err)
		} else {
			probe.ToolLoopPassed = true
		}
		probe.Latency = time.Since(start)
		return probe
	}
	resp, err := chatProbe(probeCtx, provider, modelProbeRequest(rt, probe.NativeToolsTested, contract))
	switch {
	case err != nil:
		probe.Err = err
	case modelProbeContentError(resp) != nil:
		probe.Err = modelProbeContentError(resp)
	case probe.MaintenanceContractTested:
		if maintenanceFinishReasonTruncated(resp.FinishReason) {
			probe.Err = fmt.Errorf("maintenance contract was truncated (finish_reason=%s)", resp.FinishReason)
		} else if _, err := decodePostRunAnalysis(resp.Content); err != nil {
			probe.Err = fmt.Errorf("maintenance contract failed: %w", err)
		} else {
			probe.MaintenanceContractPassed = true
		}
	case probe.ApprovalContractTested:
		if maintenanceFinishReasonTruncated(resp.FinishReason) {
			probe.Err = fmt.Errorf("approval contract was truncated (finish_reason=%s)", resp.FinishReason)
		} else if err := tools.ValidateStructuredApprovalReply(resp.Content); err != nil {
			probe.Err = fmt.Errorf("approval contract failed: %w", err)
		} else {
			probe.ApprovalContractPassed = true
			probe.ApprovalThinkingMode, probe.ApprovalNotice = probeApprovalReasoningOff(ctx, rt, resp, time.Since(start))
		}
	}
	probe.Latency = time.Since(start)
	return probe
}

// probeApprovalReasoningOff checks that the approval request really turned
// reasoning off, and finds the encoding that does when it did not. Triage asks
// for the lowest-latency tier; under the default OpenAI-compatible encoding,
// "off" is an omitted parameter, which a model that reasons by default
// ignores. The same request is then repeated once with a literal
// reasoning_effort "none", and that encoding is reported only when the answer
// still passes the contract and carries no reasoning. The observation is the
// evidence: no endpoint or model name selects the encoding. Whether it may be
// written is decided against the provider's configuration, which alone knows
// if a person declared a thinking_mode.
func probeApprovalReasoningOff(ctx context.Context, rt modelruntime.Runtime, first *llm.ChatResponse, firstLatency time.Duration) (string, string) {
	if !approvalResponseReasoned(first) || modelruntime.LowestLatencyReasoning(rt) != "none" {
		return "", ""
	}
	observed := fmt.Sprintf("The approval model %s/%s kept reasoning when asked not to (%d reasoning tokens, %.1fs)",
		rt.Provider, rt.Model, first.Usage.ReasoningOutputTokens, firstLatency.Seconds())
	stillReasoning := observed + ", so smart approvals may be slow or time out; a fast_classifier model under Role overrides that can turn reasoning off avoids it."
	protocol := modelruntime.NormalizeProtocol(rt.Protocol)
	mode := strings.ToLower(strings.TrimSpace(rt.Quirks.ThinkingMode))
	if (protocol != modelruntime.ProtocolOpenAIChat && protocol != modelruntime.ProtocolOpenAICompatible) ||
		(mode != "" && mode != modelruntime.ThinkingModeOpenAI) {
		return "", stillReasoning
	}
	candidate := rt
	candidate.Quirks.ThinkingMode = modelruntime.ThinkingModeEffortNone
	retryCtx, cancel := context.WithTimeout(ctx, modelProbeTimeout(modelProbeContractApproval))
	defer cancel()
	started := time.Now()
	resp, err := chatProbe(retryCtx, buildProviderFromRuntime(candidate), modelProbeRequest(candidate, false, modelProbeContractApproval))
	if err != nil || modelProbeContentError(resp) != nil || maintenanceFinishReasonTruncated(resp.FinishReason) ||
		tools.ValidateStructuredApprovalReply(resp.Content) != nil || approvalResponseReasoned(resp) {
		return "", stillReasoning
	}
	return modelruntime.ThinkingModeEffortNone, fmt.Sprintf("%s; reasoning_effort \"none\" turned it off (%.1fs).", observed, time.Since(started).Seconds())
}

// probeRetryPause separates a probe from its single retry after a transient
// provider failure.
const probeRetryPause = time.Second

// chatProbe sends one probe request and retries it once, after a short pause,
// when the provider reports a transient failure: a 5xx, a rate limit, or a
// dropped connection. One flake used to fail a whole model change, or park it
// for manual recovery, although the same request succeeded moments later.
// Deterministic failures and an expired probe budget are returned at once.
func chatProbe(ctx context.Context, provider llm.Provider, req llm.ChatRequest) (*llm.ChatResponse, error) {
	resp, err := provider.Chat(ctx, req)
	if err == nil || ctx.Err() != nil || !llm.IsRetryableError(err) {
		return resp, err
	}
	pause := time.NewTimer(probeRetryPause)
	defer pause.Stop()
	select {
	case <-ctx.Done():
		return resp, err
	case <-pause.C:
	}
	return provider.Chat(ctx, req)
}

func approvalResponseReasoned(resp *llm.ChatResponse) bool {
	return resp != nil && (resp.Usage.ReasoningOutputTokens > 0 || strings.TrimSpace(resp.ReasoningContent) != "")
}

func shouldProbeForegroundToolLoop(role string, provider llm.Provider, contract string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	return contract == modelProbeContractPlain &&
		(role == "" || role == "primary") &&
		llm.ProviderSupportsNativeTools(provider)
}

func probeNativeToolLoop(ctx context.Context, provider llm.Provider, rt modelruntime.Runtime) error {
	tool := llm.ToolDefinition{
		Name:        "selfmind_model_check",
		Description: "Required no-op tool for validating native tool-call replay.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"value": map[string]interface{}{"type": "string"},
			},
			"required": []string{"value"},
		},
	}
	firstMessages := []llm.Message{{
		Role: "user", Content: "Call selfmind_model_check exactly once with value ping. Do not answer before calling it.",
	}}
	firstRequest := llm.ChatRequest{
		Model: rt.Model, MaxTokens: 256, Messages: firstMessages, Tools: []llm.ToolDefinition{tool},
		SystemPrompt: "This is a native tool transport check. The only valid first response is one call to selfmind_model_check with value ping. Do not return text.",
	}
	if choice := requiredModelProbeToolChoice(rt, tool.Name); choice != nil {
		firstRequest.Options = map[string]interface{}{"tool_choice": choice, "parallel_tool_calls": false}
	}
	var first *llm.ChatResponse
	var call llm.ToolCall
	var lastSelectionErr error
	for attempt := 1; attempt <= modelProbeToolAttempts; attempt++ {
		response, err := chatProbe(ctx, provider, firstRequest)
		if err != nil {
			// Some OpenAI-compatible endpoints accept forced tool selection only
			// when thinking is disabled. Capability discovery is the error itself:
			// retry the same harmless check with automatic selection, without a
			// provider/model branch or weakening the required final evidence.
			if firstRequest.Options != nil && unsupportedRequiredToolChoice(err) {
				firstRequest.Options = nil
				attempt--
				continue
			}
			return err
		}
		first = response
		switch {
		case first == nil || len(first.ToolCalls) == 0:
			finishReason := "unset"
			if first != nil {
				finishReason = probeFinishReason(first.FinishReason)
			}
			lastSelectionErr = fmt.Errorf("model did not emit the required tool call (finish_reason=%s)", finishReason)
		case len(first.ToolCalls) != 1:
			lastSelectionErr = fmt.Errorf("model emitted %d tool calls; expected exactly one", len(first.ToolCalls))
		case first.ToolCalls[0].Function != tool.Name:
			lastSelectionErr = fmt.Errorf("model called %q instead of required tool %q", first.ToolCalls[0].Function, tool.Name)
		default:
			call = first.ToolCalls[0]
			lastSelectionErr = nil
		}
		if lastSelectionErr == nil {
			break
		}
		// A forced choice that was silently ignored has no stronger semantics
		// than auto. Subsequent attempts use the portable path.
		firstRequest.Options = nil
	}
	if lastSelectionErr != nil {
		return fmt.Errorf("%w after %d bounded attempts", lastSelectionErr, modelProbeToolAttempts)
	}
	if first == nil {
		return fmt.Errorf("model returned an empty response")
	}
	if strings.TrimSpace(call.ID) == "" {
		return fmt.Errorf("model emitted the required tool call without a call id")
	}
	// Provider-owned fields are opaque here. Replaying the entire returned tool
	// call in the second request tests the transport contract without teaching
	// the harness model names or vendor-specific signature semantics.
	secondMessages := append(append([]llm.Message(nil), firstMessages...),
		llm.Message{Role: "assistant", Content: first.Content, ReasoningContent: first.ReasoningContent, ToolCalls: first.ToolCalls},
		llm.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function, Content: `{"ok":true}`},
	)
	secondRequest := llm.ChatRequest{
		Model: rt.Model, MaxTokens: 128, Messages: secondMessages, Tools: []llm.ToolDefinition{tool},
		SystemPrompt: "The required native tool check has completed. Return exactly OK and do not call tools again.",
	}
	if choice := disabledModelProbeToolChoice(rt); choice != nil {
		secondRequest.Options = map[string]interface{}{"tool_choice": choice}
	}
	second, err := chatProbe(ctx, provider, secondRequest)
	if err != nil {
		return err
	}
	if second == nil || strings.TrimSpace(second.Content) == "" {
		return fmt.Errorf("model returned no final answer after the tool result")
	}
	return nil
}

// Tool selection belongs to the protocol boundary. Leaving the choice at
// "auto" tests whether a model happens to follow a natural-language request,
// not whether its tool transport works, and made an identical healthy route
// pass or fail nondeterministically. Protocols that cannot express a required
// choice keep the instruction-only fallback.
func requiredModelProbeToolChoice(rt modelruntime.Runtime, toolName string) interface{} {
	if modelProbeReasoningEnabled(rt) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(rt.Protocol)) {
	case modelruntime.ProtocolOpenAIChat, modelruntime.ProtocolOpenAICompatible:
		return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": toolName}}
	case modelruntime.ProtocolAnthropic:
		return map[string]interface{}{"type": "tool", "name": toolName}
	default:
		return nil
	}
}

func disabledModelProbeToolChoice(rt modelruntime.Runtime) interface{} {
	if modelProbeReasoningEnabled(rt) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(rt.Protocol)) {
	case modelruntime.ProtocolOpenAIChat, modelruntime.ProtocolOpenAICompatible:
		return "none"
	default:
		return nil
	}
}

func modelProbeReasoningEnabled(rt modelruntime.Runtime) bool {
	switch strings.ToLower(strings.TrimSpace(rt.ReasoningEffort)) {
	case "none", "off", "disabled":
		return false
	case "":
		// Fall through to an explicit thinking object or protocol quirk.
	default:
		return true
	}
	if kind, _ := rt.Thinking["type"].(string); strings.EqualFold(strings.TrimSpace(kind), "enabled") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(rt.Quirks.ThinkingMode), modelruntime.ThinkingModeDeepSeek)
}

func unsupportedRequiredToolChoice(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "tool_choice") &&
		(strings.Contains(message, "not support") || strings.Contains(message, "unsupported") || strings.Contains(message, "invalid"))
}

// plainProbeOutputBudget caps the "reply OK" health check. Reasoning tokens are
// output tokens, so this budget is what the model must finish REASONING inside
// before it can say anything at all — and it is a ceiling, not a spend: the
// model stops at "OK" and is billed for what it used.
//
// Measured against DeepSeek V4 with reasoning_effort=xhigh, the shape this
// probe actually sends: completion tokens came back as 14, 64, 22, 23, 38 over
// five attempts — one run hit the old ceiling of 64 exactly, was truncated, and
// returned nothing. That is a ~20% false failure on a healthy model, and this
// probe gates model changes: a false failure rolls the person's model choice
// back and parks their queued work, so setting a model looked like a coin flip.
const plainProbeOutputBudget = 512

func modelProbeRequest(rt modelruntime.Runtime, includeTools bool, contract string) llm.ChatRequest {
	req := llm.ChatRequest{
		Model:        rt.Model,
		MaxTokens:    plainProbeOutputBudget,
		SystemPrompt: "This is a model health check. Return exactly OK and do not call tools.",
		Messages:     []llm.Message{{Role: "user", Content: "Reply with OK."}},
	}
	if contract == modelProbeContractMaintenance {
		// Maintenance runs at the route's own reasoning level with a cap the
		// maintenance chain widens for it; the probe sends the same request.
		req.MaxTokens = postRunAnalyzerMaxTokens + llm.ReasoningHeadroom(rt.ReasoningEffort)
		if rt.MaxTokens > 0 && req.MaxTokens > rt.MaxTokens {
			req.MaxTokens = rt.MaxTokens
		}
		req.SystemPrompt = postRunAnalyzerSystemPrompt + "\nFor this health check, do not call tools."
		req.Messages = []llm.Message{{Role: "user", Content: "Health-check data only. Return task_decision KEEP and an empty memory_decisions array."}}
		req.Options = map[string]interface{}{
			"maintenance_contract_probe": true,
			"response_format":            map[string]interface{}{"type": maintenanceResponseFormat},
		}
	}
	if contract == modelProbeContractApproval {
		req.MaxTokens = judgeMaxTokens
		if rt.MaxTokens > 0 && req.MaxTokens > rt.MaxTokens {
			req.MaxTokens = rt.MaxTokens
		}
		req.SystemPrompt = judgeSystemPrompt
		req.Messages = []llm.Message{{Role: "user", Content: "Review a read-only status command inside the current workspace. The person asked to inspect the workspace; no network, credentials, privilege, or writes are involved."}}
		req.Options = map[string]interface{}{
			"approval_contract_probe": true,
			"reasoning_effort":        modelruntime.LowestLatencyReasoning(rt),
			"response_format":         map[string]interface{}{"type": "json_object"},
		}
	}
	if includeTools {
		var required []string
		req.Tools = []llm.ToolDefinition{{
			Name:        "selfmind_model_check",
			Description: "Optional no-op tool used only to validate tool schema compatibility.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"value": map[string]interface{}{"type": "string"},
				},
				"required": required,
			},
		}}
	}
	return req
}

func isMaintenanceProbeRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "auxiliary" {
		return true
	}
	return llm.ModelRole(role) == llm.RoleMemoryExtract
}

func isApprovalProbeRole(role string) bool {
	return llm.ModelRole(strings.ToLower(strings.TrimSpace(role))) == llm.RoleFastClassifier
}

func modelProbeContractForRole(role string) string {
	if isApprovalProbeRole(role) {
		return modelProbeContractApproval
	}
	if isMaintenanceProbeRole(role) {
		return modelProbeContractMaintenance
	}
	return modelProbeContractPlain
}

// ProbeConfiguredModelRoles performs a bounded live health check for the
// auxiliary route and explicit role overrides. Inherited auxiliary roles are
// represented by one probe so doctor does not multiply quota usage.
func ProbeConfiguredModelRoles(ctx context.Context, cfg *config.Config) []ModelRoleProbe {
	if cfg == nil {
		return nil
	}
	roleNames := make([]string, 0, len(cfg.Models.Roles))
	for roleName := range cfg.Models.Roles {
		roleNames = append(roleNames, roleName)
	}
	sort.Strings(roleNames)

	resolver := modelruntime.NewResolver(cfg)
	targets := make(map[string]*roleProbeTarget)
	var unresolved []ModelRoleProbe
	auxiliary := cfg.EffectiveAuxiliary()
	if cfg.AuxiliaryEnabled() && (strings.TrimSpace(auxiliary.Provider) != "" || strings.TrimSpace(auxiliary.Model) != "") {
		selection := modelruntime.Selection{
			Provider: auxiliary.Provider, Model: auxiliary.Model,
			ContextLength: auxiliary.ContextLength, ReasoningEffort: auxiliary.Reasoning,
			ServiceTier: auxiliary.ServiceTier,
		}
		rt, err := resolver.Resolve(ctx, selection)
		if err != nil {
			unresolved = append(unresolved, ModelRoleProbe{Roles: []string{"auxiliary"}, Provider: selection.Provider, Model: selection.Model, Err: err})
		} else {
			contract := modelProbeContractForRole("auxiliary")
			key := strings.Join([]string{rt.Provider, rt.Model, rt.Protocol, rt.BaseURL, contract}, "\x00")
			targets[key] = &roleProbeTarget{roles: []string{"auxiliary"}, runtime: rt, provider: buildProviderForSelectionWithRuntime(cfg, selection), contract: contract}
		}
		// fast_classifier inherits the auxiliary route, but it has a different
		// wire contract and latency policy from maintenance. Probe it separately
		// unless an explicit override below will represent the production route.
		explicitFastClassifier, explicitlyConfigured := cfg.Models.Roles[string(llm.RoleFastClassifier)]
		if !explicitlyConfigured || roleConfigEmpty(explicitFastClassifier) {
			roleCfg, _, ok := cfg.ResolveAuxiliaryRole(string(llm.RoleFastClassifier))
			if ok && !roleConfigEmpty(roleCfg) {
				providerName := firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg))
				selection := roleProviderSelection(llm.RoleFastClassifier, providerName, roleCfg)
				rt, err := resolver.Resolve(ctx, selection)
				if err != nil {
					unresolved = append(unresolved, ModelRoleProbe{Roles: []string{string(llm.RoleFastClassifier)}, Provider: selection.Provider, Model: selection.Model, Err: err})
				} else {
					contract := modelProbeContractApproval
					key := strings.Join([]string{rt.Provider, rt.Model, rt.Protocol, rt.BaseURL, contract}, "\x00")
					targets[key] = &roleProbeTarget{
						roles: []string{string(llm.RoleFastClassifier)}, runtime: rt,
						provider: buildProviderForSelectionWithRuntime(cfg, selection), contract: contract,
					}
				}
			}
		}
	}
	for _, roleName := range roleNames {
		if !cfg.AuxiliaryEnabled() && isAuxiliaryModelRole(llm.ModelRole(roleName)) {
			continue
		}
		roleCfg := cfg.Models.Roles[roleName]
		if roleConfigEmpty(roleCfg) {
			continue
		}
		selection := roleProviderSelection(llm.ModelRole(roleName), firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg)), roleCfg)
		rt, err := resolver.Resolve(ctx, selection)
		if err != nil {
			unresolved = append(unresolved, ModelRoleProbe{Roles: []string{roleName}, Provider: selection.Provider, Model: selection.Model, Err: err})
			continue
		}
		contract := modelProbeContractForRole(roleName)
		key := strings.Join([]string{rt.Provider, rt.Model, rt.Protocol, rt.BaseURL, contract}, "\x00")
		if target := targets[key]; target != nil {
			target.roles = append(target.roles, roleName)
			continue
		}
		targets[key] = &roleProbeTarget{
			roles:    []string{roleName},
			runtime:  rt,
			provider: buildProviderForSelectionWithRuntime(cfg, selection),
			contract: contract,
		}
	}

	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := append([]ModelRoleProbe(nil), unresolved...)
	for _, key := range keys {
		target := targets[key]
		probe := ModelRoleProbe{Roles: append([]string(nil), target.roles...), Provider: target.runtime.Provider, Model: target.runtime.Model}
		start := time.Now()
		if target.provider == nil {
			probe.Err = fmt.Errorf("provider could not be built")
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, modelProbeTimeout(target.contract))
			probeCtx = llm.WithModelContext(probeCtx, llm.ModelContext{Role: llm.ModelRole(target.roles[0])})
			probe.MaintenanceContractTested = target.contract == modelProbeContractMaintenance
			probe.ApprovalContractTested = target.contract == modelProbeContractApproval
			probe.NativeToolsTested = llm.ProviderSupportsNativeTools(target.provider) && target.contract == modelProbeContractPlain
			resp, err := chatProbe(probeCtx, target.provider, modelProbeRequest(target.runtime, probe.NativeToolsTested, target.contract))
			switch {
			case err != nil:
				probe.Err = err
			case modelProbeContentError(resp) != nil:
				probe.Err = modelProbeContentError(resp)
			case probe.MaintenanceContractTested && maintenanceFinishReasonTruncated(resp.FinishReason):
				probe.Err = fmt.Errorf("maintenance contract was truncated (finish_reason=%s)", resp.FinishReason)
			case probe.MaintenanceContractTested:
				if _, decodeErr := decodePostRunAnalysis(resp.Content); decodeErr != nil {
					probe.Err = fmt.Errorf("maintenance contract failed: %w", decodeErr)
				} else {
					probe.MaintenanceContractPassed = true
				}
			case probe.ApprovalContractTested && maintenanceFinishReasonTruncated(resp.FinishReason):
				probe.Err = fmt.Errorf("approval contract was truncated (finish_reason=%s)", resp.FinishReason)
			case probe.ApprovalContractTested:
				if validateErr := tools.ValidateStructuredApprovalReply(resp.Content); validateErr != nil {
					probe.Err = fmt.Errorf("approval contract failed: %w", validateErr)
				} else {
					probe.ApprovalContractPassed = true
				}
			}
			cancel()
		}
		probe.Latency = time.Since(start)
		results = append(results, probe)
	}
	sort.Slice(results, func(i, j int) bool {
		return strings.Join(results[i].Roles, ",") < strings.Join(results[j].Roles, ",")
	})
	return results
}

func modelProbeContentError(resp *llm.ChatResponse) error {
	if resp == nil {
		return fmt.Errorf("model returned an empty response")
	}
	if strings.TrimSpace(resp.Content) != "" {
		return nil
	}
	if len(resp.ToolCalls) > 0 {
		return fmt.Errorf("model returned an unexpected tool call instead of probe text")
	}
	// Being CUT OFF is a different diagnosis from a model that answered with
	// nothing, and the finish reason is the only thing that separates them.
	// Reporting just the symptom is what made a truncated reasoning model look
	// like a broken route, in the one message the person and the log both see.
	if strings.EqualFold(strings.TrimSpace(resp.FinishReason), "length") {
		return fmt.Errorf("model produced no answer within the %d-token probe budget (finish_reason=length); a reasoning model may need more", plainProbeOutputBudget)
	}
	return fmt.Errorf("model returned an empty response (finish_reason=%s)", probeFinishReason(resp.FinishReason))
}

func probeFinishReason(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unset"
}
