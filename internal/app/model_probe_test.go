package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestProbeResolvedModelValidatesOpenAIToolSchema(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Tools      []map[string]interface{} `json:"tools"`
			ToolChoice interface{}              `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		function := request.Tools[0]["function"].(map[string]interface{})
		parameters := function["parameters"].(map[string]interface{})
		required, _ := parameters["required"].([]interface{})
		if len(required) != 1 || required[0] != "value" {
			t.Fatalf("required = %#v, want value", parameters["required"])
		}
		w.Header().Set("content-type", "application/json")
		if requests == 1 {
			choice, _ := request.ToolChoice.(map[string]interface{})
			function, _ := choice["function"].(map[string]interface{})
			if choice["type"] != "function" || function["name"] != "selfmind_model_check" {
				t.Fatalf("first tool_choice = %#v", request.ToolChoice)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{\"value\":\"ping\"}"}}]}}]}`)
			return
		}
		if request.ToolChoice != "none" {
			t.Fatalf("second tool_choice = %#v, want none", request.ToolChoice)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "deepseek",
		Protocol: modelruntime.ProtocolOpenAICompatible,
		Model:    "deepseek-v4-flash",
		BaseURL:  server.URL,
		APIKey:   "test-key",
		Quirks: modelruntime.ProviderQuirks{
			SupportsTools: true,
		},
	})
	if probe.Err != nil {
		t.Fatal(probe.Err)
	}
	if !probe.NativeToolsTested {
		t.Fatal("native tool schema was not tested")
	}
	if !probe.ToolLoopTested || !probe.ToolLoopPassed || requests != 2 {
		t.Fatalf("tool loop probe=%+v requests=%d", probe, requests)
	}
}

func TestProbeResolvedModelReportsUnexpectedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"another_tool","arguments":"{}"}}]}}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "onelinkai", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key",
		Quirks: modelruntime.ProviderQuirks{SupportsTools: true},
	})
	if probe.Err == nil || !strings.Contains(probe.Err.Error(), "instead of required tool") {
		t.Fatalf("probe error = %v", probe.Err)
	}
}

func TestProbeResolvedReasoningModelRetriesAutomaticToolSelection(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			ToolChoice interface{} `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ToolChoice != nil {
			t.Fatalf("reasoning probe forced unsupported tool choice: %#v", request.ToolChoice)
		}
		w.Header().Set("content-type", "application/json")
		switch requests {
		case 1, 2:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
		case 3:
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{\"value\":\"ping\"}"}}]}}]}`)
		case 4:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "custom", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "reasoning-model", BaseURL: server.URL, APIKey: "test-key", ReasoningEffort: "high",
		Quirks: modelruntime.ProviderQuirks{SupportsTools: true},
	})
	if probe.Err != nil || !probe.ToolLoopPassed || requests != 4 {
		t.Fatalf("probe=%+v requests=%d", probe, requests)
	}
}

func TestProbeResolvedModelFallsBackWhenForcedChoiceIsUnsupported(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			ToolChoice interface{} `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		switch requests {
		case 1:
			if request.ToolChoice == nil {
				t.Fatal("first request did not try deterministic tool selection")
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"tool_choice object is not supported","type":"invalid_request_error","code":"invalid_request"}}`)
		case 2:
			if request.ToolChoice != nil {
				t.Fatalf("fallback kept tool_choice: %#v", request.ToolChoice)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{\"value\":\"ping\"}"}}]}}]}`)
		case 3:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "custom", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "tool-model", BaseURL: server.URL, APIKey: "test-key",
		Quirks: modelruntime.ProviderQuirks{SupportsTools: true},
	})
	if probe.Err != nil || !probe.ToolLoopPassed || requests != 3 {
		t.Fatalf("probe=%+v requests=%d", probe, requests)
	}
}

func TestProbeResolvedModelReplaysOpaqueToolMetadata(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		if requests == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{\"value\":\"ping\"}"},"extra_content":{"google":{"thought_signature":"required-signature"}}}]}}]}`)
			return
		}
		var replay json.RawMessage
		for _, message := range request.Messages {
			if message.Role == "assistant" && len(message.ToolCalls) == 1 {
				replay = message.ToolCalls[0].ExtraContent
				break
			}
		}
		if string(replay) != `{"google":{"thought_signature":"required-signature"}}` {
			http.Error(w, "Function call is missing a thought_signature", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "google", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "gemini-test", BaseURL: server.URL, APIKey: "test-key",
		Quirks: modelruntime.ProviderQuirks{SupportsTools: true},
	})
	if probe.Err != nil || !probe.ToolLoopTested || !probe.ToolLoopPassed || requests != 2 {
		t.Fatalf("probe=%+v requests=%d", probe, requests)
	}
}

