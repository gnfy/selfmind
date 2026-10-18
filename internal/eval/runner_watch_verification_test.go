package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRunnerUsesWatcherChildVerification(t *testing.T) {
	for _, childVerifies := range []bool{false, true} {
		t.Run(fmt.Sprintf("child_verification_%t", childVerifies), func(t *testing.T) {
			_, configPath := writeRunnerFixtures(t)
			t.Setenv("SELF_MEMORY_SEMANTIC_RECALL", "false")
			const id = "watch_child_verification_isolation"
			content := `id: watch_child_verification_isolation
title: "Watch child owns its final verification verdict"
channel: cli
workspace: isolated
turns:
  - input: "Run verify using printf READY, then register two ready observations in one watch group. After the handoff report READY."
    wait_for_external_watches: true
expect:
  verification_state: passed
  condition_state: met
`
			path := filepath.Join(t.TempDir(), "case.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadCase(path)
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
			watches := []map[string]interface{}{}
			for i, value := range []string{"READY_A", "READY_B"} {
				args, err := json.Marshal(map[string]interface{}{"command": "printf " + value, "success_pattern": value,
					"cwd": "{{SELFMIND_VCR_WORKSPACE}}", "wait_group": "prerequisites", "wait_group_mode": "all", "wait_group_size": 2,
					"interval_seconds": 5, "timeout_seconds": 60})
				if err != nil {
					t.Fatal(err)
				}
				watches = append(watches, map[string]interface{}{"ID": fmt.Sprintf("watch-%d", i), "Function": "watch_external", "Args": string(args)})
			}
			records := []map[string]interface{}{check, {"method": "stream", "events": []map[string]interface{}{{"tool_calls": watches}}}}
			if childVerifies {
				records = append(records, check, map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"tool_calls": []map[string]interface{}{{
					"ID": "finish", "Function": "finish_run", "Args": `{"status":"done","final_response":"READY","assessments":[{"result":"met","evidence_refs":["check"],"reason":"The child observed READY."}]}`,
				}}}}})
			} else {
				records = append(records, map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"content": "READY"}}})
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
					if check.OK != childVerifies {
						t.Fatalf("final verification assertion = %+v, watcher child verified = %t", check, childVerifies)
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
