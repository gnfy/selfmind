package kernel

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestContextWindowRecoveryPreservesActiveSkillReceiptExactly(t *testing.T) {
	delivery := BuildSkillMainDelivery("## Procedure\nVerify the exact pinned package.", 2048)
	skill := ActiveSkillContext{
		ActivationID: "activation-1", Name: "pinned-flow",
		DeliveryContractVersion: delivery.ContractVersion, DeliveryMode: delivery.Mode,
		DeliveredMain: delivery.Content, DeliveredHash: delivery.DeliveredHash, DeliveredBytes: delivery.DeliveredBytes,
	}
	block := skill.Prompt(4096)
	system := strings.Repeat("prefix context ", 800) + "\n" + block + strings.Repeat("suffix context ", 800)
	engine := NewContextEngine(1024, 128)
	got := engine.RecoverMessages([]llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: "latest request"},
	})
	if len(got) != 2 || got[0].Content == system {
		t.Fatalf("recovery did not reduce surrounding system context")
	}
	start := strings.Index(got[0].Content, activeSkillPromptBegin)
	end := strings.Index(got[0].Content, activeSkillPromptEnd)
	if start < 0 || end < start {
		t.Fatalf("recovery lost active Skill markers: %q", got[0].Content)
	}
	end += len(activeSkillPromptEnd)
	if gotBlock := got[0].Content[start:end] + "\n"; gotBlock != block {
		t.Fatalf("active Skill receipt changed during recovery:\nwant=%q\ngot=%q", block, gotBlock)
	}
}
