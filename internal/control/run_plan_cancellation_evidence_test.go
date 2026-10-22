package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCancellationReasonAloneCannotDiscardRequiredWork(t *testing.T) {
	for _, reason := range []string{"Execution admission refused the observation", "当前执行环境不支持该检查", "The service is unavailable; continue with an offline preparation"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "inspect current state", []RunPlanStepInput{
				{Step: "Observe current load", Status: "in_progress", SuccessCriteria: "Current target load is observed"},
				{Step: "Read independent notes", Status: "pending"},
				{Step: "Prepare collection commands", Status: "pending"},
			})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "continue independent preparation", []RunPlanStepInput{
				{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: reason},
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
				{StepID: first.Plan.Steps[2].StepID, Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if updated.Plan.Steps[0].Status != "pending" || len(updated.CancellationDeferred) != 1 {
				t.Fatalf("an unsupported scope judgment erased necessary work: %+v", updated.Plan.Steps[0])
			}
			if updated.Plan.Steps[1].Status != "completed" || updated.Plan.Steps[2].Status != "in_progress" {
				t.Fatal("preserving the obligation blocked independent work")
			}
			// Omission in a later full snapshot also cannot discard it.
			updated, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish preparation", []RunPlanStepInput{
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
				{StepID: first.Plan.Steps[2].StepID, Status: "completed"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.Plan.Steps) != 3 || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) == nil {
				t.Fatal("an omitted unresolved cancellation became a completed Run")
			}
		})
	}
}

func TestCancellationReplacementRetainsOriginalCriterionAndVerification(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	criterion := "All output rows are present in the delivered artifact"
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "alternative checks", []RunPlanStepInput{
		{Step: "Check the artifact via the original route", Status: "pending", SuccessCriteria: criterion, VerificationRequired: true},
		{Step: "Check the same artifact via an available route", Status: "in_progress", SuccessCriteria: criterion, VerificationRequired: true, WorkUnit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The alternative checks the same result", ReplacementStepID: first.Plan.Steps[1].StepID},
		{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
	}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "covered", input); err == nil {
		t.Fatal("an unverified replacement bypassed the original required check")
	}
	before, _ := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if before.Version != first.Plan.Version || before.Steps[0].Status != "pending" {
		t.Fatal("a rejected replacement committed a partial Plan")
	}
	evidence := mustCancellationJSON(t, map[string]interface{}{"evidence": map[string]interface{}{
		"tool_call_id": "replacement-check", "kind": "verification", "status": "succeeded", "started_at_unix_nano": 10, "finished_at_unix_nano": 20,
		"command": map[string]interface{}{"command": "inspect artifact", "cwd": "/workspace", "binding": map[string]interface{}{"version": 3, "step_id": first.Plan.Steps[1].StepID, "criterion": criterion, "target": first.Plan.Steps[1].StepID}},
	}})
	if _, err = store.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: evidence}); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "covered", input)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Plan.Steps[0].Status != "cancelled" || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil {
		t.Fatal("a verified replacement could not settle a redundant step")
	}
	// A later status-only update retains the exact evidence link.
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: input[0].StepID, Status: "cancelled"}, {StepID: input[1].StepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if latest.Steps[0].ReplacementStepID != first.Plan.Steps[1].StepID {
		t.Fatal("replacement provenance was erased")
	}
	if err = store.FinishRun(ctx, identity.TenantID, run.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "Confirm the same result", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || inherited.Steps[0].Status != "pending" || inherited.Steps[1].Status != "in_progress" {
		t.Fatalf("inherited cancellation bypassed verification reassessment: %+v err=%v", inherited, err)
	}
	input[1].ReusePriorVerification = true
	input[1].ReuseReason = "The unchanged artifact still satisfies the original criterion"
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, child.ID, "Reuse the exact successful prior check", input); err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err != nil {
		t.Fatal("exact continuation could not reuse a verified alternative:", err)
	}
}

