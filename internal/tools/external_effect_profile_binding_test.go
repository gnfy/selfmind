package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

// effectBindingFixture is a trusted workspace with one effect script, one
// observation script and one stdin payload, owned by person-1.
type effectBindingFixture struct {
	store *control.Store
	root  string
}

func newEffectBindingFixture(t *testing.T) effectBindingFixture {
	t.Helper()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := t.TempDir()
	for _, dir := range []string{"scripts", "config"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		"scripts/deploy.sh":   "#!/bin/sh\nprintf dispatched\n",
		"scripts/state.sh":    "#!/bin/sh\nprintf '{\"status\":\"succeeded\"}'\n",
		"config/payload.json": `{"release":"1.2"}`,
		"config/copy.json":    `{"release":"1.2"}`,
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := SetExecutionScope("person-1", ExecutionScope{TenantID: "default", PersonID: "person-1",
		WorkspaceID: "ws-1", WorkspaceRoot: root, AllowedRoots: []string{root},
		TrustLevel: executionenv.TrustTrusted, StandingGrants: InteractiveStandingGrants()})
	t.Cleanup(cleanup)
	return effectBindingFixture{store: store, root: root}
}

func (f effectBindingFixture) register(t *testing.T, profile EffectScriptProfile) string {
	t.Helper()
	profile.WorkspaceID = "ws-1"
	rule, err := BuildEffectScriptRule(profile, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantApproval(context.Background(), "person", "default", "person-1", "person-1", rule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	return rule.Key
}

func (f effectBindingFixture) targets(cwd, command string) []string {
	args := map[string]interface{}{"_tenant_id": "person-1", "_tool_name": "terminal", "cwd": cwd,
		"command": command, "_network_shared": true,
		toolExecutionPolicyArg: toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin}}
	keys, _ := externalEffectTargets(args, f.store)
	return keys
}

func textInput(value string) *string { return &value }

// An effect profile names one exact invocation. Reaching the same script from
// another directory, with other arguments, or with other declared environment
// or standard input is a different invocation, so it keeps the person-wide
// unknown target instead of the owner's narrowed one.
func TestEffectProfileMatchesOnlyTheExactRegisteredInvocation(t *testing.T) {
	f := newEffectBindingFixture(t)
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"east"},
		TargetKeys: []string{"cluster:east"}, AllowNetwork: true,
		Env: []string{"DEPLOY_REGION=eu-1"}, StdinText: textInput("yes")})
	scripts := filepath.Join(f.root, "scripts")

	exact := "DEPLOY_REGION=eu-1 ./scripts/deploy.sh east <<< yes"
	if got := f.targets(f.root, exact); len(got) != 1 || got[0] != "cluster:east" {
		t.Fatalf("exact invocation claimed %v, want the registered target", got)
	}
	for name, call := range map[string]struct{ cwd, command string }{
		"another cwd":       {scripts, "DEPLOY_REGION=eu-1 ./deploy.sh east <<< yes"},
		"different args":    {f.root, "DEPLOY_REGION=eu-1 ./scripts/deploy.sh west <<< yes"},
		"extra args":        {f.root, "DEPLOY_REGION=eu-1 ./scripts/deploy.sh east --force <<< yes"},
		"different env":     {f.root, "DEPLOY_REGION=us-1 ./scripts/deploy.sh east <<< yes"},
		"missing env":       {f.root, "./scripts/deploy.sh east <<< yes"},
		"extra env":         {f.root, "DEPLOY_REGION=eu-1 DRY_RUN=0 ./scripts/deploy.sh east <<< yes"},
		"different stdin":   {f.root, "DEPLOY_REGION=eu-1 ./scripts/deploy.sh east <<< no"},
		"missing stdin":     {f.root, "DEPLOY_REGION=eu-1 ./scripts/deploy.sh east"},
		"stdin from a file": {f.root, "DEPLOY_REGION=eu-1 ./scripts/deploy.sh east < config/payload.json"},
	} {
		if got := f.targets(call.cwd, call.command); len(got) != 1 || got[0] != control.UnknownExternalTarget {
			t.Errorf("%s (%q in %s) claimed %v, want the unknown target", name, call.command, call.cwd, got)
		}
	}
}