// Maintenance runs at the route's configured reasoning level, so the probe that
// gates a model change must send that level — and the cap the maintenance chain
// widens for it — rather than a thinking-free shape the real work never uses.
func TestProbeResolvedModelValidatesMaintenanceContract(t *testing.T) {
	for _, tc := range []struct {
		reasoning    string
		wantThinking string
		wantMax      int
	}{
		{reasoning: "high", wantThinking: "enabled", wantMax: 16000},
		{reasoning: "none", wantThinking: "disabled", wantMax: postRunAnalyzerMaxTokens},
	} {
		t.Run(tc.reasoning, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					MaxTokens int                      `json:"max_tokens"`
					Tools     []map[string]interface{} `json:"tools"`
					Thinking  map[string]interface{}   `json:"thinking"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.MaxTokens != tc.wantMax {
					t.Errorf("maintenance probe max_tokens=%d, want %d", request.MaxTokens, tc.wantMax)
				}
				if len(request.Tools) != 0 {
					t.Errorf("maintenance probe must not carry agent tools: %#v", request.Tools)
				}
				if request.Thinking["type"] != tc.wantThinking {
					t.Errorf("maintenance probe thinking=%#v, want %s", request.Thinking, tc.wantThinking)
				}
				w.Header().Set("content-type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"task_decision\":\"KEEP\",\"memory_decisions\":[]}"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()

			probe := ProbeResolvedModelForRole(t.Context(), modelruntime.Runtime{
				Provider: "deepseek", Protocol: modelruntime.ProtocolOpenAICompatible,
				Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key", MaxTokens: 16000,
				ReasoningEffort: tc.reasoning, Thinking: map[string]interface{}{"type": "enabled"},
				Quirks: modelruntime.ProviderQuirks{ThinkingMode: modelruntime.ThinkingModeDeepSeek, SupportsTools: true},
			}, "memory_extract")
			if probe.Err != nil || !probe.MaintenanceContractTested || !probe.MaintenanceContractPassed {
				t.Fatalf("probe=%+v", probe)
			}
			if probe.NativeToolsTested {
				t.Fatal("native tools were reported as tested even though the maintenance contract intentionally omitted them")
			}
		})
	}
}

// A model that reasons by default ignores an omitted reasoning parameter, and
// the approval judge then thinks through every verdict inside the person's
// turn (observed: 206-236 reasoning tokens and 6.9-11.6s against a 10s triage
// budget). The probe must notice it from what the endpoint returns — never
// from its name — and report the encoding that turns reasoning off, but only
// one the endpoint accepts and actually honors.
func TestApprovalProbeFindsHowToTurnReasoningOff(t *testing.T) {
	const verdict = `{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"Bounded read-only check.\"}`
	for _, tc := range []struct {
		name             string
		quirks           modelruntime.ProviderQuirks
		levels           []string
		reasonsByDefault bool
		acceptsNone      bool
		wantMode         string
		wantRequests     int
		wantNotice       string
	}{
		{name: "reasons by default and honors none", reasonsByDefault: true, acceptsNone: true,
			wantMode: modelruntime.ThinkingModeEffortNone, wantRequests: 2, wantNotice: `reasoning_effort "none" turned it off`},
		{name: "resolved default encoding is replaceable", quirks: modelruntime.ProviderQuirks{ThinkingMode: modelruntime.ThinkingModeOpenAI},
			reasonsByDefault: true, acceptsNone: true, wantMode: modelruntime.ThinkingModeEffortNone, wantRequests: 2, wantNotice: "turned it off"},
		{name: "rejects none", reasonsByDefault: true, wantRequests: 2, wantNotice: "may be slow"},
		{name: "reasoning already off", acceptsNone: true, wantRequests: 1},
		{name: "declared encoding is kept", quirks: modelruntime.ProviderQuirks{ThinkingMode: modelruntime.ThinkingModeOmit},
			reasonsByDefault: true, acceptsNone: true, wantRequests: 1, wantNotice: "may be slow"},
		{name: "lowest tier reasons by design", levels: []string{"low", "high"}, reasonsByDefault: true, acceptsNone: true, wantRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body struct {
					ReasoningEffort string `json:"reasoning_effort"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.ReasoningEffort == "none" && !tc.acceptsNone {
					w.Header().Set("content-type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"unsupported reasoning_effort","type":"invalid_request_error"}}`)
					return
				}
				reasoning := 0
				if tc.reasonsByDefault && body.ReasoningEffort != "none" {
					reasoning = 300
				}
				w.Header().Set("content-type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"message":{"content":"%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
					verdict, reasoning+40, reasoning)
			}))
			defer server.Close()

			probe := ProbeResolvedModelForRole(t.Context(), modelruntime.Runtime{
				Provider: "lab", Protocol: modelruntime.ProtocolOpenAICompatible, Model: "lab-model",
				BaseURL: server.URL, APIKey: "test-key", ReasoningLevels: tc.levels, Quirks: tc.quirks,
			}, "fast_classifier")
			if probe.Err != nil || !probe.ApprovalContractPassed {
				t.Fatalf("the approval contract itself must pass: %+v", probe)
			}
			if probe.ApprovalThinkingMode != tc.wantMode {
				t.Fatalf("thinking mode = %q, want %q (notice %q)", probe.ApprovalThinkingMode, tc.wantMode, probe.ApprovalNotice)
			}
			if requests != tc.wantRequests {
				t.Fatalf("requests = %d, want %d", requests, tc.wantRequests)
			}
			if tc.wantNotice == "" && probe.ApprovalNotice != "" {
				t.Fatalf("unexpected notice %q", probe.ApprovalNotice)
			}
			if tc.wantNotice != "" && !strings.Contains(probe.ApprovalNotice, tc.wantNotice) {
				t.Fatalf("notice %q does not say %q", probe.ApprovalNotice, tc.wantNotice)
			}
		})
	}
}

