package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func appendUserStart(t *testing.T, store *Store, run *Run, text string) *Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{
		"snapshot": map[string]string{"source": "direct", "raw_user_text": text}}})
	event, err := store.AppendEvent(context.Background(), Event{RunID: run.ID, Type: "run.started", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// Every recorded source keeps its shape: a known kind and a non-empty id
// without spaces or control characters, within the byte bound.
func TestCancellationQuoteSourceShape(t *testing.T) {
	longest := "run_started:" + strings.Repeat("r", cancellationQuoteSourceMaxBytes-len("run_started:"))
	for _, valid := range []string{"steering:steer_1", "run_input:run_1", " run_started:run_1 ", longest} {
		if _, err := normalizeCancellationQuoteSource(valid); err != nil {
			t.Errorf("%q refused: %v", valid, err)
		}
	}
	for _, malformed := range []string{"event:1", "steering:", "mailbox_7", "run_input:two words", "steering:a\x00b", longest + "r"} {
		if _, err := normalizeCancellationQuoteSource(malformed); err == nil {
			t.Errorf("%q accepted", malformed)
		}
	}
}

// Only a validated quote names an input: a cancellation that a completed
// replacement covers records no source.
func TestCancellationWithoutAQuoteRecordsNoSource(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
		{Step: "Check the old dashboard", Status: "pending", SuccessCriteria: "the dashboard is green"},
		{Step: "Check the new dashboard", Status: "in_progress", SuccessCriteria: "the dashboard is green"},
	})
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "replaced", []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required",
			CancellationReason: "the new dashboard covers it", ReplacementStepID: first.Plan.Steps[1].StepID},
		{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if step := replaced.Plan.Steps[0]; step.Status != "cancelled" || step.CancellationQuoteSource != "" {
		t.Fatalf("a replacement-backed cancellation = %s source %q", step.Status, step.CancellationQuoteSource)
	}
}