func TestUnrelatedOrWeakenedReplacementDoesNotCloseTheGoal(t *testing.T) {
	for _, target := range []string{"other step", "self", "unknown"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{Step: "Read live queue depth", Status: "pending", SuccessCriteria: "Current live queue depth is observed"},
				{Step: "Read an offline snapshot", Status: "completed", SuccessCriteria: "An old snapshot exists"},
			})
			if err != nil {
				t.Fatal(err)
			}
			ref := first.Plan.Steps[1].StepID
			if target == "self" {
				ref = first.Plan.Steps[0].StepID
			}
			if target == "unknown" {
				ref = "another-runs-step"
			}
			changed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", SuccessCriteria: "An old snapshot exists", CancellationDisposition: "not_required", CancellationReason: "The local snapshot is enough", ReplacementStepID: ref},
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if changed.Plan.Steps[0].Status != "pending" || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) == nil {
				t.Fatal("replacement lowered the original acceptance bar")
			}
		})
	}
}

// A step planned without success_criteria has no bar a replacement could
// repeat, so it could never close. A completed replacement that the
// cancellation references and that states its own criterion covers it: the
// runtime checks the reference, Main judges that the criterion covers the goal.
func TestCriteriaLessStepClosesThroughAStatedCompletedReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, original, alternative, criterion, status, ref string
		closed                                              bool
		reason                                              string
	}{
		{"stated completed replacement", "Check the old dashboard", "Check the current dashboard", "The current dashboard shows queue depth 7", "completed", "replacement", true, ""},
		{"reworded target", "Ask the vendor portal for the invoice", "Download the invoice from the shared drive", "invoice-0425.pdf is saved under reports/", "completed", "replacement", true, ""},
		{"replacement states no criterion", "Check the old dashboard", "Check the current dashboard", "", "completed", "replacement", false, CancellationDeferredCriterionUnstated},
		{"replacement still open", "Check the old dashboard", "Check the current dashboard", "The current dashboard shows queue depth 7", "in_progress", "replacement", false, CancellationDeferredReplacementOpen},
		{"self reference", "Check the old dashboard", "Check the current dashboard", "The current dashboard shows queue depth 7", "completed", "self", false, CancellationDeferredReplacementUnknown},
		{"unknown reference", "Check the old dashboard", "Check the current dashboard", "The current dashboard shows queue depth 7", "completed", "another-runs-step", false, CancellationDeferredReplacementUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{Step: tc.original, Status: "pending"},
				{Step: tc.alternative, Status: "in_progress", SuccessCriteria: tc.criterion, WorkUnit: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			ref := first.Plan.Steps[1].StepID
			switch tc.ref {
			case "self":
				ref = first.Plan.Steps[0].StepID
			case "another-runs-step":
				ref = tc.ref
			}
			changed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The alternative observes the same result", ReplacementStepID: ref},
				{StepID: first.Plan.Steps[1].StepID, Status: tc.status},
			})
			if err != nil {
				t.Fatal(err)
			}
			completion := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID)
			if tc.closed {
				if changed.Plan.Steps[0].Status != "cancelled" || len(changed.CancellationDeferred) != 0 || completion != nil {
					t.Fatalf("a referenced completed replacement could not settle a criteria-less step: %+v %v", changed.Plan.Steps[0], completion)
				}
				return
			}
			if changed.Plan.Steps[0].Status != "pending" || len(changed.CancellationDeferred) != 1 || changed.CancellationDeferred[0].Reason != tc.reason {
				t.Fatalf("deferral = %+v, want one %s", changed.CancellationDeferred, tc.reason)
			}
			if gap := changed.CancellationDeferred[0]; gap.Step.StepID != first.Plan.Steps[0].StepID || gap.Criterion != "" || gap.ReplacementStepID != ref {
				t.Fatalf("deferral lost the facts it names: %+v", gap)
			}
			if tc.status == "completed" && tc.ref == "replacement" && store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) == nil {
				t.Fatal("an unstated replacement completed the Run")
			}
		})
	}
}

