package control_test

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// The store checks admission values against sets that mirror the producers'
// names, because control may not import them. Every value a producer emits
// today must be accepted, or wiring the snapshot would fail at admission.
func TestAdmissionAcceptsEveryProducerValue(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(name string, admission control.AdmissionSnapshot) {
		t.Helper()
		if _, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
			Content: name, Admission: admission}); err != nil {
			t.Fatalf("%s: producer value refused: %v", name, err)
		}
	}
	snapshot := func(strategy kernel.TaskStrategy, mode tools.ApprovalMode) control.AdmissionSnapshot {
		return control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: string(mode),
			Strategy: control.AdmissionStrategy{Class: string(strategy.Class), ToolMode: string(strategy.ToolMode),
				PlanPolicy: string(strategy.PlanPolicy), WebPolicy: string(strategy.WebPolicy),
				ChannelMode: strategy.ChannelMode, VerificationOnly: strategy.VerificationOnly}}
	}
	for _, prompt := range []string{"", "who are you", "look up today's release notes", "write a go function", "fix the failing build"} {
		for _, channel := range []string{"cli", "telegram", "6f1d1c9e-4f0b-4d55-9b3e-52b3b1f2a9c1", ""} {
			strategy := kernel.BuildTaskStrategy(prompt, channel)
			enqueue("built "+prompt+" on "+channel, snapshot(strategy, tools.DefaultApprovalMode))
			enqueue("web "+prompt+" on "+channel, snapshot(strategy.WithWebEnabled(), tools.DefaultApprovalMode))
		}
	}
	base := kernel.DefaultTaskStrategy()
	for _, class := range []kernel.TaskClass{kernel.TaskClassSimpleAnswer, kernel.TaskClassCodingExample, kernel.TaskClassStableAdvice,
		kernel.TaskClassExternalLookup, kernel.TaskClassRepoTask, kernel.TaskClassDebugTask, kernel.TaskClassCICDTask, kernel.TaskClassGeneralTask} {
		strategy := base
		strategy.Class = class
		enqueue("class "+string(class), snapshot(strategy, tools.DefaultApprovalMode))
	}
	for _, mode := range []kernel.ToolMode{kernel.ToolModeNone, kernel.ToolModeWeb, kernel.ToolModeLocalRead, kernel.ToolModeLocalWrite, kernel.ToolModeFull} {
		strategy := base
		strategy.ToolMode = mode
		enqueue("tool mode "+string(mode), snapshot(strategy, tools.DefaultApprovalMode))
	}
	for _, policy := range []kernel.PlanPolicy{kernel.PlanPolicyDisabled, kernel.PlanPolicyOptional, kernel.PlanPolicyRequired} {
		strategy := base
		strategy.PlanPolicy = policy
		enqueue("plan policy "+string(policy), snapshot(strategy, tools.DefaultApprovalMode))
	}
	for _, policy := range []kernel.WebPolicy{kernel.WebPolicyDisabled, kernel.WebPolicyExplicit, kernel.WebPolicyEnabled} {
		strategy := base
		strategy.WebPolicy = policy
		enqueue("web policy "+string(policy), snapshot(strategy, tools.DefaultApprovalMode))
	}
	verifyOnly := base
	verifyOnly.VerificationOnly, verifyOnly.ToolMode = true, kernel.ToolModeLocalRead
	recovery := snapshot(verifyOnly, tools.ApprovalSmart)
	recovery.RecoveryMode, recovery.ExecutionProfile = control.RunRecoveryModeVerifyOnly, tools.ExecutionProfileWatchFinalization
	enqueue("verify-only watcher recovery", recovery)
	for _, word := range []string{"", "on-request", "read-only", "readonly", "auto-edit", "auto", "full-auto", "yolo", "smart", "nonsense"} {
		enqueue("approval mode "+word, snapshot(base, tools.EffectiveApprovalMode(word)))
	}
}
