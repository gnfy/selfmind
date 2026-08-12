package llm

import (
	"reflect"
	"testing"
)

func TestRegisteredTransports(t *testing.T) {
	got := RegisteredTransports()
	want := []string{
		TransportAnthropic,
		TransportResponses,
		TransportOpenAIChat,
		TransportOpenAICompatible,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisteredTransports() = %#v, want %#v", got, want)
	}
}

func TestBuildTransportProviderUsesProtocolFamilies(t *testing.T) {
	tests := []struct {
		name      string
		cfg       TransportConfig
		wantType  interface{}
		wantModel string
	}{
		{
			name:      "anthropic",
			cfg:       TransportConfig{Protocol: TransportAnthropic, APIKey: "k", Model: "claude-test", BaseURL: "https://api.anthropic.com"},
			wantType:  &AnthropicAdapter{},
			wantModel: "claude-test",
		},
		{
			name:      "responses",
			cfg:       TransportConfig{Protocol: TransportResponses, APIKey: "k", Model: "gpt-test", ResponsesStoreFalse: true, ResponsesRequireStream: true},
			wantType:  &ResponsesAdapter{},
			wantModel: "gpt-test",
		},
		{
			name:      "openai chat",
			cfg:       TransportConfig{Protocol: TransportOpenAIChat, APIKey: "k", Model: "gpt-test", BaseURL: "https://api.openai.com/v1"},
			wantType:  &OpenAIAdapter{},
			wantModel: "gpt-test",
		},
		{
			name:      "openrouter compatible",
			cfg:       TransportConfig{Provider: "openrouter", Protocol: TransportOpenAICompatible, APIKey: "k", Model: "openrouter-test"},
			wantType:  &OpenRouterAdapter{},
			wantModel: "openrouter-test",
		},
		{
			name:      "gemini compatible",
			cfg:       TransportConfig{Provider: "gemini", Protocol: TransportOpenAICompatible, APIKey: "k", Model: "gemini-test"},
			wantType:  &GeminiAdapter{},
			wantModel: "gemini-test",
		},
		{
			name:      "generic compatible",
			cfg:       TransportConfig{Provider: "custom-vendor", Protocol: TransportOpenAICompatible, APIKey: "k", Model: "model-test", BaseURL: "https://example.test/v1"},
			wantType:  &GenericOpenAIAdapter{},
			wantModel: "model-test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildTransportProvider(tt.cfg)
			if got == nil {
				t.Fatalf("BuildTransportProvider() returned nil")
			}
			if reflect.TypeOf(got) != reflect.TypeOf(tt.wantType) {
				t.Fatalf("provider type = %T, want %T", got, tt.wantType)
			}
			if getter, ok := got.(interface{ GetModel() string }); !ok || getter.GetModel() != tt.wantModel {
				t.Fatalf("model = %q, want %q", modelOf(got), tt.wantModel)
			}
			if responses, ok := got.(*ResponsesAdapter); ok {
				if responses.Store == nil || *responses.Store {
					t.Fatalf("responses Store = %#v, want false pointer", responses.Store)
				}
				if !responses.RequireStream {
					t.Fatalf("responses RequireStream = false, want true")
				}
			}
		})
	}
}

func TestBuildTransportProviderCarriesGenericRequestExtras(t *testing.T) {
	extraBody := map[string]interface{}{"user_id": "user-42"}
	extraQuery := map[string]interface{}{"api-version": "2026-08-11"}
	headers := map[string]string{"X-Vendor": "test"}
	base := TransportConfig{APIKey: "k", Model: "test", Headers: headers, ExtraBody: extraBody, ExtraQuery: extraQuery}

	base.Protocol = TransportOpenAICompatible
	base.Provider = "deepseek"
	openai := BuildTransportProvider(base).(*GenericOpenAIAdapter)
	if openai.ExtraBody["user_id"] != "user-42" || openai.ExtraQuery["api-version"] != "2026-08-11" || openai.Headers["X-Vendor"] != "test" {
		t.Fatalf("openai extras = body:%#v query:%#v headers:%#v", openai.ExtraBody, openai.ExtraQuery, openai.Headers)
	}

	base.Protocol = TransportAnthropic
	base.Provider = "anthropic"
	anthropic := BuildTransportProvider(base).(*AnthropicAdapter)
	if anthropic.ExtraBody["user_id"] != "user-42" || anthropic.ExtraQuery["api-version"] != "2026-08-11" {
		t.Fatalf("anthropic extras = body:%#v query:%#v", anthropic.ExtraBody, anthropic.ExtraQuery)
	}

	base.Protocol = TransportResponses
	base.Provider = "codex-cli"
	responses := BuildTransportProvider(base).(*ResponsesAdapter)
	if responses.ExtraBody["user_id"] != "user-42" || responses.ExtraQuery["api-version"] != "2026-08-11" {
		t.Fatalf("responses extras = body:%#v query:%#v", responses.ExtraBody, responses.ExtraQuery)
	}
}

func modelOf(value interface{}) string {
	if getter, ok := value.(interface{ GetModel() string }); ok {
		return getter.GetModel()
	}
	return ""
}
