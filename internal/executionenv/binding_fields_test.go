package executionenv

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A Binding is stored as JSON on the durable row it governs. It carries two
// fields recorded at different times — the sandbox policy the registering Run
// froze, and the in-place host writes its approval covered — so a row written
// before either existed must keep reading as it was, and a field a row never
// recorded must not be written as if it had been.
func TestBindingCarriesTheFrozenPolicyAndTheApprovedWritesThroughJSON(t *testing.T) {
	registered := Binding{
		Version: BindingVersion, ID: "binding:watch", TenantID: "tenant", PersonID: "person",
		TrustLevel:       TrustTrusted,
		SandboxPolicy:    &SandboxPolicy{Enabled: true},
		ApprovedBoundary: "isolated",
		HostStateWrites:  []string{"/h/.aws/cli/cache", "/h/.aws/sso/cache"},
	}
	data, err := json.Marshal(registered)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"sandbox_policy":{"enabled":true,"required":false}`,
		`"approved_boundary":"isolated"`,
		`"host_state_writes":["/h/.aws/cli/cache","/h/.aws/sso/cache"]`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the binding must record %s, got %s", want, data)
		}
	}
	var loaded Binding
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, registered) {
		t.Fatalf("binding did not round trip:\n got %#v\nwant %#v", loaded, registered)
	}
}

func TestOldBindingRowsReadWithoutTheNewFieldsAndAreNotRewrittenWithThem(t *testing.T) {
	const identity = `"version":1,"id":"binding:old","tenant_id":"tenant","person_id":"person","trust_level":"trusted","execution_capabilities":["network:shared"]`
	for _, tc := range []struct {
		name         string
		row          string
		wantPolicy   *SandboxPolicy
		wantWrites   []string
		wantBoundary string
	}{
		// A row from before any of the fields was recorded.
		{name: "neither-field", row: `{` + identity + `}`},
		// A row that recorded the boundary its registration was approved for,
		// and neither of the others.
		{name: "boundary-only", row: `{` + identity + `,"approved_boundary":"host"}`, wantBoundary: "host"},
		// The column's default for a watch that predates bindings altogether.
		{name: "no-binding", row: `{}`},
		// A row that recorded the policy but not the writes.
		{name: "policy-only", row: `{` + identity + `,"sandbox_policy":{"enabled":true,"required":true,"allow_network":false}}`,
			wantPolicy: &SandboxPolicy{Enabled: true, Required: true}},
		// A row that recorded the writes but not the policy.
		{name: "writes-only", row: `{` + identity + `,"host_state_writes":["/h/.aws/sso/cache"]}`,
			wantWrites: []string{"/h/.aws/sso/cache"}},
		// A row written while the policy still carried a network flag. No
		// execution ever read it, so it is dropped on read, not carried on.
		{name: "policy-with-retired-network-flag", row: `{` + identity + `,"sandbox_policy":{"enabled":true,"required":false,"allow_network":true}}`,
			wantPolicy: &SandboxPolicy{Enabled: true}},
		// A policy that froze everything off is a recorded policy, not an absent one.
		{name: "policy-all-off", row: `{` + identity + `,"sandbox_policy":{"enabled":false,"required":false,"allow_network":false}}`,
			wantPolicy: &SandboxPolicy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loaded Binding
			if err := json.Unmarshal([]byte(tc.row), &loaded); err != nil {
				t.Fatalf("an old row must still read: %v", err)
			}
			if !reflect.DeepEqual(loaded.SandboxPolicy, tc.wantPolicy) {
				t.Fatalf("policy = %#v, want %#v", loaded.SandboxPolicy, tc.wantPolicy)
			}
			if !reflect.DeepEqual(loaded.HostStateWrites, tc.wantWrites) {
				t.Fatalf("host writes = %#v, want %#v", loaded.HostStateWrites, tc.wantWrites)
			}
			if loaded.ApprovedBoundary != tc.wantBoundary {
				t.Fatalf("approved boundary = %q, want %q", loaded.ApprovedBoundary, tc.wantBoundary)
			}
			rewritten, err := json.Marshal(loaded)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rewritten), "allow_network") {
				t.Fatalf("a retired network flag was written back: %s", rewritten)
			}
			if tc.wantBoundary == "" && strings.Contains(string(rewritten), "approved_boundary") {
				t.Fatalf("a row that recorded no boundary was rewritten as if it had: %s", rewritten)
			}
			if tc.wantPolicy == nil && strings.Contains(string(rewritten), "sandbox_policy") {
				t.Fatalf("a row that froze no policy was rewritten as if it had: %s", rewritten)
			}
			if len(tc.wantWrites) == 0 && strings.Contains(string(rewritten), "host_state_writes") {
				t.Fatalf("a row that approved no writes was rewritten as if it had: %s", rewritten)
			}
		})
	}
}