// Once a snapshot gives a criteria-less step a criterion, that is the bar a
// replacement must keep.
func TestLaterEstablishedCriterionGovernsReplacement(t *testing.T) {
	for _, replacementCriterion := range []string{"A stale snapshot exists", "Current live queue depth is observed"} {
		ctx := context.Background()
		store, identity, _, run := newRecoveryFixture(t)
		first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
			{Step: "Read live queue depth", Status: "pending"},
			{Step: "Read the depth through the replica", Status: "in_progress", SuccessCriteria: replacementCriterion, WorkUnit: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
			{StepID: first.Plan.Steps[0].StepID, Status: "pending", SuccessCriteria: "Current live queue depth is observed"},
			{StepID: first.Plan.Steps[1].StepID, Status: "in_progress"},
		}); err != nil {
			t.Fatal(err)
		}
		changed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
			{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The replica reports the same depth", ReplacementStepID: first.Plan.Steps[1].StepID},
			{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
		})
		if err != nil {
			t.Fatal(err)
		}
		matches := replacementCriterion == "Current live queue depth is observed"
		if (changed.Plan.Steps[0].Status == "cancelled") != matches {
			t.Fatalf("replacement %q settled=%v, want %v", replacementCriterion, changed.Plan.Steps[0].Status == "cancelled", matches)
		}
		if !matches && (len(changed.CancellationDeferred) != 1 || changed.CancellationDeferred[0].Reason != CancellationDeferredCriterionChanged ||
			changed.CancellationDeferred[0].Criterion != "Current live queue depth is observed") {
			t.Fatalf("deferral did not name the established bar: %+v", changed.CancellationDeferred)
		}
	}
}

// A complete instruction is often short. A quote of one whole user message,
// or one whole sentence of it, is evidence however brief; only a partial span
// must clear the length floor. Both quote paths share the rule.
func TestCancellationQuotesAcceptWholeShortInstructions(t *testing.T) {
	for _, tc := range []struct {
		input, quote string
		ok           bool
	}{
		{"不用了", "不用了", true},
		{"我来", "我来", true},
		{"skip it", "skip it", true},
		{"算了", "算了", true},
		{"我来做", "我来做", true},
		{"no need", "no need", true},
		{"I'll do it", "I'll do it", true},
		{"  不用了。 ", "不用了", true},
		// A whole sentence of a longer message.
		{"Thanks for the summary. No need.", "No need", true},
		{"Thanks for the summary. No need.", "No need.", true},
		{"算了。我来做。", "我来做", true},
		{"Looks fine!\nI'll do it", "I'll do it", true},
		// A whole message may trail off.
		{"不用了……", "不用了", true},
		{"不用了~", "不用了", true},
		{"不用了～", "不用了", true},
		// A partial span still needs the floor, and a clause is not a sentence.
		{"I'll do it later today", "I'll do it", false},
		{"no need to rush", "no need", false},
		{"算了，我来做", "我来做", false},
		{"skip it for now", "skip it", false},
		{"v1.2 is fine", "2 is fine", false},
		// Enumerators, abbreviations and code are not sentence boundaries.
		{"1. build 2. test 3. deploy", "deploy", false},
		{"Run the suite, e.g. tests", "tests", false},
		{"Run this:\n```\nmake\n```", "make", false},
		{"Run this:\n```\nmake\n```", "```", false},
		// Source validation is independent of Main's interpretation. Polite
		// requests and factual questions both offer their actual text; neither
		// changes a plan without a structured cancellation verdict from Main.
		{"No need?", "No need", true},
		{"No need?", "No need?", true},
		{"Could you stop the rollout?", "Could you stop the rollout?", true},
		{"停止后续部署，可以吗？", "停止后续部署，可以吗？", true},
		{"Should I skip the staging deploy?", "skip the staging deploy", true},
		{"Why is it slow? Skip the tests.", "Skip the tests", true},
		// A question mark that opens a URL query neither asks nor ends the
		// sentence, so its tail is no whole sentence below the length floor.
		{"Skip job?id=5 now.", "id=5 now", false},
		{"Open page?tab=2 now.", "tab=2 now", false},
		{"Skip job?id=5 now.", "Skip job?id=5 now", true},
		{"跳过任务?编号=5", "编号=5", false},
		// A mark inside a URL opens its query whatever follows it, so CJK
		// text after it does not become a whole sentence either.
		{"打开https://baidu.com/s?百度", "百度", false},
		{"打开https://baidu.com/s?百度", "打开https://baidu.com/s?百度", true},
		// They also close it, so the next sentence stands whole.
		{"(do we still need the backfill?) Ship it.", "Ship it", true},
		{"『这个检查还需要吗？』好的。", "好的", true},
		// A whole message retains its exact text regardless of punctuation.
		{"Why is it slow? Skip the tests.", "Why is it slow? Skip the tests", true},
		{"Why is it slow? Skip the tests.", "Skip the tests", true},
		{"为什么这么慢？先跳过测试。", "为什么这么慢？先跳过测试", true},
		{"为什么这么慢？先跳过测试。", "先跳过测试", true},
		// Every code block style is code, not a sentence.
		{"Run this:\n~~~\nmake\n~~~", "make", false},
		{"Run this:\n\n    make\n", "make", false},
		{"Run this:\nmake", "make", true},
	} {
		for _, path := range []string{"scope_change_quote", "user_takeover_quote"} {
			t.Run(path+"/"+tc.input+"/"+tc.quote, func(t *testing.T) {
				ctx := context.Background()
				store, identity, task, run := newRecoveryFixture(t)
				m, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: tc.input})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.db.Exec(`UPDATE steering_mailbox SET status='consumed' WHERE id=?`, m.ID); err != nil {
					t.Fatal(err)
				}
				step := RunPlanStepInput{Step: "Check the staging service", Status: "cancelled", CancellationReason: "The person changed what this Run covers"}
				if path == "scope_change_quote" {
					step.CancellationDisposition, step.ScopeChangeQuote = "not_required", tc.quote
				} else {
					step.CancellationDisposition, step.UserTakeoverQuote = "user_takeover", tc.quote
				}
				_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{step})
				if (err == nil) != tc.ok {
					t.Fatalf("quote %q of %q accepted=%v, want %v (err=%v)", tc.quote, tc.input, err == nil, tc.ok, err)
				}
				if tc.ok && store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil {
					t.Fatal("an accepted whole instruction left the Run unable to finish")
				}
				if !tc.ok && !strings.Contains(err.Error(), "whole user message") {
					t.Fatalf("rejection does not say how to quote a short instruction: %v", err)
				}
			})
		}
	}
}