// consumeSteering records content as guidance a Run's model already took in.
func consumeSteering(t *testing.T, store *Store, identity *IdentityContext, task *Task, run *Run, content string) *SteeringMessage {
	t.Helper()
	m, err := store.AcceptSteering(context.Background(), SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE steering_mailbox SET status='consumed' WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

// recordedStep reads one step of a Run's latest plan and that plan's version.
func recordedStep(t *testing.T, store *Store, tenantID, runID, stepID string) (RunPlanStep, int) {
	t.Helper()
	plan, err := store.LatestRunPlan(context.Background(), tenantID, runID)
	if err != nil || plan == nil {
		t.Fatalf("plan for %s: %+v err=%v", runID, plan, err)
	}
	for _, step := range plan.Steps {
		if step.StepID == stepID {
			return step, plan.Version
		}
	}
	t.Fatalf("plan for %s has no step %s: %+v", runID, stepID, plan.Steps)
	return RunPlanStep{}, 0
}

// quotedCancellation cancels a step on the evidence of one quote, in the field
// its disposition reads.
func quotedCancellation(stepID, disposition, quote string) RunPlanStepInput {
	step := RunPlanStepInput{StepID: stepID, Status: "cancelled", CancellationDisposition: disposition,
		CancellationReason: "the person changed what this work covers"}
	if disposition == "user_takeover" {
		step.UserTakeoverQuote = quote
	} else {
		step.ScopeChangeQuote = quote
	}
	return step
}

// When the runtime validates a takeover or scope-change quote it records the
// exact input the quote matched: a consumed steering message, the text a Run
// started with, or a Run's input summary.
func TestValidatedQuoteRecordsTheInputItMatched(t *testing.T) {
	for _, tc := range []struct {
		name, request, started, steering, disposition, quote, want string
	}{
		{name: "takeover from consumed guidance", request: "rotate the keys and update the docs",
			steering: "I'll rotate the keys myself", disposition: "user_takeover", quote: "I'll rotate the keys myself", want: "steering"},
		{name: "takeover from guidance in CJK", request: "轮换密钥并更新文档",
			steering: "密钥我自己来轮换。文档继续改", disposition: "user_takeover", quote: "密钥我自己来轮换", want: "steering"},
		{name: "scope change from start-event text", request: "ship the release",
			started: "ship the release; skip the live check this time", disposition: "not_required", quote: "skip the live check this time", want: "run_started"},
		{name: "scope change from the run input", request: "ship the release; skip the live check this time",
			disposition: "not_required", quote: "skip the live check this time", want: "run_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, initial := newRecoveryFixture(t)
			if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", tc.request)
			if err != nil {
				t.Fatal(err)
			}
			want := "run_input:" + run.ID
			switch tc.want {
			case "steering":
				want = "steering:" + consumeSteering(t, store, identity, task, run, tc.steering).ID
			case "run_started":
				appendUserStart(t, store, run, tc.started)
				want = "run_started:" + run.ID
			}
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
				{Step: "Do the work the person changed", Status: "pending"},
				{Step: "Do the rest", Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			changed, rest := first.Plan.Steps[0].StepID, first.Plan.Steps[1].StepID
			if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "cancelled", []RunPlanStepInput{
				quotedCancellation(changed, tc.disposition, tc.quote), {StepID: rest, Status: "in_progress"},
			}); err != nil {
				t.Fatal(err)
			}
			if step, _ := recordedStep(t, store, identity.TenantID, run.ID, changed); step.Status != "cancelled" || step.CancellationQuoteSource != want {
				t.Fatalf("recorded source = %q (status %s), want %q", step.CancellationQuoteSource, step.Status, want)
			}
		})
	}
}

// When several inputs hold the quote, the runtime records the earliest on the
// lineage: an earlier Run's input before a later Run's, a Run's request before
// guidance it consumed, its start-event text before its input summary, and
// guidance in the order it arrived.
func TestQuoteSourceIsTheEarliestMatchingInput(t *testing.T) {
	ctx := context.Background()
	quote := "I will run the deploy myself"
	t.Run("an earlier run's request before a later run's guidance", func(t *testing.T) {
		store, identity, task, initial := newRecoveryFixture(t)
		if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
			t.Fatal(err)
		}
		parent, err := store.StartRun(ctx, task, "cli", quote+"; prepare the release notes")
		if err != nil {
			t.Fatal(err)
		}
		appendUserStart(t, store, parent, quote+"; prepare the release notes")
		plan, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "plan", []RunPlanStepInput{
			{Step: "Prepare the release notes", Status: "in_progress"}, {Step: "Run the deploy", Status: "pending"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
		if err != nil {
			t.Fatal(err)
		}
		consumeSteering(t, store, identity, task, child, quote)
		notes, deploy := plan.Plan.Steps[0].StepID, plan.Plan.Steps[1].StepID
		if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "handed over", []RunPlanStepInput{
			{StepID: notes, Status: "in_progress"}, quotedCancellation(deploy, "user_takeover", quote),
		}); err != nil {
			t.Fatal(err)
		}
		if step, _ := recordedStep(t, store, identity.TenantID, child.ID, deploy); step.CancellationQuoteSource != "run_started:"+parent.ID {
			t.Fatalf("source = %q, want the parent's start text run_started:%s", step.CancellationQuoteSource, parent.ID)
		}
	})
	t.Run("a run's request before its guidance", func(t *testing.T) {
		store, identity, task, initial := newRecoveryFixture(t)
		if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", "跳过线上检查这一次; ship the release")
		if err != nil {
			t.Fatal(err)
		}
		consumeSteering(t, store, identity, task, run, "跳过线上检查这一次")
		plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
			{Step: "Run the live check", Status: "pending"}, {Step: "Ship the release", Status: "in_progress"},
		})
		if err != nil {
			t.Fatal(err)
		}
		live := plan.Plan.Steps[0].StepID
		if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "scope changed", []RunPlanStepInput{
			quotedCancellation(live, "not_required", "跳过线上检查这一次"), {StepID: plan.Plan.Steps[1].StepID, Status: "in_progress"},
		}); err != nil {
			t.Fatal(err)
		}
		if step, _ := recordedStep(t, store, identity.TenantID, run.ID, live); step.CancellationQuoteSource != "run_input:"+run.ID {
			t.Fatalf("source = %q, want the run's own request run_input:%s", step.CancellationQuoteSource, run.ID)
		}
	})
	t.Run("guidance in the order it arrived", func(t *testing.T) {
		store, identity, task, run := newRecoveryFixture(t)
		later := consumeSteering(t, store, identity, task, run, quote)
		earlier := consumeSteering(t, store, identity, task, run, quote)
		// The second row arrived first; insertion order is not arrival order.
		if _, err := store.db.Exec(`UPDATE steering_mailbox SET created_at = created_at - 60 WHERE id = ?`, earlier.ID); err != nil {
			t.Fatal(err)
		}
		// Guidance the model never took in is no source, however early.
		unread, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: quote})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE steering_mailbox SET created_at = created_at - 600 WHERE id = ?`, unread.ID); err != nil {
			t.Fatal(err)
		}
		plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
			{Step: "Run the deploy", Status: "pending"}, {Step: "Write the notes", Status: "in_progress"},
		})
		if err != nil {
			t.Fatal(err)
		}
		deploy := plan.Plan.Steps[0].StepID
		if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "handed over", []RunPlanStepInput{
			quotedCancellation(deploy, "user_takeover", quote), {StepID: plan.Plan.Steps[1].StepID, Status: "in_progress"},
		}); err != nil {
			t.Fatal(err)
		}
		if step, _ := recordedStep(t, store, identity.TenantID, run.ID, deploy); step.CancellationQuoteSource != "steering:"+earlier.ID {
			t.Fatalf("source = %q, want the guidance that arrived first steering:%s (not %s)", step.CancellationQuoteSource, earlier.ID, later.ID)
		}
	})
}

// The source is runtime-owned: a value a caller supplies with the snapshot,
// such as a model's update_plan arguments decoded into the store input, is
// never stored, whether or not the runtime validates the step's quote.
func TestCallerSuppliedQuoteSourceIsNeverStored(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract int
		want     func(run *Run) string
	}{
		{"validated quote", CurrentRunRecoveryContractVersion, func(run *Run) string { return "run_input:" + run.ID }},
		{"first recovery contract, which validates nothing", RunRecoveryContractVersion, func(*Run) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, initial := newRecoveryFixture(t)
			if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "I will run the deploy myself. Write the notes.")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE runs SET recovery_contract_version = ? WHERE id = ?`, tc.contract, run.ID); err != nil {
				t.Fatal(err)
			}
			plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
				{Step: "Run the deploy", Status: "pending"}, {Step: "Write the notes", Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var input []RunPlanStepInput
			raw := fmt.Sprintf(`[{"step_id":%q,"status":"cancelled","cancellation_disposition":"user_takeover",
				"cancellation_reason":"the person deploys","user_takeover_quote":"I will run the deploy myself",
				"cancellation_quote_source":"steering:forged"},{"step_id":%q,"status":"in_progress"}]`,
				plan.Plan.Steps[0].StepID, plan.Plan.Steps[1].StepID)
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "handed over", input); err != nil {
				t.Fatal(err)
			}
			if step, _ := recordedStep(t, store, identity.TenantID, run.ID, plan.Plan.Steps[0].StepID); step.CancellationQuoteSource != tc.want(run) {
				t.Fatalf("stored source = %q, want %q", step.CancellationQuoteSource, tc.want(run))
			}
		})
	}
}

