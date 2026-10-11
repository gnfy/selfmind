package control

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func queuedAdmission(t *testing.T, store *Store, tenantID, queueID string) AdmissionSnapshot {
	t.Helper()
	row, err := store.GetQueued(context.Background(), tenantID, queueID)
	if err != nil || row == nil || row.AdmissionError != nil {
		t.Fatalf("resume row %s = %+v err=%v", queueID, row, err)
	}
	return row.Admission
}

func otherAdmission() AdmissionSnapshot {
	a := verifyOnlyCronAdmission()
	a.Strategy.ToolMode, a.Strategy.VerificationOnly, a.RecoveryMode = "full", false, ""
	return a
}

// A resume row's authority is the parked Run's, copied inside the answer's
// transaction. A caller may restate it, but never supply a different one.
func TestHumanWaitResumeRowsCopyTheParkedRunsOwnAdmission(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// A parked approval answered later: the caller passes nothing and the row
	// still carries the parked Run's snapshot.
	owner, task, run := humanWaitRun(t, store, "owner")
	parked := pendingApproval(t, store, owner, task, run)
	if _, ok, err := store.ParkApprovalRequest(ctx, owner.TenantID, parked.ID, "wait budget elapsed"); err != nil || !ok {
		t.Fatalf("park: %v %v", ok, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	resume := QueuedTask{TaskID: task.ID, Content: "approved", IdempotencyKey: "approval-resume:" + parked.ID}
	widened := resume
	widened.Admission = otherAdmission()
	if _, _, err := store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, parked.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, widened); err == nil {
		t.Fatal("a resume row took a snapshot other than the parked Run's")
	}
	if still, err := store.GetApprovalRequest(ctx, owner.TenantID, parked.ID); err != nil || still.Status != "pending" {
		t.Fatalf("refused resume changed the approval: %+v err=%v", still, err)
	}
	_, row, err := store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, parked.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, resume)
	if err != nil {
		t.Fatal(err)
	}
	if row.Admission != verifyOnlyCronAdmission() || queuedAdmission(t, store, owner.TenantID, row.ID) != verifyOnlyCronAdmission() {
		t.Fatalf("approval-resume row = %+v, want the parked Run's snapshot", row.Admission)
	}

	// A clarification answered after its Run parked.
	clarify := pendingClarification(t, store, owner, task, run)
	if _, _, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
		QueuedTask{Admission: otherAdmission()}); err == nil {
		t.Fatal("a clarify-resume row took a snapshot other than the parked Run's")
	}
	_, clarifyRow, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli", QueuedTask{})
	if err != nil {
		t.Fatal(err)
	}
	if queuedAdmission(t, store, owner.TenantID, clarifyRow.ID) != verifyOnlyCronAdmission() {
		t.Fatal("clarify-resume row did not copy the parked Run's snapshot")
	}

	// A decision recovered after a crash.
	crashedOwner, crashedTask, crashedRun := humanWaitRun(t, store, "crashed")
	live := pendingApproval(t, store, crashedOwner, crashedTask, crashedRun)
	if _, err := store.RespondApprovalRequest(ctx, crashedOwner.TenantID, crashedOwner.PersonID, live.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, crashedOwner.TenantID, crashedRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	recovered := QueuedTask{TenantID: crashedOwner.TenantID, PersonID: crashedOwner.PersonID, TaskID: crashedTask.ID,
		Content: "approved", IdempotencyKey: "approval-resume:" + live.ID}
	wrong := recovered
	wrong.Admission = otherAdmission()
	if _, _, err := store.EnqueueRecoveredApprovalContinuation(ctx, live.ID, wrong); err == nil {
		t.Fatal("a recovered resume row took a snapshot other than the parked Run's")
	}
	recoveredRow, created, err := store.EnqueueRecoveredApprovalContinuation(ctx, live.ID, recovered)
	if err != nil || !created || queuedAdmission(t, store, crashedOwner.TenantID, recoveredRow.ID) != verifyOnlyCronAdmission() {
		t.Fatalf("recovered resume row = %+v created=%v err=%v", recoveredRow, created, err)
	}
}