// A Run's stored input summary is cut with "..." at a byte limit, so its last
// "sentence" may be a cut prefix. Only text known to be whole can supply a
// whole-sentence quote; the full text in the start event still can.
func TestTruncatedInputSummaryCannotSupplyAWholeSentence(t *testing.T) {
	prefix := strings.Repeat("x", 231) + ". "
	full := prefix + "No need to wait for staging."
	summary := full[:240] + "..."
	for _, tc := range []struct {
		name, quote string
		rawText     bool
		ok          bool
	}{
		{"cut tail of the summary", "No need", false, false},
		{"cut tail with its marker", "No need...", false, false},
		{"whole sentence from the full text", "No need to wait for staging", true, true},
		{"cut tail against the full text", "No need", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, initial := newRecoveryFixture(t)
			if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", summary)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rawText {
				payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{"snapshot": map[string]string{"source": "direct", "raw_user_text": full}}})
				if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Wait for staging", Status: "cancelled",
				CancellationDisposition: "not_required", CancellationReason: "The person dropped the wait", ScopeChangeQuote: tc.quote}})
			if (err == nil) != tc.ok {
				t.Fatalf("quote %q accepted=%v, want %v (err=%v)", tc.quote, err == nil, tc.ok, err)
			}
		})
	}
}