// Every restatement keeps the recorded source: restating the same quote keeps
// it, even after another input with the same words appears; a changed quote
// records the input it now matches; a step that is no longer cancelled drops
// it; an exact continuation inherits it with the step.
func TestRestatedQuoteKeepsItsRecordedSource(t *testing.T) {
	for _, disposition := range []string{"user_takeover", "not_required"} {
		t.Run(disposition, func(t *testing.T) {
			ctx := context.Background()
			store, identity, task, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
				{Step: "Rotate the keys", Status: "pending"}, {Step: "Update the docs", Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			rotate, docs := first.Plan.Steps[0].StepID, first.Plan.Steps[1].StepID
			sync := func(explanation string, steps ...RunPlanStepInput) {
				t.Helper()
				if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, explanation, steps); err != nil {
					t.Fatal(err)
				}
			}
			source := func() (string, int) {
				t.Helper()
				step, version := recordedStep(t, store, identity.TenantID, run.ID, rotate)
				return step.CancellationQuoteSource, version
			}
			const quote = "Leave the key rotation to me"
			original := consumeSteering(t, store, identity, task, run, quote)
			sync("handed over", quotedCancellation(rotate, disposition, quote), RunPlanStepInput{StepID: docs, Status: "in_progress"})
			recorded, version := source()
			if recorded != "steering:"+original.ID {
				t.Fatalf("recorded source = %q, want steering:%s", recorded, original.ID)
			}
			sync("restated", quotedCancellation(rotate, disposition, quote), RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, v := source(); got != recorded || v != version {
				t.Fatalf("a full restatement = %q v%d, want %q v%d (no change)", got, v, recorded, version)
			}
			sync("progress", RunPlanStepInput{StepID: rotate, Status: "cancelled"}, RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, v := source(); got != recorded || v != version {
				t.Fatalf("a status-only restatement = %q v%d, want %q v%d (no change)", got, v, recorded, version)
			}
			// The same words again, recorded as having arrived earlier, would now
			// match first; the restated quote still names the input it matched.
			repeated := consumeSteering(t, store, identity, task, run, quote)
			if _, err := store.db.Exec(`UPDATE steering_mailbox SET created_at = created_at - 600 WHERE id = ?`, repeated.ID); err != nil {
				t.Fatal(err)
			}
			sync("restated again", quotedCancellation(rotate, disposition, quote), RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, _ := source(); got != recorded {
				t.Fatalf("a restated quote was re-pointed: %q, want %q", got, recorded)
			}
			// A changed quote records the input it now matches.
			const changedQuote = "I am rotating the keys on my side"
			changed := consumeSteering(t, store, identity, task, run, changedQuote)
			sync("new words", quotedCancellation(rotate, disposition, changedQuote), RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, _ := source(); got != "steering:"+changed.ID {
				t.Fatalf("a changed quote = %q, want steering:%s", got, changed.ID)
			}
			// A step that is no longer cancelled drops it.
			sync("reopened", RunPlanStepInput{StepID: rotate, Status: "pending"}, RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, _ := source(); got != "" {
				t.Fatalf("a reopened step kept source %q", got)
			}
			sync("handed over again", quotedCancellation(rotate, disposition, changedQuote), RunPlanStepInput{StepID: docs, Status: "in_progress"})
			if got, _ := source(); got != "steering:"+changed.ID {
				t.Fatalf("a renewed cancellation = %q, want steering:%s", got, changed.ID)
			}
			// An exact continuation inherits the source with its quote.
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if step, _ := recordedStep(t, store, identity.TenantID, child.ID, rotate); step.Status != "cancelled" || step.CancellationQuoteSource != "steering:"+changed.ID {
				t.Fatalf("the continuation's step = %s source %q, want cancelled steering:%s", step.Status, step.CancellationQuoteSource, changed.ID)
			}
		})
	}
}

// A cancellation recorded before schema v26 has no source. It still loads and
// validates, restating it is no change, and a continuation inherits it as it
// was recorded: the upgrade invents no source, and restating a historical quote
// does not either. A new quote that live work validates does record one.
func TestCancellationsFromBeforeV26KeepNoSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	loadRowFixture(t, dir, releasedRowFixtures[2])
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const historical = "run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62"
	plan, err := store.LatestRunPlan(ctx, DefaultTenantID, historical)
	if err != nil || plan == nil {
		t.Fatalf("historical plan = %+v err=%v", plan, err)
	}
	cancelled := 0
	statusOnly := make([]RunPlanStepInput, 0, len(plan.Steps))
	restated := make([]RunPlanStepInput, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		if step.Status == "cancelled" {
			cancelled++
		}
		if step.CancellationQuoteSource != "" {
			t.Fatalf("historical step %s loaded with source %q", step.StepID, step.CancellationQuoteSource)
		}
		statusOnly = append(statusOnly, RunPlanStepInput{StepID: step.StepID, Status: step.Status})
		restated = append(restated, step.RunPlanStepInput)
	}
	if cancelled != 2 {
		t.Fatalf("fixture plan has %d cancelled steps, want a takeover and a scope change", cancelled)
	}
	for name, input := range map[string][]RunPlanStepInput{"status-only": statusOnly, "full": restated} {
		projection, err := store.SyncRunPlan(ctx, DefaultTenantID, historical, plan.Explanation, input)
		if err != nil {
			t.Fatalf("%s restatement of a historical plan: %v", name, err)
		}
		if projection.Changed || projection.Plan.Version != plan.Version {
			t.Fatalf("%s restatement of a historical plan changed it: v%d changed=%t", name, projection.Plan.Version, projection.Changed)
		}
		for _, step := range projection.Plan.Steps {
			if step.CancellationQuoteSource != "" {
				t.Fatalf("%s restatement gave historical step %s source %q", name, step.StepID, step.CancellationQuoteSource)
			}
		}
	}
	if err := store.ValidateRunCompletion(ctx, DefaultTenantID, historical); err != nil {
		t.Fatalf("historical cancellations no longer validate: %v", err)
	}
	run, err := store.GetRun(ctx, DefaultTenantID, historical)
	if err != nil || run == nil {
		t.Fatalf("historical run = %+v err=%v", run, err)
	}
	task, err := store.GetTask(ctx, DefaultTenantID, run.TaskID)
	if err != nil || task == nil {
		t.Fatalf("historical thread = %+v err=%v", task, err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: historical})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := store.LatestRunPlan(ctx, DefaultTenantID, child.ID)
	if err != nil || inherited == nil {
		t.Fatalf("inherited plan = %+v err=%v", inherited, err)
	}
	requoted := make([]RunPlanStepInput, 0, len(inherited.Steps))
	takeover := ""
	for _, step := range inherited.Steps {
		if step.CancellationQuoteSource != "" {
			t.Fatalf("a continuation gave historical step %s source %q", step.StepID, step.CancellationQuoteSource)
		}
		input := RunPlanStepInput{StepID: step.StepID, Status: step.Status}
		if step.CancellationDisposition == "user_takeover" {
			// Another whole sentence of the same historical request.
			takeover, input = step.StepID, quotedCancellation(step.StepID, "user_takeover", "skip the live check this time")
		}
		requoted = append(requoted, input)
	}
	if _, err := store.SyncRunPlan(ctx, DefaultTenantID, child.ID, "requoted", requoted); err != nil {
		t.Fatal(err)
	}
	if step, _ := recordedStep(t, store, DefaultTenantID, child.ID, takeover); step.CancellationQuoteSource != "run_input:"+historical {
		t.Fatalf("a new quote on a historical cancellation = %q, want run_input:%s", step.CancellationQuoteSource, historical)
	}
}

// Only the person's own inputs are evidence. Another person's consumed
// guidance on the same Run, or another person's Run on its continuation edge,
// never matches a quote, even when it holds the words first.
func TestQuoteSourceIgnoresAnotherPersonsInput(t *testing.T) {
	ctx := context.Background()
	const quote = "I will run the deploy myself"
	cancelDeploy := func(t *testing.T, store *Store, tenantID, runID string) (string, error) {
		t.Helper()
		plan, err := store.SyncRunPlan(ctx, tenantID, runID, "plan", []RunPlanStepInput{
			{Step: "Run the deploy", Status: "pending"}, {Step: "Write the notes", Status: "in_progress"},
		})
		if err != nil {
			t.Fatal(err)
		}
		deploy := plan.Plan.Steps[0].StepID
		_, err = store.SyncRunPlan(ctx, tenantID, runID, "handed over", []RunPlanStepInput{
			quotedCancellation(deploy, "user_takeover", quote), {StepID: plan.Plan.Steps[1].StepID, Status: "in_progress"},
		})
		return deploy, err
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		var precondition interface{ PlanCancellationPrecondition() bool }
		if err == nil || !errors.As(err, &precondition) {
			t.Fatalf("another person's words authorized a takeover: err=%v", err)
		}
	}
	t.Run("another person's guidance on the run", func(t *testing.T) {
		store, identity, task, run := newRecoveryFixture(t)
		other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "another-person", "Another Person")
		if err != nil || other.PersonID == identity.PersonID {
			t.Fatalf("other person = %+v err=%v", other, err)
		}
		foreign, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: other.PersonID, RunID: run.ID, TaskID: task.ID, Content: quote})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE steering_mailbox SET status='consumed', created_at = created_at - 600 WHERE id = ?`, foreign.ID); err != nil {
			t.Fatal(err)
		}
		_, err = cancelDeploy(t, store, identity.TenantID, run.ID)
		refused(t, err)
		// The person's own later guidance does authorize it, and is what the
		// step records.
		own := consumeSteering(t, store, identity, task, run, quote)
		deploy, err := cancelDeploy(t, store, identity.TenantID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if step, _ := recordedStep(t, store, identity.TenantID, run.ID, deploy); step.CancellationQuoteSource != "steering:"+own.ID {
			t.Fatalf("source = %q, want the person's own steering:%s (not %s)", step.CancellationQuoteSource, own.ID, foreign.ID)
		}
	})
	t.Run("another person's run on the continuation edge", func(t *testing.T) {
		store, identity, _, run := newRecoveryFixture(t)
		other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "another-person", "Another Person")
		if err != nil {
			t.Fatal(err)
		}
		otherTask, err := store.CreateTask(ctx, TaskCreate{TenantID: other.TenantID, PersonID: other.PersonID, Title: "their release", Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := store.StartRun(ctx, otherTask, "cli", quote+"; prepare the notes")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE runs SET resumes_run_id = ? WHERE id = ?`, foreign.ID, run.ID); err != nil {
			t.Fatal(err)
		}
		_, err = cancelDeploy(t, store, identity.TenantID, run.ID)
		refused(t, err)
	})
}

