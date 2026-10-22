package control

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/verification"
)

func TestStandaloneVerificationReplacementWithPlan(t *testing.T) {
	for _, withPlan := range []bool{false, true} {
		name := "without-plan"
		if withPlan {
			name = "with-plan"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, i, _, r := newRecoveryFixture(t)
			var stepID string
			if withPlan {
				p, err := s.SyncRunPlan(ctx, i.TenantID, r.ID, "work", []RunPlanStepInput{{Step: "Check requested target", Status: "in_progress", SuccessCriteria: "ready", VerificationRequired: true}})
				if err != nil {
					t.Fatal(err)
				}
				stepID = p.Plan.Steps[0].StepID
			}
			none := []string{}
			b, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, verification.Binding{Version: 2, Criterion: "unrelated service healthy", Target: "remote-b", LocalDependencies: &none}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"evidence": map[string]any{"kind": "verification", "tool_call_id": "standalone-fail", "status": "failed", "started_at_unix_nano": 10, "finished_at_unix_nano": 11, "command": map[string]any{"command": "read status", "cwd": "/workspace", "binding": b}}})
			if _, err = s.AppendEvent(ctx, Event{RunID: r.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			next, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, verification.Binding{Replaces: "standalone-fail", Reason: "correct reader for the same condition and target"}, "/workspace")
			if err != nil || next == nil || next.StepID != "" || next.Target != b.Target || next.Criterion != b.Criterion {
				t.Fatalf("standalone replacement=%+v err=%v", next, err)
			}
			for _, bad := range []verification.Binding{
				{Replaces: "standalone-fail", Reason: "retry", Target: "another-service"},
				{Replaces: "standalone-fail", Reason: "retry", Criterion: "weaker-condition"},
				{Replaces: "foreign-check", Reason: "retry"},
			} {
				if _, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, bad, "/workspace"); err == nil {
					t.Fatalf("invalid replacement accepted: %+v", bad)
				}
			}
			if _, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, verification.Binding{Replaces: "standalone-fail", Reason: "retry"}, "/other-workspace"); err == nil {
				t.Fatal("different cwd accepted")
			}
			if withPlan {
				selected, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, verification.Binding{StepID: stepID, Replaces: "standalone-fail", Reason: "Main explicitly associates this observation with the chosen step"}, "/workspace")
				if err != nil || selected == nil || selected.StepID != stepID || selected.Version != 3 {
					t.Fatalf("explicit step selection=%+v err=%v", selected, err)
				}
			}
		})
	}
}

