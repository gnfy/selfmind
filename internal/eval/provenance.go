package eval

import (
	"encoding/json"
	"os"
	"strings"

	"selfmind/internal/buildinfo"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
)

// RunProvenance contains comparison identity, never credentials or prompt text.
// Builds identify evidence, but must differ in a before/after comparison.
type RunProvenance struct {
	Mode             string `json:"mode"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Reasoning        string `json:"reasoning"`
	ApprovalMode     string `json:"approval_mode"`
	CaseHash         string `json:"case_hash"`
	Build            string `json:"build"`
	PolicyHash       string `json:"policy_hash"`
	RecordingSession string `json:"recording_session,omitempty"`
	RecordingSources string `json:"recording_sources,omitempty"`
}

func newRunProvenance(c *Case, cfg *config.Config, provider, model string) *RunProvenance {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_EVAL_VCR")))
	if mode != "record" && mode != "replay" {
		mode = "live"
	}
	data, _ := json.Marshal(c)
	approvalMode := c.ApprovalMode
	if approvalMode == "" {
		approvalMode = "full-auto"
	}
	policy, _ := json.Marshal(struct {
		Models  config.ModelsConfig
		Sandbox config.ExecSandboxConfig
	}{cfg.Models, cfg.ExecSandbox})
	p := &RunProvenance{Mode: mode, Provider: provider, Model: model, Reasoning: cfg.Models.Primary.Reasoning,
		ApprovalMode: approvalMode, CaseHash: hashText(string(data)), Build: buildinfo.Fingerprint(), PolicyHash: hashText(string(policy))}
	if mode == "replay" {
		p.RecordingSources = llm.CassetteRecordingSources(c.ID)
	}
	return p
}