// A source whose input is no longer stored names nothing: no kind is guessed
// for it, so a display shows no authorizing input rather than a wrong one.
func TestQuoteOriginOfAMissingInputIsUnknown(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	guidance := consumeSteering(t, store, identity, task, run, "I will run the deploy myself")
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
		{Step: "Run the deploy", Status: "pending"}, {Step: "Write the notes", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deploy := plan.Plan.Steps[0].StepID
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "handed over", []RunPlanStepInput{
		quotedCancellation(deploy, "user_takeover", "I will run the deploy myself"), {StepID: plan.Plan.Steps[1].StepID, Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	step, _ := recordedStep(t, store, identity.TenantID, run.ID, deploy)
	present := RunPlanStep{CancellationQuoteSource: "run_input:" + run.ID}
	origins, err := store.CancellationQuoteOrigins(ctx, identity.TenantID, []RunPlanStep{step, present})
	if err != nil {
		t.Fatal(err)
	}
	if got := origins[step.CancellationQuoteSource]; got.Kind != CancellationQuoteFromSteering || got.RunID != run.ID || got.ReceivedAt.IsZero() {
		t.Fatalf("stored guidance origin = %+v", got)
	}
	if _, err := store.db.Exec(`DELETE FROM steering_mailbox WHERE id = ?`, guidance.ID); err != nil {
		t.Fatal(err)
	}
	missingRun := RunPlanStep{CancellationQuoteSource: "run_input:run_no_longer_stored"}
	origins, err = store.CancellationQuoteOrigins(ctx, identity.TenantID, []RunPlanStep{step, missingRun, present})
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{step.CancellationQuoteSource, missingRun.CancellationQuoteSource} {
		if origin, named := origins[missing]; named {
			t.Fatalf("missing input %s resolved to %+v", missing, origin)
		}
	}
	if got := origins[present.CancellationQuoteSource]; got.Kind != CancellationQuoteFromRunInput || got.RunID != run.ID {
		t.Fatalf("stored request origin = %+v", got)
	}
}

func TestScopeChangeUsesMainJudgmentDespiteQuestionPunctuation(t *testing.T) {
	for _, text := range []string{"Cancel the rollout, please?", "停止后续部署，可以吗？", "Could you stop the rollout?"} {
		t.Run(text, func(t *testing.T) {
			store, identity, task, run := newRecoveryFixture(t)
			ctx := context.Background()
			plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{{Step: "Deploy production", Status: "pending"}})
			if err != nil {
				t.Fatal(err)
			}
			consumeSteering(t, store, identity, task, run, text)
			_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "user stopped rollout", []RunPlanStepInput{quotedCancellation(plan.Plan.Steps[0].StepID, "not_required", text)})
			if err != nil {
				t.Fatalf("actual exact user cancellation rejected: %v", err)
			}
		})
	}
}
