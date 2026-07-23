package llm

import (
	"strings"
	"testing"
)

func TestAnthropicEmptyResponsePreservesStopReasonAndUsage(t *testing.T) {
	adapter := &AnthropicAdapter{ProviderName: "kimi-coding"}
	_, err := adapter.decodeAnthropicResponse(strings.NewReader(`{
		"content": [],
		"stop_reason": "max_tokens",
		"usage": {
			"input_tokens": 1234,
			"output_tokens": 3072,
			"cache_read_input_tokens": 111,
			"cache_creation_input_tokens": 22
		}
	}`))
	if err == nil {
		t.Fatal("empty response must be an error")
	}
	info, ok := ProviderErrorInfo(err)
	if !ok {
		t.Fatalf("error type = %T, want ProviderError", err)
	}
	if info.Class != ProviderErrorEmptyResponse || info.StopReason != "max_tokens" {
		t.Fatalf("provider error = %+v", info)
	}
	if info.Usage.InputTokens != 1234 || info.Usage.OutputTokens != 3072 ||
		info.Usage.CacheReadInputTokens != 111 || info.Usage.CacheCreationInputTokens != 22 {
		t.Fatalf("usage = %+v", info.Usage)
	}
}