// One transient provider failure used to fail a whole model change, or park
// it for manual recovery, although the same request succeeded moments later
// (observed: HTTP 500 "An error occurred in model serving", three times in four
// switches). A deterministic failure is still reported at once.
func TestModelProbeRetriesOneTransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantOK       bool
		wantRequests int
	}{
		{name: "transient 5xx", status: http.StatusInternalServerError,
			body: `{"error":{"message":"An error occurred in model serving","type":"server_error"}}`, wantOK: true, wantRequests: 2},
		{name: "invalid request", status: http.StatusBadRequest,
			body: `{"error":{"message":"unsupported parameter","type":"invalid_request_error"}}`, wantRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("content-type", "application/json")
				if requests == 1 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()

			probe := ProbeResolvedModelForRole(t.Context(), modelruntime.Runtime{
				Provider: "lab", Protocol: modelruntime.ProtocolOpenAICompatible,
				Model: "lab-model", BaseURL: server.URL, APIKey: "test-key",
			}, "semantic_recall")
			if (probe.Err == nil) != tc.wantOK || requests != tc.wantRequests {
				t.Fatalf("probe err=%v requests=%d, want ok=%t requests=%d", probe.Err, requests, tc.wantOK, tc.wantRequests)
			}
		})
	}
}

func TestProbeResolvedModelValidatesApprovalContractAtCapabilityFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ReasoningEffort string                 `json:"reasoning_effort"`
			ResponseFormat  map[string]interface{} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ReasoningEffort != "low" {
			t.Fatalf("approval probe reasoning=%q, want low", request.ReasoningEffort)
		}
		if request.ResponseFormat["type"] != "json_object" {
			t.Fatalf("approval probe response format=%#v", request.ResponseFormat)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"The requested read-only status check is bounded.\"}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	probe := ProbeResolvedModelForRole(t.Context(), modelruntime.Runtime{
		Provider: "google", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "gemini-3.8-flash", BaseURL: server.URL, APIKey: "test-key",
		ReasoningLevels: []string{"low", "medium", "high"},
	}, "fast_classifier")
	if probe.Err != nil || !probe.ApprovalContractTested || !probe.ApprovalContractPassed {
		t.Fatalf("probe=%+v", probe)
	}
	if probe.NativeToolsTested {
		t.Fatal("approval contract probe must not carry agent tools")
	}
}

