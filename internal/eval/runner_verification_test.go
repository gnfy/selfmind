package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// This scripted transport tests the runner's state isolation, not model quality.
// Release evidence still uses real provider recordings in evalcases.
func TestRunnerDoesNotCarryVerificationBetweenTurns(t *testing.T) {
	for _, verifyAgain := range []bool{false, true} {
		t.Run(fmt.Sprintf("second_turn_verification_%t", verifyAgain), func(t *testing.T) {
			_, configPath := writeRunnerFixtures(t)
			t.Setenv("SELF_MEMORY_SEMANTIC_RECALL", "false")
			const id = "verification_turn_isolation"
			content := `id: verification_turn_isolation
title: "Each turn owns its verification verdict"
channel: cli
workspace: isolated
turns:
  - input: "Use verify to run printf READY as a custom check, then report the check result."
  - input: "Explain what READY means."
expect:
  verification_state: passed
  condition_state: met
checks:
  no_empty_response: true
`
			if verifyAgain {
				content = `id: verification_turn_isolation
title: "Each turn owns its verification verdict"
channel: cli
workspace: isolated
turns:
  - input: "Use verify to run printf READY as a custom check, then report the check result."
  - input: "Use verify to run printf READY again, then report this new check result."
expect:
  verification_state: passed
  condition_state: met
checks:
  no_empty_response: true
`
			}
			casePath := filepath.Join(t.TempDir(), "case.yaml")
			if err := os.WriteFile(casePath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadCase(casePath)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(os.Getenv("SELFMIND_EVAL_VCR_DIR"), id)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			check := map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"tool_calls": []map[string]interface{}{{
				"ID": "check", "Function": "verify", "Args": `{"kind":"custom","command":"printf READY","cwd":"{{SELFMIND_VCR_WORKSPACE}}","criterion":"stdout is READY","target":"stdout"}`,
			}}}}}
			answer := map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"content": "READY"}}}
			finish := map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"tool_calls": []map[string]interface{}{{
				"ID": "finish", "Function": "finish_run", "Args": `{"status":"done","final_response":"READY","assessments":[{"result":"met","evidence_refs":["check"],"reason":"The observed output is READY."}]}`,
			}}}}}
			records := []map[string]interface{}{check, finish}
			if verifyAgain {
				records = append(records, check, finish)
			} else {
				records = append(records, answer)
			}
			for i, record := range records {
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.json", i)), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RunCase(context.Background(), c, RunOptions{ConfigPath: configPath, OutputPath: filepath.Join(t.TempDir(), "result.jsonl")})
			if err != nil {
				t.Fatal(err)
			}
			matched := 0
			for _, check := range result.Checks {
				if check.Name == "verification_state:passed" || check.Name == "condition_state:met" {
					if check.OK != verifyAgain {
						t.Fatalf("final verification assertion = %+v, second turn verified = %t", check, verifyAgain)
					}
					matched++
				}
			}
			if matched != 2 {
				t.Fatalf("missing independent condition/execution assertions: %+v", result)
			}
		})
	}
}