// Start-event text is bounded too, without a marker; the snapshot then says
// its evidence is incomplete. Its cut tail is no whole sentence, and a partial
// span may not end at the cut, while the text before the cut still counts.
func TestBoundedStartTextCannotSupplyItsCutTail(t *testing.T) {
	raw := "Skip the canary. " + strings.Repeat("y", 300) + ". Please skip the staging dep. No need"
	for _, tc := range []struct {
		name, quote string
		incomplete  bool
		ok          bool
	}{
		{"cut tail", "No need", true, false},
		{"complete last sentence", "No need", false, true},
		{"sentence before the cut", "Skip the canary", true, true},
		{"partial span inside the text", "Please skip the staging", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			snapshot := map[string]interface{}{"source": "direct", "raw_user_text": raw, "authorization_evidence_incomplete": tc.incomplete}
			payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{"snapshot": snapshot}})
			if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
				t.Fatal(err)
			}
			_, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Wait for staging", Status: "cancelled",
				CancellationDisposition: "not_required", CancellationReason: "The person dropped the wait", ScopeChangeQuote: tc.quote}})
			if (err == nil) != tc.ok {
				t.Fatalf("quote %q accepted=%v, want %v (err=%v)", tc.quote, err == nil, tc.ok, err)
			}
		})
	}
	// A partial span may not end where the text was cut.
	cut := "Skip the canary. Please skip the staging dep"
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{"snapshot": map[string]interface{}{"source": "direct", "raw_user_text": cut, "authorization_evidence_incomplete": true}}})
	if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Wait for staging", Status: "cancelled",
		CancellationDisposition: "not_required", CancellationReason: "The person dropped the wait", ScopeChangeQuote: "skip the staging dep"}}); err == nil {
		t.Fatal("a span ending at the cut was accepted")
	}
}

// A quote is evidence of an instruction only as a meaningful span of ONE
// actual user input. With the Run input "do the work", the quote "the" or
// "work" occurs by coincidence and must not settle a cancellation, through
// either quote path.
func TestCancellationQuotesMustBeMeaningfulSpansOfOneUserInput(t *testing.T) {
	steering := "also exclude the staging check; 只需要本地准备，不用检查线上状态"
	for _, tc := range []struct {
		quote string
		ok    bool
	}{
		{"the", false},
		{"work", false},
		{"the work", false},
		{"he work", false},
		{"do the wor", false},
		{"检查", false},
		// A span may not join two separate inputs.
		{"do the work also exclude", false},
		{"do the work", true},
		{"exclude the staging check", true},
		{"不用检查线上状态", true},
		{"只需要本地准备", true},
	} {
		for _, path := range []string{"scope_change_quote", "user_takeover_quote"} {
			t.Run(path+"/"+tc.quote, func(t *testing.T) {
				ctx := context.Background()
				store, identity, task, run := newRecoveryFixture(t)
				m, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: steering})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.db.Exec(`UPDATE steering_mailbox SET status='consumed' WHERE id=?`, m.ID); err != nil {
					t.Fatal(err)
				}
				step := RunPlanStepInput{Step: "Check the staging service", Status: "cancelled", CancellationReason: "The person changed what this Run covers"}
				if path == "scope_change_quote" {
					step.CancellationDisposition, step.ScopeChangeQuote = "not_required", tc.quote
				} else {
					step.CancellationDisposition, step.UserTakeoverQuote = "user_takeover", tc.quote
				}
				_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{step})
				if (err == nil) != tc.ok {
					t.Fatalf("quote %q accepted=%v, want %v (err=%v)", tc.quote, err == nil, tc.ok, err)
				}
				if !tc.ok && !strings.Contains(err.Error(), path) {
					t.Fatalf("rejection does not name %s: %v", path, err)
				}
				if tc.ok && store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil {
					t.Fatal("an accepted quote left the step unresolved")
				}
			})
		}
	}
}

func TestCancellationScopeQuoteIsGroundedInTheWorkLineage(t *testing.T) {
	for _, actual := range []bool{true, false} {
		ctx := context.Background()
		store, identity, _, run := newRecoveryFixture(t)
		quote := "I only need the local preparation now; exclude the live check"
		if actual {
			payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{"snapshot": map[string]string{"source": "direct", "raw_user_text": quote}}})
			if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Read live state", Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The user changed the scope to preparation", ScopeChangeQuote: quote}})
		if actual && (err != nil || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil) {
			t.Fatalf("actual scope change was rejected: %v", err)
		}
		if !actual && err == nil {
			t.Fatal("an invented scope quote conferred authority")
		}
	}
}