func TestProbeResolvedModelValidatesNativeToolLoop(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []struct {
				Role             string `json:"role"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("content-type", "application/json")
		switch requests {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"content":null,"reasoning_content":"call the required check","tool_calls":[{"id":"call-1","type":"function","function":{"name":"selfmind_model_check","arguments":"{\"value\":\"ping\"}"}}]}}]}`)
		case 2:
			if len(request.Messages) < 2 || request.Messages[len(request.Messages)-2].ReasoningContent == "" {
				t.Fatal("second request did not replay reasoning_content")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	probe := ProbeResolvedModel(t.Context(), modelruntime.Runtime{
		Provider: "deepseek", Protocol: modelruntime.ProtocolOpenAICompatible,
		Model: "deepseek-v4-flash", BaseURL: server.URL, APIKey: "test-key",
		ReasoningEffort: "xhigh", Thinking: map[string]interface{}{"type": "enabled"},
		Quirks: modelruntime.ProviderQuirks{
			ThinkingMode: modelruntime.ThinkingModeDeepSeek, UserIdentityField: "user_id", SupportsTools: true,
		},
	})
	if probe.Err != nil || !probe.ToolLoopTested || !probe.ToolLoopPassed || requests != 2 {
		t.Fatalf("probe=%+v requests=%d", probe, requests)
	}
}

func TestResolveModelRuntimeUsesAuxiliaryAndRoleOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		"memory_extract": {Model: "memory-model"},
	}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Normalize()

	auxiliary, err := ResolveModelRuntime(context.Background(), cfg, "auxiliary")
	if err != nil {
		t.Fatal(err)
	}
	if auxiliary.Model != "aux-model" {
		t.Fatalf("auxiliary model = %q", auxiliary.Model)
	}
	memory, err := ResolveModelRuntime(context.Background(), cfg, "memory_extract")
	if err != nil {
		t.Fatal(err)
	}
	if memory.Provider != "openai" || memory.Model != "memory-model" {
		t.Fatalf("memory runtime = %+v", memory)
	}
}

func TestResolveModelRuntimeDoesNotApplyAuxiliaryToVision(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Normalize()

	if _, err := ResolveModelRuntime(context.Background(), cfg, "vision"); err == nil {
		t.Fatal("vision must require an explicit capability-specific role")
	}
}

func TestConfiguredRoleProbesKeepInheritedApprovalContractDistinct(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var system string
		for _, message := range request.Messages {
			if message.Role == "system" {
				system = message.Content
				break
			}
		}
		w.Header().Set("content-type", "application/json")
		if strings.Contains(system, "additional human confirmation") {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"The bounded health check is authorized.\"}"},"finish_reason":"stop"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"task_decision\":\"KEEP\",\"memory_decisions\":[]}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Providers.OpenAI.BaseURL = server.URL
	cfg.Normalize()

	probes := ProbeConfiguredModelRoles(t.Context(), cfg)
	if len(probes) != 2 || requests != 2 {
		t.Fatalf("probes=%+v requests=%d, want one maintenance and one approval probe", probes, requests)
	}
	var maintenancePassed, approvalPassed bool
	for _, probe := range probes {
		if probe.Err != nil {
			t.Fatalf("probe failed: %+v", probe)
		}
		maintenancePassed = maintenancePassed || probe.MaintenanceContractPassed
		approvalPassed = approvalPassed || probe.ApprovalContractPassed
	}
	if !maintenancePassed || !approvalPassed {
		t.Fatalf("probes=%+v", probes)
	}
}

// TestModelProbeBudgetFitsAReasoningModel pins the output cap on the probe that
// GATES MODEL CHANGES. Reasoning tokens are output tokens, so this cap is what
// the model must finish thinking inside before it can answer at all. Measured
// against DeepSeek V4 at reasoning_effort=xhigh, in this probe's exact shape,
// five attempts used 14, 64, 22, 23 and 38 completion tokens — one hit the old
// ceiling of 64 exactly and returned nothing. A false failure here rolls the
// person's model choice back and parks their queued work, which is why setting
// a model behaved like a coin flip.
func TestModelProbeBudgetFitsAReasoningModel(t *testing.T) {
	req := modelProbeRequest(modelruntime.Runtime{Model: "probe-model"}, false, modelProbeContractPlain)
	if req.MaxTokens < 128 {
		t.Fatalf("plain probe budget = %d; a reasoning model can spend more than that before saying anything",
			req.MaxTokens)
	}
}

// TestModelProbeContentErrorNamesTruncation: an empty answer that was CUT OFF
// is a different diagnosis from a route that answered with nothing, and the
// finish reason is the only thing separating them.
func TestModelProbeContentErrorNamesTruncation(t *testing.T) {
	err := modelProbeContentError(&llm.ChatResponse{FinishReason: "length"})
	if err == nil {
		t.Fatal("a truncated empty answer must still fail the probe")
	}
	for _, want := range []string{"finish_reason=length", "budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("probe error must name %q so a log shows the cause: %q", want, err)
		}
	}

	if err := modelProbeContentError(&llm.ChatResponse{FinishReason: "content_filter"}); err == nil ||
		!strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("a non-truncation empty answer should name its finish reason: %v", err)
	}
	if err := modelProbeContentError(&llm.ChatResponse{}); err == nil ||
		!strings.Contains(err.Error(), "unset") {
		t.Fatalf("an absent finish reason should read as unset: %v", err)
	}
	if err := modelProbeContentError(&llm.ChatResponse{Content: "OK", FinishReason: "stop"}); err != nil {
		t.Fatalf("a normal answer must pass: %v", err)
	}
}
