package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
)

func TestClassifyModelProbeFailureOnlyBlamesDeterministicRouteFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want modelchange.FailureClass
	}{
		{name: "semantic contract", err: errors.New("maintenance contract failed"), want: modelchange.FailureModel},
		{name: "invalid request", err: &llm.ProviderError{Class: llm.ProviderErrorInvalidRequest}, want: modelchange.FailureModel},
		{name: "auth", err: &llm.ProviderError{Class: llm.ProviderErrorAuth}, want: modelchange.FailureModel},
		{name: "rate limit", err: &llm.ProviderError{Class: llm.ProviderErrorRateLimit}, want: modelchange.FailureInfrastructure},
		{name: "transient", err: &llm.ProviderError{Class: llm.ProviderErrorTransient}, want: modelchange.FailureInfrastructure},
		{name: "deadline", err: context.DeadlineExceeded, want: modelchange.FailureInfrastructure},
		{name: "unknown provider error", err: &llm.ProviderError{Class: llm.ProviderErrorUnknown}, want: modelchange.FailureInfrastructure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyModelProbeFailure(test.err); got != test.want {
				t.Fatalf("class = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExpandedModelValidationRoutesCoversOnlyBackgroundRolesThatInherit(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
		string(modelchange.RouteMemoryExtract): {Provider: "anthropic", Model: "claude-role"},
	}}}
	got := expandedModelValidationRoutes(cfg, []modelchange.Route{modelchange.RouteAuxiliary})
	want := []modelchange.Route{
		modelchange.RouteAuxiliary,
		modelchange.RouteFastClassifier,
		modelchange.RouteBackgroundReview,
		modelchange.RouteSkillCurator,
		modelchange.RouteSemanticRecall,
		modelchange.RouteSummarizer,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}