// The same invariant with another directory, another argument shape and a
// file as standard input: the registered cwd is matched both as written and
// as resolved, and the declared input is that exact workspace file with its
// exact content.
func TestEffectProfileBindsItsCwdAndDeclaredInputFile(t *testing.T) {
	f := newEffectBindingFixture(t)
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"west", "--wait"},
		TargetKeys: []string{"cluster:west"}, AllowNetwork: true, Cwd: "scripts",
		StdinFile: "config/payload.json"})
	scripts := filepath.Join(f.root, "scripts")
	alias := filepath.Join(f.root, "linked-scripts")
	if err := os.Symlink(scripts, alias); err != nil {
		t.Fatal(err)
	}

	for _, cwd := range []string{scripts, "scripts", "./scripts/", filepath.Join(f.root, "config", "..", "scripts")} {
		if got := f.targets(cwd, "./deploy.sh west --wait < ../config/payload.json"); len(got) != 1 || got[0] != "cluster:west" {
			t.Fatalf("exact invocation from %s claimed %v", cwd, got)
		}
	}
	for name, call := range map[string]struct{ cwd, command string }{
		// The script sees another $PWD through a symlinked directory.
		"linked directory":      {alias, "./deploy.sh west --wait < ../config/payload.json"},
		"workspace root":        {f.root, "./scripts/deploy.sh west --wait < config/payload.json"},
		"another file":          {scripts, "./deploy.sh west --wait < ../config/copy.json"},
		"text instead of file":  {scripts, `./deploy.sh west --wait <<< '{"release":"1.2"}'`},
		"no input":              {scripts, "./deploy.sh west --wait"},
		"reordered args":        {scripts, "./deploy.sh --wait west < ../config/payload.json"},
		"declared env added":    {scripts, "TRACE=1 ./deploy.sh west --wait < ../config/payload.json"},
		"another stdin fd":      {scripts, "./deploy.sh west --wait 3< ../config/payload.json"},
		"expanded input name":   {scripts, "./deploy.sh west --wait < ../config/$NAME.json"},
		"output redirect added": {scripts, "./deploy.sh west --wait < ../config/payload.json > out.txt"},
	} {
		if got := f.targets(call.cwd, call.command); len(got) != 1 || got[0] != control.UnknownExternalTarget {
			t.Errorf("%s (%q in %s) claimed %v, want the unknown target", name, call.command, call.cwd, got)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "config/payload.json"), []byte(`{"release":"9.9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.targets(scripts, "./deploy.sh west --wait < ../config/payload.json"); len(got) != 1 || got[0] != control.UnknownExternalTarget {
		t.Fatalf("changed input content kept the narrowed target: %v", got)
	}
}

// A script sees the path it was invoked by as $0, so a symlink alias of the
// registered script is another invocation even though the file is the same:
// a profile for deploy-staging.sh must not claim for deploy.sh, and the
// reverse. Other spellings of the same path still match.
func TestEffectProfileBindsTheInvokedScriptPath(t *testing.T) {
	f := newEffectBindingFixture(t)
	if err := os.Symlink("deploy.sh", filepath.Join(f.root, "scripts", "deploy-staging.sh")); err != nil {
		t.Fatal(err)
	}
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy-staging.sh", Argv: []string{"east"},
		TargetKeys: []string{"cluster:staging"}, AllowNetwork: true})
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"west"},
		TargetKeys: []string{"cluster:prod"}, AllowNetwork: true})
	for command, want := range map[string]string{
		"./scripts/deploy-staging.sh east":            "cluster:staging",
		"scripts/./deploy-staging.sh east":            "cluster:staging",
		"./scripts/deploy.sh west":                    "cluster:prod",
		"./scripts/deploy.sh east":                    control.UnknownExternalTarget,
		"./scripts/deploy-staging.sh west":            control.UnknownExternalTarget,
		f.root + "/scripts/deploy-staging.sh east":    "cluster:staging",
		f.root + "/scripts/../scripts/deploy.sh west": "cluster:prod",
	} {
		if got := f.targets(f.root, command); len(got) != 1 || got[0] != want {
			t.Errorf("%q claimed %v, want %s", command, got, want)
		}
	}
}

// The owner-approved observation command of a profile runs from the profile's
// directory with no other inputs; a watcher elsewhere does not bind the claim.
func TestEffectObservationBindsTheProfileCwd(t *testing.T) {
	f := newEffectBindingFixture(t)
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"east"},
		TargetKeys: []string{"cluster:east"}, AllowNetwork: true, ObservationCommand: "./scripts/state.sh east"})
	readRule, err := BuildObservationScriptRule(ObservationScriptProfile{WorkspaceID: "ws-1",
		ScriptPath: "scripts/state.sh", ArgvPrefix: []string{"east"}, AllowNetwork: true}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantApproval(context.Background(), "person", "default", "person-1", "person-1", readRule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	watch := func(cwd, command string) bool {
		_, ok := RegisteredEffectObservation(map[string]interface{}{"_tenant_id": "person-1", "cwd": cwd,
			"command": command, "_network_shared": true}, f.store)
		return ok
	}
	if !watch(f.root, "./scripts/state.sh east") {
		t.Fatal("the exact observation command did not bind")
	}
	if err := os.Symlink("state.sh", filepath.Join(f.root, "scripts", "status.sh")); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]struct{ cwd, command string }{
		"another cwd":   {filepath.Join(f.root, "scripts"), "./state.sh east"},
		"extra args":    {f.root, "./scripts/state.sh east --verbose"},
		"declared env":  {f.root, "REGION=eu ./scripts/state.sh east"},
		"declared text": {f.root, "./scripts/state.sh east <<< id"},
		"linked alias":  {f.root, "./scripts/status.sh east"},
	} {
		if watch(call.cwd, call.command) {
			t.Errorf("%s (%q in %s) bound the effect claim", name, call.command, call.cwd)
		}
	}
	if _, err := BuildEffectScriptRule(EffectScriptProfile{WorkspaceID: "ws-1", ScriptPath: "scripts/deploy.sh",
		TargetKeys: []string{"cluster:east"}, ObservationCommand: "REGION=eu ./scripts/state.sh east"}, f.root); err == nil {
		t.Fatal("an observation command with its own inputs was registered")
	}
}

// A profile registered before invocations were bound recorded no cwd or
// inputs: it means the workspace root with none. Registering the same
// root-invocation profile again must produce that same key, so the grant is
// refreshed in place rather than duplicated into an ambiguous second match.
func TestEffectProfileWithoutDeclaredInputsKeepsItsKey(t *testing.T) {
	f := newEffectBindingFixture(t)
	key := f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"east"},
		TargetKeys: []string{"cluster:east"}, AllowNetwork: true})
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, effectScriptRulePrefix))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		switch name {
		case "w", "r", "d", "a", "t", "n", "c":
		default:
			t.Fatalf("root profile without inputs encodes new field %q: %s", name, raw)
		}
	}
	if got := f.targets(f.root, "./scripts/deploy.sh east"); len(got) != 1 || got[0] != "cluster:east" {
		t.Fatalf("root invocation claimed %v", got)
	}
	if got := f.targets(filepath.Join(f.root, "scripts"), "./deploy.sh east"); len(got) != 1 || got[0] != control.UnknownExternalTarget {
		t.Fatalf("a root profile matched from another directory: %v", got)
	}
}

func TestEffectProfileRejectsInvalidDeclaredInputs(t *testing.T) {
	f := newEffectBindingFixture(t)
	for name, profile := range map[string]EffectScriptProfile{
		"cwd outside the workspace": {Cwd: ".."},
		"missing cwd":               {Cwd: "missing"},
		"cwd is a file":             {Cwd: "config/payload.json"},
		"invalid env name":          {Env: []string{"1BAD=x"}},
		"env without value":         {Env: []string{"REGION"}},
		"repeated env":              {Env: []string{"REGION=a", "REGION=b"}},
		"two stdin sources":         {StdinText: textInput("yes"), StdinFile: "config/payload.json"},
		"stdin file outside":        {StdinFile: "../outside.json"},
		"missing stdin file":        {StdinFile: "config/missing.json"},
	} {
		profile.WorkspaceID = "ws-1"
		profile.ScriptPath = "scripts/deploy.sh"
		profile.TargetKeys = []string{"cluster:east"}
		if _, err := BuildEffectScriptRule(profile, f.root); err == nil {
			t.Errorf("%s: profile accepted", name)
		}
	}
}

// The shell follows a symlink before it applies a later "..": from the root,
// link/../scripts/deploy.sh runs the deploy.sh beside the link's target, not
// scripts/deploy.sh, though the path cleans as text to the registered one.
// Such a call runs another file, so it keeps the unknown target and binds no
// observation, and a profile cannot be registered through such a path. A ".."
// after a real directory still names the registered script.
func TestEffectProfileReadsDotDotAsTheShellDoes(t *testing.T) {
	f := newEffectBindingFixture(t)
	for _, dir := range []string{"other/deep", "other/scripts", "other/config"} {
		if err := os.MkdirAll(filepath.Join(f.root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"scripts/deploy.sh", "scripts/state.sh", "config/payload.json"} {
		if err := os.WriteFile(filepath.Join(f.root, "other", name), []byte("#!/bin/sh\nprintf elsewhere\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(f.root, "other", "deep"), filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"east"},
		TargetKeys: []string{"cluster:east"}, AllowNetwork: true, ObservationCommand: "./scripts/state.sh east"})
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"west"},
		TargetKeys: []string{"cluster:west"}, AllowNetwork: true, Cwd: "link"})
	f.register(t, EffectScriptProfile{ScriptPath: "scripts/deploy.sh", Argv: []string{"south"},
		TargetKeys: []string{"cluster:south"}, AllowNetwork: true, StdinFile: "config/payload.json"})
	for _, call := range []struct{ cwd, command, want string }{
		{f.root, "./scripts/deploy.sh east", "cluster:east"},
		{f.root, "config/../scripts/deploy.sh east", "cluster:east"},
		{f.root, "link/../scripts/deploy.sh east", control.UnknownExternalTarget},
		{f.root, f.root + "/link/../scripts/deploy.sh east", control.UnknownExternalTarget},
		// From the linked directory, ".." is other/, not the workspace root.
		{"link", "../scripts/deploy.sh west", control.UnknownExternalTarget},
		{"link", f.root + "/scripts/deploy.sh west", "cluster:west"},
		// The shell opens a redirected file the same way.
		{f.root, "./scripts/deploy.sh south < config/payload.json", "cluster:south"},
		{f.root, "./scripts/deploy.sh south < link/../config/payload.json", control.UnknownExternalTarget},
	} {
		if got := f.targets(call.cwd, call.command); len(got) != 1 || got[0] != call.want {
			t.Errorf("%q in %s claimed %v, want %s", call.command, call.cwd, got, call.want)
		}
	}

	readRule, err := BuildObservationScriptRule(ObservationScriptProfile{WorkspaceID: "ws-1",
		ScriptPath: "scripts/state.sh", ArgvPrefix: []string{"east"}, AllowNetwork: true}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantApproval(context.Background(), "person", "default", "person-1", "person-1", readRule.Key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]bool{
		"./scripts/state.sh east":         true,
		"config/../scripts/state.sh east": true,
		"link/../scripts/state.sh east":   false,
		"./link/../scripts/state.sh east": false,
	} {
		_, bound := RegisteredEffectObservation(map[string]interface{}{"_tenant_id": "person-1", "cwd": f.root,
			"command": command, "_network_shared": true}, f.store)
		if bound != want {
			t.Errorf("watcher %q bound the effect claim = %v, want %v", command, bound, want)
		}
	}

	for name, profile := range map[string]EffectScriptProfile{
		"script":      {ScriptPath: "link/../scripts/deploy.sh"},
		"observation": {ScriptPath: "scripts/deploy.sh", ObservationCommand: "link/../scripts/state.sh east"},
		"stdin file":  {ScriptPath: "scripts/deploy.sh", StdinFile: "link/../config/payload.json"},
	} {
		profile.WorkspaceID = "ws-1"
		profile.TargetKeys = []string{"cluster:east"}
		if _, err := BuildEffectScriptRule(profile, f.root); err == nil {
			t.Errorf("%s reached through a symlink and \"..\" was registered", name)
		}
	}
}
