package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestSkillDeliveryUsesContextProportionalBudgets(t *testing.T) {
	small := RuntimeContextBudgetForContextTokens(32768)
	large := RuntimeContextBudgetForContextTokens(128000)
	unknown := RuntimeContextBudgetForContextTokens(0)
	if small.SkillMainTokens != 983 || small.SkillMainBytes != small.SkillMainTokens*4 {
		t.Fatalf("small main budget = %+v", small)
	}
	if large.SkillMainTokens != 2048 || large.SkillMainBytes != 8*1024 || large.SkillCatalogBytes != 8000 {
		t.Fatalf("large budget = %+v", large)
	}
	if unknown.SkillMainTokens != 512 || unknown.SkillCatalogTokens != 512 || unknown.SkillMainBytes != 2048 || unknown.SkillCatalogBytes != 2048 {
		t.Fatalf("unknown context did not use conservative explicit fallback: %+v", unknown)
	}
	if small.TotalChars <= 8000 || large.TotalChars <= 8000 {
		t.Fatalf("runtime totals did not reserve a separate Skill slice: small=%d large=%d", small.TotalChars, large.TotalChars)
	}
}

func TestSkillMainDeliveryEnforcesTokenAndByteBudgets(t *testing.T) {
	content := "## Procedure\n" + strings.Repeat("界", 600)
	delivery := BuildSkillMainDeliveryWithinBudget(content, 4096, 300)
	if delivery.Mode != SkillDeliveryModePaged {
		t.Fatalf("token-heavy main should page despite fitting byte ceiling: %+v", delivery)
	}
	if len(delivery.Content) > 4096 || estimateTokens(delivery.Content) > 300 {
		t.Fatalf("paged index exceeded dual budgets: bytes=%d tokens=%d", len(delivery.Content), estimateTokens(delivery.Content))
	}
}

func TestSkillMainDeliveryRemovesFrontMatterAndPagesWithoutPrefixTruncation(t *testing.T) {
	content := "---\nname: deploy\ndescription: metadata only\n---\n\n## Applicability\n" + strings.Repeat("A", 700) + "\n## Procedure\n" + strings.Repeat("B", 700)
	full := BuildSkillMainDelivery(content, 2000)
	if full.Mode != SkillDeliveryModeFull || strings.Contains(full.Content, "description: metadata only") || !strings.Contains(full.Content, "## Procedure") {
		t.Fatalf("full delivery = %+v", full)
	}
	paged := BuildSkillMainDelivery(content, 512)
	if paged.Mode != SkillDeliveryModePaged || len(paged.Content) > 512 || strings.Contains(paged.Content, strings.Repeat("A", 200)) {
		t.Fatalf("paged delivery = %+v", paged)
	}
	if !strings.Contains(paged.Content, "skill_view") || len(paged.Sections) != 2 {
		t.Fatalf("paged index is not actionable: %+v", paged)
	}
}

func TestActiveSkillPromptV1CarriesOnlyModelRelevantIdentity(t *testing.T) {
	delivery := BuildSkillMainDelivery("## Procedure\nDo the bounded thing.", 4096)
	ctx := ActiveSkillContext{
		ActivationID: "activation-visible", WorkUnitID: "unit-hidden", Key: "key-hidden",
		Name: "visible-name", VersionHash: "version-hidden", Scope: "scope-hidden", Source: "source-hidden",
		DeliveryContractVersion: delivery.ContractVersion, DeliveryMode: delivery.Mode,
		DeliveredMain: delivery.Content, DeliveredHash: delivery.DeliveredHash, DeliveredBytes: delivery.DeliveredBytes,
	}
	prompt := ctx.Prompt(4096)
	if !strings.Contains(prompt, "activation-visible") || !strings.Contains(prompt, "visible-name") || !strings.Contains(prompt, delivery.Content) {
		t.Fatalf("prompt lost delivery: %q", prompt)
	}
	for _, hidden := range []string{"unit-hidden", "key-hidden", "version-hidden", "scope-hidden", "source-hidden"} {
		if strings.Contains(prompt, hidden) {
			t.Fatalf("prompt leaked control-plane identity %q: %q", hidden, prompt)
		}
	}
}

