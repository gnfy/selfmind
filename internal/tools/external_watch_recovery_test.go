package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

func TestRecoveryWatchProductionPreflightPreservesTerminalMeaning(t *testing.T) {
	for _, tc := range []struct {
		name, observed string
		eventFailure   bool
	}{{"succeeded", "SUCCEEDED", false}, {"failed", "FAILED", false}, {"event-save-failed", "SUCCEEDED", true}} {
		t.Run(tc.name, func(t *testing.T) {
			observed := tc.observed
			if !ExecSandboxAvailable() {
				t.Skip("enforced sandbox unavailable")
			}
			ctx := context.Background()
			dataDir := t.TempDir()
			store, err := control.OpenStore(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "recovery-fixture", "Recovery")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "Observe an uncertain operation", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.StartRun(ctx, task, "cli", "Perform operation")
			if err != nil {
				t.Fatal(err)
			}
			claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: person.TenantID, PersonID: person.PersonID, RunID: parent.ID, EffectID: "uncertain-operation", TargetKeys: []string{control.UnknownExternalTarget}})
			if err != nil || !claim.Granted {
				t.Fatalf("claim %v %v", claim, err)
			}
			if err := store.MarkExternalEffectPossible(ctx, person.TenantID, parent.ID, "uncertain-operation"); err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, person.TenantID, parent.ID, "blocked"); err != nil {
				t.Fatal(err)
			}
			child, err := store.StartRunWithOptions(ctx, task, "cli", "Observe without replay", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			base := fixtureBase(t)
			home := filepath.Join(base, "home")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
			previous, _, _ := lookupExecutionScope(tenant)
			snapshotID, ok := executionenv.DefaultRegistry().ForLease(previous.LeaseID)
			if !ok {
				t.Fatal("fixture snapshot missing")
			}
			lease, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{ID: previous.LeaseID, TenantID: person.TenantID, PersonID: person.PersonID, RunID: child.ID, WorkspaceID: "ws-recovery", EnvironmentSnapshotID: snapshotID.ID, EnvironmentGeneration: snapshotID.Generation, PrincipalFingerprint: snapshotID.PrincipalFingerprint, EnvironmentFingerprint: snapshotID.EnvironmentFingerprint, CredentialSourceHash: snapshotID.CredentialSourceHash})
			if err != nil {
				t.Fatal(err)
			}
			scope := previous
			scope.TenantID = person.TenantID
			scope.PersonID = person.PersonID
			scope.TaskID = task.ID
			scope.RunID = child.ID
			scope.WorkspaceID = lease.WorkspaceID
			scope.ParallelWork = true
			scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, Required: true, AllowNetwork: false}
			cleanup := SetExecutionScope(tenant, scope)
			defer cleanup()
			if err := os.WriteFile(filepath.Join(workspace, "observed.state"), []byte(observed+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.eventFailure {
				db, err := sql.Open("sqlite", filepath.Join(dataDir, "control.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`CREATE TRIGGER fail_recovery_event BEFORE INSERT ON task_events WHEN NEW.type='external_watch.created' BEGIN SELECT RAISE(ABORT,'injected event save failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			result, err := NewExternalWatchTool(store).Execute(map[string]interface{}{"_tenant_id": tenant, "command": "cat observed.state", "cwd": workspace, "terminal_success_pattern": "^SUCCEEDED$", "terminal_failure_pattern": "^FAILED$", "effect_claim_id": claim.Claims[0].ID})
			if tc.eventFailure {
				watches, listErr := store.ListExternalWatchesForPerson(ctx, person.TenantID, person.PersonID, "all", 10, 0)
				if listErr != nil || len(watches) != 1 || !watches[0].Finalized {
					t.Fatalf("saved observation lost after event failure: %+v %v", watches, listErr)
				}
				if err == nil || !strings.Contains(err.Error(), watches[0].ID) {
					t.Fatalf("saved observation reference was lost in failure reply: %v", err)
				}
				effects, listErr := store.ListUnresolvedExternalEffects(ctx, person.TenantID, person.PersonID, 10)
				if listErr != nil || len(effects) != 1 || effects[0].State != control.ExternalClaimUncertain {
					t.Fatalf("failed reply released effect: %+v %v", effects, listErr)
				}
				return
			}
			if observed == "FAILED" {
				if err == nil || !strings.Contains(err.Error(), "first check already matches the failure pattern") {
					t.Fatalf("failed preflight became trusted evidence: %s %v", result, err)
				}
				watches, listErr := store.ListExternalWatchesForPerson(ctx, person.TenantID, person.PersonID, "all", 10, 0)
				if listErr != nil || len(watches) != 0 {
					t.Fatalf("failed preflight registered a watcher: %+v %v", watches, listErr)
				}
				effects, listErr := store.ListUnresolvedExternalEffects(ctx, person.TenantID, person.PersonID, 10)
				if listErr != nil || len(effects) != 1 || effects[0].State != control.ExternalClaimUncertain {
					t.Fatalf("failed preflight erased effect: %+v %v", effects, listErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("production recovery registration: %v", err)
			}
			// The model bound this successful check to the claim of the Run it
			// continues, so the check releases that claim itself (item 3).
			var response struct {
				WatchID  string `json:"watch_id"`
				Released bool   `json:"released"`
			}
			if err := json.Unmarshal([]byte(result), &response); err != nil || response.WatchID == "" || !response.Released {
				t.Fatalf("reply=%s err=%v", result, err)
			}
			if strings.Contains(result, "lifecycle_handoff") || strings.Contains(result, "/effects resolve") {
				t.Fatalf("settled observation parked the Run or asked for a confirmation: %s", result)
			}
			watch, err := store.GetExternalWatch(ctx, person.TenantID, response.WatchID)
			if err != nil || watch.Status != strings.ToLower(observed) || !watch.Finalized || !watch.Notified {
				t.Fatalf("observation was not atomically settled: %+v %v", watch, err)
			}
			// Its polls keep the policy the registering Run's call ran under.
			if frozen := watch.ExecutionBinding.SandboxPolicy; frozen == nil ||
				*frozen != (executionenv.SandboxPolicy{Enabled: true, Required: true}) {
				t.Fatalf("registration froze sandbox policy %+v, want the Run's", frozen)
			}
			unresolved, err := store.ListUnresolvedExternalEffects(ctx, person.TenantID, person.PersonID, 10)
			if err != nil || len(unresolved) != 0 {
				t.Fatalf("bound successful observation did not release the claim: %+v %v", unresolved, err)
			}
			released, err := store.ExternalEffectClaimsForEffect(ctx, person.TenantID, person.PersonID, parent.ID, "uncertain-operation")
			if err != nil || len(released) != 1 || released[0].ObservationRef != "watch:"+watch.ID+":r1:bound" {
				t.Fatalf("release does not name the watcher: %+v %v", released, err)
			}
		})
	}
}
