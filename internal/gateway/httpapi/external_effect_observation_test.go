package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

func TestFinalizedOwnerBoundWatchReleasesOnlyUnchangedExactEffect(t *testing.T) {
	for _, state := range []string{"unchanged", "changed", "revoked", "observe-revoked"} {
		t.Run(state, func(t *testing.T) {
			daemon, store, owner, task, _ := newApprovalTestServer(t)
			ctx := context.Background()
			run, err := store.StartRun(ctx, task, "cli", "deploy target")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: owner.TenantID,
				OwnerPersonID: owner.PersonID, Name: "test", LocalPath: root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetWorkspaceTrust(ctx, owner.TenantID, owner.PersonID, workspace.ID, "trusted", "local_cli"); err != nil {
				t.Fatal(err)
			}
			effectPath := filepath.Join(root, "deploy.sh")
			observePath := filepath.Join(root, "observe.sh")
			if err := os.WriteFile(effectPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			observation := []byte("#!/bin/sh\nprintf '{\"status\":\"succeeded\"}'\n")
			if err := os.WriteFile(observePath, observation, 0o700); err != nil {
				t.Fatal(err)
			}
			rule, err := tools.BuildEffectScriptRule(tools.EffectScriptProfile{WorkspaceID: workspace.ID,
				ScriptPath: effectPath, Argv: []string{"east"}, TargetKeys: []string{"cluster:east"},
				ObservationCommand: "./observe.sh east", AllowNetwork: true}, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.GrantApproval(ctx, "person", owner.TenantID, owner.PersonID, owner.PersonID, rule.Key, time.Time{}); err != nil {
				t.Fatal(err)
			}
			readRule, err := tools.BuildObservationScriptRule(tools.ObservationScriptProfile{WorkspaceID: workspace.ID,
				ScriptPath: observePath, ArgvPrefix: []string{"east"}, AllowNetwork: true}, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.GrantApproval(ctx, "person", owner.TenantID, owner.PersonID, owner.PersonID, readRule.Key, time.Time{}); err != nil {
				t.Fatal(err)
			}
			claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID,
				PersonID: owner.PersonID, RunID: run.ID, EffectID: "call-east", TargetKeys: []string{"cluster:east"}})
			if err != nil || !claim.Granted {
				t.Fatalf("claim = %+v %v", claim, err)
			}
			if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "call-east"); err != nil {
				t.Fatal(err)
			}
			command := "./observe.sh east"
			digest := fmt.Sprintf("%x", sha256.Sum256(observation))
			watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: owner.TenantID,
				PersonID: owner.PersonID, WorkspaceID: workspace.ID, TaskID: task.ID, RunID: run.ID,
				Channel: "cli", CWD: root, Command: command, SpecVersion: 3,
				ObservationAdapter: tools.ExternalWatchAdapterStatusJSON,
				PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchContinuationReceiptVersion,
					CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command))), EffectID: "call-east",
					EffectRuleKey: rule.Key, ObservationRuleKey: readRule.Key, EffectTargetKeys: []string{"cluster:east"},
					EffectScriptRoot: root, EffectScriptPath: observePath, EffectScriptDigest: digest},
				TimeoutAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}
			output := `{"status":"succeeded"}`
			if state == "unchanged" {
				// Exercise the real durable watcher check before releasing the
				// exact target. Keep its finalization queued without starting a
				// model-backed continuation in this deterministic test.
				if !daemon.coordinator().beginActive(owner.PersonID, &activeRun{TaskID: "busy"}) {
					t.Fatal("failed to hold the finalization queue")
				}
				defer daemon.coordinator().endActive(owner.PersonID)
				daemon.runExternalWatchPass(ctx)
				stored, err := store.GetExternalWatch(ctx, owner.TenantID, watch.ID)
				if err != nil || stored == nil || stored.Status != control.ExternalWatchSucceeded || stored.LastOutput != output {
					t.Fatalf("durable watcher result = %+v, %v", stored, err)
				}
			} else if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, control.ExternalWatchSucceeded, output, ""); err != nil || !ok {
				t.Fatalf("watch finish = %v %v", ok, err)
			}
			if state == "changed" {
				if err := os.WriteFile(observePath, []byte("#!/bin/sh\nprintf forged\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "revoked" || state == "observe-revoked" {
				grants, err := store.ListApprovalGrants(ctx, owner.TenantID, owner.PersonID, false)
				if err != nil || len(grants) != 2 {
					t.Fatalf("grants = %+v %v", grants, err)
				}
				key := rule.Key
				if state == "observe-revoked" {
					key = readRule.Key
				}
				grantID := ""
				for _, grant := range grants {
					if grant.PatternKey == key {
						grantID = grant.ID
					}
				}
				if revoked, err := store.RevokeApprovalGrant(ctx, owner.TenantID, owner.PersonID, grantID); err != nil || !revoked {
					t.Fatalf("revoke = %t %v", revoked, err)
				}
			}
			stored, err := store.GetExternalWatch(ctx, owner.TenantID, watch.ID)
			if err != nil || stored == nil {
				t.Fatalf("stored watch = %+v %v", stored, err)
			}
			if state != "changed" && !tools.ValidateEffectObservationScript(root, observePath, digest) {
				t.Fatal("unchanged script failed validation")
			}
			if state != "unchanged" {
				daemon.finalizeExternalWatch(ctx, *stored, stored.Status, output, "")
			}
			claims, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if state != "unchanged" && len(claims) != 1 || state == "unchanged" && len(claims) != 0 {
				t.Fatalf("state=%s unresolved claims = %+v", state, claims)
			}
		})
	}
}