func TestFrozenSourceCannotUseLaterWorkUnitCheck(t *testing.T) {
	for _, tc := range []struct {
		name, path                                                                string
		lateFailure, legacyReference, secondCheck, legacyBinding, malformedWindow bool
		allow                                                                     bool
	}{
		{name: "changed-original-input", path: "/workspace/input-A"},
		{name: "unrelated-write", path: "/workspace/report", allow: true},
		{name: "later-failure-outside-window", lateFailure: true, allow: true},
		{name: "legacy-out-of-window-reference", legacyReference: true},
		{name: "every-effective-input-must-stay-fresh", path: "/workspace/input-A", secondCheck: true},
		{name: "legacy-binding-in-window", legacyBinding: true, allow: true},
		{name: "malformed-window-cannot-inherit", malformedWindow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, i, task, r := newRecoveryFixture(t)
			p, err := s.SyncRunPlan(ctx, i.TenantID, r.ID, "work", []RunPlanStepInput{{Step: "Check target A", Status: "in_progress", SuccessCriteria: "target A is ready", VerificationRequired: true}, {Step: "Independent report", Status: "pending", SuccessCriteria: "report exists", WorkUnit: true}})
			if err != nil {
				t.Fatal(err)
			}
			id := p.Plan.Steps[0].StepID
			deps := []string{"/workspace/input-A"}
			b, err := s.ResolveVerificationBinding(ctx, i.TenantID, r.ID, verification.Binding{StepID: id, Target: "target-A", LocalDependencies: &deps}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacyBinding {
				b.Version, b.StepID, b.Target, b.LocalDependencies = 1, "", id, nil
			}
			appendPlanEvidence(t, s, r.ID, "original-A", b, 10)
			late := *b
			none := []string{}
			late.Criterion = "target B is ready"
			late.Target = "target-B"
			late.LocalDependencies = &none
			if tc.secondCheck {
				appendPlanEvidence(t, s, r.ID, "second-effective-B", &late, 15)
			}
			if _, err = s.SyncRunPlan(ctx, i.TenantID, r.ID, "next", []RunPlanStepInput{{StepID: id, Status: "completed"}, {StepID: p.Plan.Steps[1].StepID, Status: "in_progress"}}); err != nil {
				t.Fatal(err)
			}
			if tc.path != "" {
				raw, _ := json.Marshal(map[string]any{"evidence": map[string]any{"kind": "mutation", "finished_at_unix_nano": 20, "files": []map[string]string{{"path": tc.path, "before_sha256": "a", "after_sha256": "b"}}}})
				if _, err = s.AppendEvent(ctx, Event{RunID: r.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			appendPlanEvidence(t, s, r.ID, "late-B", &late, 30)
			if tc.lateFailure {
				if _, err = s.db.ExecContext(ctx, `UPDATE task_events SET payload_json=json_set(payload_json,'$.evidence.status','failed') WHERE run_id=? AND json_extract(payload_json,'$.evidence.tool_call_id')='late-B'`, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.malformedWindow {
				if _, err = s.db.ExecContext(ctx, `UPDATE task_events SET payload_json=json_remove(payload_json,'$.evidence.command') WHERE run_id=? AND json_extract(payload_json,'$.evidence.tool_call_id')='original-A'`, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.FinishRun(ctx, i.TenantID, r.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			child, err := s.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: r.ID})
			if err != nil {
				t.Fatal(err)
			}
			refs, err := s.ListInheritedPlanEvidence(ctx, i.TenantID, child.ID)
			if err != nil || len(refs) != 1 {
				t.Fatalf("refs=%+v err=%v", refs, err)
			}
			wantID := "original-A"
			if tc.secondCheck {
				wantID = "second-effective-B"
			}
			if tc.malformedWindow {
				wantID = ""
				if refs[0].ReuseBlockedReason == "" {
					t.Fatal("malformed source window has no blocker")
				}
			}
			if refs[0].CheckID != wantID {
				t.Fatalf("outside-window observation selected: %+v", refs[0])
			}
			if tc.legacyReference {
				// Simulate the previous release's derived journal reference. It must not
				// gain authority merely because the old binary persisted it.
				old := refs[0]
				old.CheckID = "late-B"
				old.Target = late.Target
				old.ObservedCriterion = late.Criterion
				raw, _ := json.Marshal(inheritedEvidenceReference{Version: 1, ResumeSourceRunID: r.ID, Items: []InheritedPlanEvidence{old}})
				if _, err = s.db.ExecContext(ctx, `UPDATE task_events SET payload_json=? WHERE run_id=? AND type='plan.evidence_inherited'`, string(raw), child.ID); err != nil {
					t.Fatal(err)
				}
				refs, err = s.ListInheritedPlanEvidence(ctx, i.TenantID, child.ID)
				if err != nil || len(refs) != 1 || refs[0].ReuseBlockedReason == "" {
					t.Fatalf("invalid legacy reference=%+v err=%v", refs, err)
				}
			}
			before, err := s.LatestRunPlan(ctx, i.TenantID, child.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.SyncRunPlan(ctx, i.TenantID, child.ID, "adopt unchanged A", []RunPlanStepInput{{StepID: id, Status: "completed", ReusePriorVerification: true, ReuseReason: "same target A"}, {StepID: p.Plan.Steps[1].StepID, Status: "in_progress"}})
			if (err == nil) != tc.allow {
				t.Fatalf("reuse allowed=%v want=%v err=%v refs=%+v", err == nil, tc.allow, err, refs)
			}
			if !tc.allow {
				after, err := s.LatestRunPlan(ctx, i.TenantID, child.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Version != before.Version || after.Steps[0].Status != before.Steps[0].Status {
					t.Fatal("rejected reuse changed Plan state")
				}
			}
		})
	}
}
