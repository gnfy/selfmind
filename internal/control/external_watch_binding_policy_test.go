package control

import (
	"context"
	"reflect"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

// The watch row stores its execution binding as JSON. The two things a poll is
// held to — the sandbox policy its registering Run froze and the in-place host
// writes the registration's approval covered — must survive that round trip
// through every read path, and a row stored before either existed must keep
// loading with neither (a poll then follows the configured policy and covers no
// writes).
func TestExternalWatchStoresTheFrozenPolicyAndTheApprovedWrites(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	register := func(binding executionenv.Binding) *ExternalWatch {
		t.Helper()
		binding.Version = executionenv.BindingVersion
		created, err := store.CreateExternalWatch(ctx, ExternalWatch{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
			Channel: "cli", CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
			TimeoutAt: time.Now().Add(time.Hour), ExecutionBinding: binding,
		})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	load := func(id string) executionenv.Binding {
		t.Helper()
		loaded, err := store.GetExternalWatch(ctx, identity.TenantID, id)
		if err != nil || loaded == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		return loaded.ExecutionBinding
	}
	frozen := &executionenv.SandboxPolicy{Enabled: true}
	writes := []string{"/h/.aws/cli/cache", "/h/.aws/sso/cache"}

	registered := register(executionenv.Binding{SandboxPolicy: frozen, ApprovedBoundary: "isolated", HostStateWrites: writes})
	got := load(registered.ID)
	if !reflect.DeepEqual(got.SandboxPolicy, frozen) || got.ApprovedBoundary != "isolated" || !reflect.DeepEqual(got.HostStateWrites, writes) {
		t.Fatalf("the registration's policy and writes did not round trip: %#v", got)
	}
	// The same through the scan the poller claims due watches with.
	due, err := store.ListDueExternalWatches(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var polled *ExternalWatch
	for index := range due {
		if due[index].ID == registered.ID {
			polled = &due[index]
		}
	}
	if polled == nil || !reflect.DeepEqual(polled.ExecutionBinding.SandboxPolicy, frozen) || polled.ExecutionBinding.ApprovedBoundary != "isolated" ||
		!reflect.DeepEqual(polled.ExecutionBinding.HostStateWrites, writes) {
		t.Fatalf("the poller did not read the frozen policy and writes back: %#v", polled)
	}

	// A registration that approved no in-place writes records none; a row that
	// froze no policy (an early binding) reads back as having none.
	none := register(executionenv.Binding{})
	if got := load(none.ID); got.SandboxPolicy != nil || got.ApprovedBoundary != "" || len(got.HostStateWrites) != 0 {
		t.Fatalf("a registration that froze nothing read back as having: %#v", got)
	}

	// Rows stored by earlier versions: the binding column holds the JSON they
	// wrote, with neither field, or with only the one they knew.
	identityJSON := `"version":1,"id":"binding:` + registered.ID + `","tenant_id":"` + identity.TenantID +
		`","person_id":"` + identity.PersonID + `","trust_level":"trusted","execution_capabilities":["network:shared"]`
	for _, tc := range []struct {
		name         string
		row          string
		wantPolicy   *executionenv.SandboxPolicy
		wantWrites   []string
		wantBoundary string
	}{
		{name: "before-any-field", row: `{` + identityJSON + `}`},
		// A policy written while it still carried a network flag nothing read.
		{name: "policy-only", row: `{` + identityJSON + `,"sandbox_policy":{"enabled":true,"required":true,"allow_network":false}}`,
			wantPolicy: &executionenv.SandboxPolicy{Enabled: true, Required: true}},
		{name: "policy-with-retired-network-flag", row: `{` + identityJSON + `,"sandbox_policy":{"enabled":true,"required":false,"allow_network":true}}`,
			wantPolicy: &executionenv.SandboxPolicy{Enabled: true}},
		{name: "writes-only", row: `{` + identityJSON + `,"host_state_writes":["/h/.aws/sso/cache"]}`,
			wantWrites: []string{"/h/.aws/sso/cache"}},
		{name: "policy-and-writes-without-boundary", row: `{` + identityJSON + `,"sandbox_policy":{"enabled":true,"required":false},"host_state_writes":["/h/.aws/sso/cache"]}`,
			wantPolicy: &executionenv.SandboxPolicy{Enabled: true}, wantWrites: []string{"/h/.aws/sso/cache"}},
		{name: "boundary-only", row: `{` + identityJSON + `,"approved_boundary":"host"}`, wantBoundary: "host"},
		{name: "default-column-value", row: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := register(executionenv.Binding{})
			if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET execution_binding_json = ? WHERE id = ?`, tc.row, old.ID); err != nil {
				t.Fatal(err)
			}
			got := load(old.ID)
			if !reflect.DeepEqual(got.SandboxPolicy, tc.wantPolicy) || !reflect.DeepEqual(got.HostStateWrites, tc.wantWrites) || got.ApprovedBoundary != tc.wantBoundary {
				t.Fatalf("an older row read back as policy=%#v writes=%#v boundary=%q, want policy=%#v writes=%#v boundary=%q",
					got.SandboxPolicy, got.HostStateWrites, got.ApprovedBoundary, tc.wantPolicy, tc.wantWrites, tc.wantBoundary)
			}
			// Reading the row never records the fields it lacked.
			listed, err := store.ListExternalWatchesForPerson(ctx, identity.TenantID, identity.PersonID, "", 50, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, watch := range listed {
				if watch.ID == old.ID && (!reflect.DeepEqual(watch.ExecutionBinding.SandboxPolicy, tc.wantPolicy) ||
					!reflect.DeepEqual(watch.ExecutionBinding.HostStateWrites, tc.wantWrites)) {
					t.Fatalf("the listing read the older row as %#v", watch.ExecutionBinding)
				}
			}
		})
	}
}
