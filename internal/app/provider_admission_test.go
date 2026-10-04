package app

import (
	"testing"

	"selfmind/internal/modelruntime"
)

func TestProviderRequestRouteIdentitySharesModelsButSeparatesCredentialsAndEndpoints(t *testing.T) {
	base := modelruntime.Runtime{
		Provider: "openai", BaseURL: "https://api.example.test/v1/chat/completions",
		APIKey: "key-a", Model: "model-a",
	}
	baseID := providerRequestRouteID(base)
	otherModel := base
	otherModel.Model = "model-b"
	otherModel.BaseURL = "https://API.example.test/v1"
	if got := providerRequestRouteID(otherModel); got != baseID {
		t.Fatalf("models sharing endpoint and credential were split: %s != %s", got, baseID)
	}
	otherCredential := base
	otherCredential.APIKey = "key-b"
	if got := providerRequestRouteID(otherCredential); got == baseID {
		t.Fatal("different credentials shared a provider quota route")
	}
	otherEndpoint := base
	otherEndpoint.BaseURL = "https://other.example.test/v1"
	if got := providerRequestRouteID(otherEndpoint); got == baseID {
		t.Fatal("different endpoints shared a provider quota route")
	}
}