func TestSkillSectionPageReturnsOneExactSection(t *testing.T) {
	content := "---\nname: demo\n---\n\n# Demo\n## Inputs\nA\n## Procedure\nB\nC\n## Verification\nD"
	page, ok := SkillSectionPage(content, "procedure")
	if !ok || page != "## Procedure\nB\nC" {
		t.Fatalf("page=%q ok=%v", page, ok)
	}
}

func TestSkillDeliveryReceiptRejectsAnyByteDeviation(t *testing.T) {
	delivery := BuildSkillMainDelivery("## Procedure\nUse the pinned bytes.", 2048)
	if err := ValidateSkillMainDeliveryReceipt(delivery.ContractVersion, delivery.Mode,
		delivery.Content, delivery.DeliveredHash, delivery.DeliveredBytes); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	if err := ValidateSkillMainDeliveryReceipt(delivery.ContractVersion, delivery.Mode,
		delivery.Content+"x", delivery.DeliveredHash, delivery.DeliveredBytes); err == nil {
		t.Fatal("changed delivery bytes were accepted")
	}
}

func TestRuntimeBundleUsesItsFunctionDerivedSkillBudget(t *testing.T) {
	body := strings.Repeat("instruction ", 520)
	delivery := BuildSkillMainDelivery(body, 7000)
	bundle := RuntimeContextBundle{
		Budget: RuntimeContextBudgetForContextTokens(128000),
		ActiveSkill: &ActiveSkillContext{
			ActivationID: "activation-budget", Name: "budgeted-skill",
			DeliveryContractVersion: delivery.ContractVersion, DeliveryMode: delivery.Mode,
			DeliveredMain: delivery.Content, DeliveredHash: delivery.DeliveredHash, DeliveredBytes: delivery.DeliveredBytes,
		},
	}
	prompt := bundle.Prompt(bundle.Budget.TotalChars)
	if !strings.Contains(prompt, delivery.Content) {
		t.Fatalf("function-derived runtime budget truncated the fixed Skill main: len=%d budget=%d", len(prompt), bundle.Budget.TotalChars)
	}
}

func TestActiveSkillEnvelopeUsesTheSameLinkedFileBudgetAsDelivery(t *testing.T) {
	linked := []string{strings.Repeat("nested-path-", 80) + ".md"}
	budget := 2048
	delivery := BuildSkillMainDelivery(strings.Repeat("step ", 200), ActiveSkillDeliveryBodyBudget(budget, linked))
	skill := ActiveSkillContext{
		ActivationID: "activation-envelope", Name: "bounded-envelope", LinkedFiles: linked,
		DeliveryContractVersion: delivery.ContractVersion, DeliveryMode: delivery.Mode,
		DeliveredMain: delivery.Content, DeliveredHash: delivery.DeliveredHash, DeliveredBytes: delivery.DeliveredBytes,
	}
	if prompt := skill.Prompt(budget); len(prompt) > budget {
		t.Fatalf("linked-file envelope exceeded its slice: got=%d budget=%d", len(prompt), budget)
	}
}

func TestProviderBoundPreflightRejectsChangedProtectedSlice(t *testing.T) {
	delivery := BuildSkillMainDeliveryWithinBudget("## Procedure\nUse exact bytes.", 2048, 512)
	active := &ActiveSkillContext{
		ActivationID: "activation-preflight", Name: "exact-skill",
		DeliveryContractVersion: delivery.ContractVersion, DeliveryMode: delivery.Mode,
		DeliveredMain: delivery.Content, DeliveredHash: delivery.DeliveredHash, DeliveredBytes: delivery.DeliveredBytes,
	}
	bundle := RuntimeContextBundle{Budget: DefaultRuntimeContextBudget(), ActiveSkill: active}
	ctx := WithRuntimeContextBundle(context.Background(), bundle)
	prompt := active.Prompt(bundle.Budget.SkillMainBytes)
	if err := ensureActiveSkillProviderDelivery(ctx, []llm.Message{{Role: "system", Content: "prefix\n" + prompt}}); err != nil {
		t.Fatalf("exact provider-bound slice rejected: %v", err)
	}
	tampered := strings.Replace(prompt, "Use exact bytes.", "Use changed bytes.", 1)
	if err := ensureActiveSkillProviderDelivery(ctx, []llm.Message{{Role: "system", Content: tampered}}); err == nil {
		t.Fatal("changed protected slice reached provider preflight")
	}
}