// A Run admitted without a snapshot keeps none on resume: a caller cannot give
// historical work an authority it never had.
func TestHumanWaitResumeOfALegacyRunCarriesNoSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	run, err := store.StartRun(ctx, task, "cli", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	clarify := pendingClarification(t, store, owner, task, run)
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
		QueuedTask{Admission: verifyOnlyCronAdmission()}); err == nil {
		t.Fatal("a legacy Run's resume row took a supplied snapshot")
	}
	_, row, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli", QueuedTask{})
	if err != nil {
		t.Fatal(err)
	}
	if got := queuedAdmission(t, store, owner.TenantID, row.ID); !got.IsZero() {
		t.Fatalf("legacy resume row carries %+v", got)
	}
}

// A summary cut at its bound can end on whitespace. The stored snapshot is
// trimmed after the cut, so a caller restating it, or the original it came
// from, matches the parked Run's; a different summary still does not.
func TestRestatedAdmissionMatchesWhenTheSummaryIsCutAtWhitespace(t *testing.T) {
	for name, boundary := range map[string]string{"space": " ", "tab": "\t", "ideographic space": "　"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, task := admissionOwnerAndThread(t, store, "owner")
			original := verifyOnlyCronAdmission()
			original.Input.Summary = strings.Repeat("a", admissionSummaryMaxRunes-1) + boundary + "and the rest of a long request"
			if normalized := original.normalized(); normalized.normalized() != normalized {
				t.Fatalf("normalizing twice changed the snapshot: %q", normalized.Input.Summary)
			}
			run, err := store.StartRunWithOptions(ctx, task, "cli", "deploy", StartRunOptions{Admission: original})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
			if err != nil || stored.Input.Summary != strings.Repeat("a", admissionSummaryMaxRunes-1) {
				t.Fatalf("stored summary = %q err=%v", stored.Input.Summary, err)
			}
			clarify := pendingClarification(t, store, owner, task, run)
			approval := pendingApproval(t, store, owner, task, run)
			if _, ok, err := store.ParkApprovalRequest(ctx, owner.TenantID, approval.ID, "wait budget elapsed"); err != nil || !ok {
				t.Fatalf("park: %v %v", ok, err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			different := stored
			different.Input.Summary = strings.Repeat("b", admissionSummaryMaxRunes-1)
			if _, _, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
				QueuedTask{Admission: different}); !errors.Is(err, ErrResumeAdmissionMismatch) {
				t.Fatalf("a different summary was accepted: %v", err)
			}
			_, clarifyRow, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
				QueuedTask{Admission: stored})
			if err != nil {
				t.Fatalf("restating the stored snapshot was refused: %v", err)
			}
			if queuedAdmission(t, store, owner.TenantID, clarifyRow.ID) != stored {
				t.Fatal("clarify-resume row does not carry the parked Run's snapshot")
			}
			_, approvalRow, err := store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, approval.ID, "approved", "cli",
				ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{TaskID: task.ID, Content: "approved",
					IdempotencyKey: "approval-resume:" + approval.ID, Admission: original})
			if err != nil {
				t.Fatalf("restating the original snapshot was refused: %v", err)
			}
			if queuedAdmission(t, store, owner.TenantID, approvalRow.ID) != stored {
				t.Fatal("approval-resume row does not carry the parked Run's snapshot")
			}
		})
	}
}

// A parked snapshot stored before that trim keeps its trailing whitespace.
// Both sides are compared in normalized form, so restating it still matches,
// and the resume row copies the stored bytes unchanged.
func TestRestatedAdmissionMatchesAParkedSummaryStoredWithTrailingWhitespace(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	clarify := pendingClarification(t, store, owner, task, run)
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET admission_json = json_set(admission_json, '$.input.summary', ?) WHERE id = ?`,
		"check the nightly export ", run.ID); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.db.QueryRowContext(ctx, `SELECT admission_json FROM runs WHERE id = ?`, run.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	stored, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
	if err != nil || stored.Input.Summary != "check the nightly export " {
		t.Fatalf("stored summary = %q err=%v", stored.Input.Summary, err)
	}
	_, row, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
		QueuedTask{Admission: stored})
	if err != nil {
		t.Fatalf("restating a parked snapshot stored with trailing whitespace was refused: %v", err)
	}
	var copied string
	if err := store.db.QueryRowContext(ctx, `SELECT admission_json FROM task_queue WHERE id = ?`, row.ID).Scan(&copied); err != nil || copied != raw {
		t.Fatalf("resume row stored %q, want the parked bytes %q (err=%v)", copied, raw, err)
	}
}
