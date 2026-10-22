package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEfficiencyPreservesFailuresAndRunBoundaries(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"case_started","case_id":"a"}`,
		`{"type":"provider.call.usage","run_id":"one","metadata":{"role":"coding_agent","duration_ms":100,"output_tokens":12,"status":"failed"}}`,
		`{"type":"tool_failed","run_id":"one","tool":"finish_run","tool_call_id":"bad","ok":false}`,
		`{"type":"provider.call.usage","run_id":"two","metadata":{"role":"coding_agent","duration_ms":300}}`,
		`{"type":"provider.call.usage","run_id":"one","metadata":{"role":"coding_agent","duration_ms":200}}`,
		`{"type":"tool_started","run_id":"one","tool":"finish_run","tool_call_id":"corrected"}`,
		`{"type":"tool_finished","run_id":"one","tool":"finish_run","tool_call_id":"corrected","ok":true}`,
		`{"type":"tool_started","run_id":"two","tool":"finish_run","tool_call_id":"first"}`,
		`{"type":"tool_finished","run_id":"two","tool":"finish_run","tool_call_id":"first","ok":true}`,
		`{"type":"provider.call.usage","metadata":{"role":"fast_classifier","duration_ms":999}}`,
		`{"type":"provider.call.usage","metadata":{"role":"coding_agent","duration_ms":500,"provider_dispatched":false,"status":"deferred"}}`,
		`{"type":"tool_started","tool":"verify","tool_call_id":"v"}`,
	}, "\n")
	r, err := ReadReport("inline", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	e := r.Efficiency
	if e.MainCalls != 3 || e.MainMS != 600 || e.OtherRoleCalls != 1 || e.MainOutputTokens != 12 {
		t.Fatalf("usage: %+v", e)
	}
	if e.LifecycleCalls != 3 || e.VerifyCalls != 1 || e.FinishAttempts != 3 {
		t.Fatalf("tools: %+v", e)
	}
	if e.FinishRuns != 2 || e.FinishFirstPassRuns != 1 || e.ClosingModelMS != 600 || e.ClosingModelCalls != 3 {
		t.Fatalf("closing: %+v", e)
	}
}

func TestLegacyMetricsRemainUnknown(t *testing.T) {
	r, err := ReadReport("old", strings.NewReader(`{"type":"case_finished","status":"passed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Efficiency.UsageAvailable || r.Efficiency.ClosingAvailable {
		t.Fatalf("fabricated metrics: %+v", r.Efficiency)
	}
}

func TestAggregateSamplesKeepsCostsAndFailures(t *testing.T) {
	s := []RunResult{{CaseID: "x", Status: "failed", Duration: 10 * time.Second, ToolErrors: 2, OutputTokens: 100},
		{CaseID: "x", Status: "error", Duration: 20 * time.Second, Error: "startup"},
		{CaseID: "x", Status: "passed", Duration: time.Second, OutputTokens: 5}}
	r := aggregateSamples("x", s, 1)
	if r.Status != "failed" || len(r.Samples) != 3 || r.Duration != 31*time.Second || r.ToolErrors != 2 || r.OutputTokens != 105 {
		t.Fatalf("aggregate: %+v", r)
	}
	if medianDuration(r.Samples) != 10*time.Second {
		t.Fatal("last successful attempt masked prior failures")
	}
}

func TestRepeatedRecordingsRetainAllAttempts(t *testing.T) {
	if caseSessionID("case", 0, 3, true) == caseSessionID("case", 1, 3, true) {
		t.Fatal("repeated recording overwrites earlier attempt")
	}
	if caseSessionID("case", 2, 3, false) != "case" || caseSessionID("case", 0, 1, true) != "case" {
		t.Fatal("canonical replay or single record changed")
	}
}

func TestRepeatedCaseReturnsEverySampleThroughRunner(t *testing.T) {
	_, cfg := writeRunnerFixtures(t)
	c := writeRunnerCase(t, "sample_controls", "model_required: false\nworkspace: isolated\nrepeat: 3\n")
	c.Turns[0].Input = "/status"
	r, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfg, OutputPath: filepath.Join(t.TempDir(), "run.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Samples) != 3 {
		t.Fatalf("only last sample returned: %+v", r)
	}
	var duration time.Duration
	for _, sample := range r.Samples {
		duration += sample.Duration
		if _, err := os.Stat(sample.OutputPath); err != nil {
			t.Fatal(err)
		}
		set, err := LoadResultSet(sample.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(set.Samples) != 1 || set.Samples[0].Provenance == nil || set.Samples[0].Provenance.Mode != "replay" {
			t.Fatalf("provenance: %+v", set)
		}
	}
	if r.Duration != duration {
		t.Fatal("sample costs lost")
	}
}

func TestRunnerClosingMetricsUseGatewayRunIdentity(t *testing.T) {
	_, cfg := writeRunnerFixtures(t)
	c, err := LoadCase("../../evalcases/quality/atomic-plan-completion.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../.vcr")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", root)
	r, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfg, OutputPath: filepath.Join(t.TempDir(), "run.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Efficiency.FinishRuns != 1 || r.Efficiency.FinishFirstPassRuns != 1 || !r.Efficiency.ClosingAvailable {
		t.Fatalf("scoped finish metrics missing on real path: %+v", r.Efficiency)
	}
}

func TestCompareRefusesDifferentModesAndRetainsIncomplete(t *testing.T) {
	p := RunProvenance{Mode: "live", Provider: "route", Model: "model", CaseHash: "fixture", PolicyHash: "policy"}
	a := RunResult{CaseID: "x", Status: "passed", Duration: 10 * time.Second, Provenance: &p}
	b := a
	b.Duration = time.Second
	before, after := ResultSet{Version: 1, Samples: []RunResult{a}}, ResultSet{Version: 1, Samples: []RunResult{b}}
	if got := CompareResultSets(before, after); !strings.Contains(got, "-90.0%") {
		t.Fatal(got)
	}
	for _, change := range []func(*RunProvenance){func(p *RunProvenance) { p.Mode = "replay" }, func(p *RunProvenance) { p.Model = "different" }, func(p *RunProvenance) { p.PolicyHash = "other" }} {
		other := p
		change(&other)
		after.Samples[0].Provenance = &other
		if got := CompareResultSets(before, after); strings.Contains(got, "-90.0%") {
			t.Fatalf("false speed claim: %s", got)
		}
	}
	set, err := readJSONLSamples("unfinished", []byte(`{"type":"case_started","case_id":"lost"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Samples) != 1 || set.Samples[0].Status != "incomplete" {
		t.Fatalf("lost sample: %+v", set)
	}
	path := filepath.Join(t.TempDir(), "set.json")
	data, _ := json.Marshal(before)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadResultSet(path)
	if err != nil || len(loaded.Samples) != 1 {
		t.Fatalf("roundtrip: %v %+v", err, loaded)
	}
}
