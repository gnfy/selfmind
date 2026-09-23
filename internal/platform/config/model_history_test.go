package config

import "testing"

func TestRememberedModelsRetainManualReasoningAndRemainBounded(t *testing.T) {
	var models ModelsConfig
	models.RememberModel("google", "gemini-custom", "high")
	models.RememberModel("google", "gemini-custom", "low")
	models.RememberModel("google", "gemini-custom", "HIGH")
	if len(models.Remembered) != 1 {
		t.Fatalf("remembered models = %+v", models.Remembered)
	}
	got := models.Remembered[0]
	if got.Provider != "google" || got.Model != "gemini-custom" || len(got.Reasoning) != 2 || got.Reasoning[0] != "high" || got.Reasoning[1] != "low" {
		t.Fatalf("remembered model = %+v", got)
	}
	for index := 0; index < maxRememberedModels+5; index++ {
		models.RememberModel("provider", string(rune('a'+index)), "")
	}
	if len(models.Remembered) != maxRememberedModels {
		t.Fatalf("remembered count = %d", len(models.Remembered))
	}
}

func TestForgetRememberedModelIsProviderScoped(t *testing.T) {
	models := ModelsConfig{Remembered: []RememberedModelConfig{
		{Provider: "google", Model: "shared"},
		{Provider: "openai", Model: "shared"},
	}}
	if !models.ForgetModel("GOOGLE", "shared") {
		t.Fatal("expected remembered model removal")
	}
	if len(models.Remembered) != 1 || models.Remembered[0].Provider != "openai" {
		t.Fatalf("remaining models = %+v", models.Remembered)
	}
}
