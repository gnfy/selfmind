package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// This is the full application wiring boundary that reproduced the DeepSeek
// tools[15].function.name 400: many installed Skills must not become provider
// function schemas, and the remaining generic catalog has a regression budget.
func TestApplicationToolCatalogHasNoPerSkillSchemasAndStaysWithinBudget(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Evolution: config.EvolutionConfig{SkillsDir: base}}
	root := tools.SkillsDirForTenant(base, "default")
	for index := 0; index < 80; index++ {
		name := fmt.Sprintf("generated-skill-%03d", index)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("---\nname: %s\ndescription: Generated catalog fixture %d\n---\n\n## Procedure\nInspect the declared input.\n", name, index)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dispatcher, err := InitTools(nil, cfg, nil, "default", nil, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"queue_user_input", "set_delivery_target", "work_search", "work_inspect", "work_select"} {
		if !hasTool(dispatcher, name) {
			t.Fatalf("application tool catalog missing %s", name)
		}
	}
	agent := kernel.NewAgent(nil, dispatcher, &judgeCaptureProvider{}, "test", 1, 1, nil)
	preview := agent.ProviderToolCatalogPreview(context.Background())
	if !preview.Valid() {
		t.Fatalf("provider catalog invalid: %+v", preview)
	}
	for _, entry := range preview.Entries {
		if strings.HasPrefix(strings.ToLower(entry.SourceName), "skill:") {
			t.Fatalf("installed Skill leaked as provider tool source=%q wire=%q", entry.SourceName, entry.WireName)
		}
	}
	if !preview.WithinBudget() || preview.WireBytes > llm.ProviderToolCatalogBudgetBytes {
		t.Fatalf("generic provider catalog exceeded budget: %+v", preview)
	}
}
