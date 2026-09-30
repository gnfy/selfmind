package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

func TestExactEffectScriptProfilesSeparateTargetsAndFailClosedOnDrift(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	script := filepath.Join(root, "deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf accepted\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"cluster:east", "cluster:west"} {
		arg := target[len("cluster:"):]
		rule, err := BuildEffectScriptRule(EffectScriptProfile{WorkspaceID: "ws-1", ScriptPath: script,
			Argv: []string{arg}, TargetKeys: []string{target}, AllowNetwork: true}, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.GrantApproval(ctx, "person", "default", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{TenantID: "default", PersonID: "person-1",
		WorkspaceID: "ws-1", WorkspaceRoot: root, AllowedRoots: []string{root},
		TrustLevel: executionenv.TrustTrusted, StandingGrants: InteractiveStandingGrants()})
	defer cleanup()
	args := map[string]interface{}{"_tenant_id": "person-1", "_tool_name": "terminal", "cwd": root,
		"command": "./deploy.sh east", "_network_shared": true,
		toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}
	if targets, ok := registeredEffectScriptTargets(args, store); !ok || len(targets) != 1 || targets[0] != "cluster:east" {
		t.Fatalf("east target = %v, %t", targets, ok)
	}
	if targets, complete := externalEffectTargets(args, store); complete || len(targets) != 1 || targets[0] != "cluster:east" {
		t.Fatalf("claim target = %v, complete=%t", targets, complete)
	}
	args["command"] = "./deploy.sh west"
	if targets, ok := registeredEffectScriptTargets(args, store); !ok || len(targets) != 1 || targets[0] != "cluster:west" {
		t.Fatalf("west target = %v, %t", targets, ok)
	}
	for _, command := range []string{"./deploy.sh unknown", "./deploy.sh east; echo extra", "sh deploy.sh east"} {
		args["command"] = command
		if targets, ok := registeredEffectScriptTargets(args, store); ok || targets != nil {
			t.Fatalf("unreviewed command %q narrowed target: %v", command, targets)
		}
	}
	args["command"] = "./deploy.sh east"
	args["_network_shared"] = false
	if targets, ok := registeredEffectScriptTargets(args, store); ok || targets != nil {
		t.Fatalf("different network authority narrowed target: %v", targets)
	}
	args["_network_shared"] = true
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf changed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if targets, ok := registeredEffectScriptTargets(args, store); ok || targets != nil {
		t.Fatalf("changed script narrowed target: %v", targets)
	}
}

func TestEffectScriptProfileRejectsUnknownAndDuplicateTargets(t *testing.T) {
	if effectArgsHash(nil) != effectArgsHash([]string{}) {
		t.Fatal("a no-argument profile would never match its invocation")
	}
	root := t.TempDir()
	script := filepath.Join(root, "effect.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, targets := range [][]string{{control.UnknownExternalTarget}, {"cluster:east", "cluster:east"}, {"bad target"}} {
		if _, err := BuildEffectScriptRule(EffectScriptProfile{WorkspaceID: "ws-1", ScriptPath: script,
			TargetKeys: targets}, root); err == nil {
			t.Fatalf("invalid target profile accepted: %v", targets)
		}
	}
}

func TestRegisteredEffectScriptsClaimIndependentTargetsAcrossRuns(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := filepath.Join(root, "deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf dispatched\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"cluster:east", "cluster:west"} {
		rule, err := BuildEffectScriptRule(EffectScriptProfile{WorkspaceID: "ws-1", ScriptPath: script,
			Argv: []string{target[len("cluster:"):]}, TargetKeys: []string{target}, AllowNetwork: true}, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.GrantApproval(ctx, "person", owner.TenantID, owner.PersonID, owner.PersonID, rule.Key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	registry := NewRegistry()
	registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
	registry.Register(&BaseTool{name: "terminal", description: "test effect command",
		schema: ToolSchema{Type: "object", AdditionalProperties: rejectAdditionalProperties(),
			Properties: map[string]PropertyDef{"command": {Type: "string"}, "cwd": {Type: "string"}}},
		metadata: ToolMetadata{Category: "network", OperationClasses: []OperationClass{OpClassNetwork}},
		handler:  func(map[string]interface{}) (string, error) { return "dispatched", nil }})
	var runs []*control.Run
	for _, name := range []string{"east", "west", "duplicate"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID,
			PersonID: owner.PersonID, Title: name, Channel: name})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, name, name, control.StartRunOptions{MaxActiveRuns: 3})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		cleanup := SetExecutionScope(ExecutionScopeKeyForRun(run.ID), ExecutionScope{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: run.ID, WorkspaceID: "ws-1", WorkspaceRoot: root,
			AllowedRoots: []string{root}, TrustLevel: executionenv.TrustTrusted,
			StandingGrants: InteractiveStandingGrants(), ParallelWork: true})
		t.Cleanup(cleanup)
	}
	dispatch := func(run *control.Run, target string) error {
		_, err := registry.Dispatch("terminal", map[string]interface{}{
			"command": "./deploy.sh " + target, "cwd": root, "_network_shared": true,
			"_tool_call_id":     "call-" + run.ID,
			"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(run.ID)},
		})
		return err
	}
	if err := dispatch(runs[0], "east"); err != nil {
		t.Fatalf("east dispatch: %v", err)
	}
	if err := dispatch(runs[1], "west"); err != nil {
		t.Fatalf("independent west target was blocked: %v", err)
	}
	if err := dispatch(runs[2], "east"); err == nil {
		t.Fatal("same target was dispatched while east effect remained uncertain")
	}
}

func TestEffectObservationBindingRequiresExactOwnerReviewedCommand(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	effect := filepath.Join(root, "deploy.sh")
	observe := filepath.Join(root, "state.sh")
	for _, path := range []string{effect, observe} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"status\":\"succeeded\"}'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rule, err := BuildEffectScriptRule(EffectScriptProfile{WorkspaceID: "ws-1", ScriptPath: effect,
		Argv: []string{"east"}, TargetKeys: []string{"cluster:east"}, AllowNetwork: true,
		ObservationCommand: "./state.sh east"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantApproval(ctx, "person", "default", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	readRule, err := BuildObservationScriptRule(ObservationScriptProfile{WorkspaceID: "ws-1",
		ScriptPath: observe, ArgvPrefix: []string{"east"}, AllowNetwork: true}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantApproval(ctx, "person", "default", "person-1", "person-1", readRule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{TenantID: "default", PersonID: "person-1",
		WorkspaceID: "ws-1", WorkspaceRoot: root, AllowedRoots: []string{root},
		TrustLevel: executionenv.TrustTrusted, StandingGrants: InteractiveStandingGrants()})
	defer cleanup()
	args := map[string]interface{}{"_tenant_id": "person-1", "cwd": root, "command": "./state.sh east", "_network_shared": true}
	bound, ok := RegisteredEffectObservation(args, store)
	if !ok || len(bound.TargetKeys) != 1 || bound.TargetKeys[0] != "cluster:east" || bound.ScriptDigest == "" {
		t.Fatalf("exact observation binding = %+v, %t", bound, ok)
	}
	for _, command := range []string{"./state.sh west", "./state.sh east; echo extra", "sh state.sh east"} {
		args["command"] = command
		if _, ok := RegisteredEffectObservation(args, store); ok {
			t.Fatalf("unreviewed command %q bound", command)
		}
	}
	args["command"] = "./state.sh east"
	if err := os.WriteFile(observe, []byte("#!/bin/sh\nprintf changed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ValidateEffectObservationScript(bound.ScriptRoot, bound.ScriptPath, bound.ScriptDigest) {
		t.Fatal("changed script kept original proof")
	}
	if _, ok := RegisteredEffectObservation(args, store); ok {
		t.Fatal("changed observation bound")
	}
}
