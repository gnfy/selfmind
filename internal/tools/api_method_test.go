package tools

import (
	"reflect"
	"strings"
	"testing"
)

// The observation catalog and the remembered-class derivation must agree on the
// HTTP verb of a `gh api` call. They once parsed it separately: the catalog
// matched the substring "-x " and so read `-X=DELETE` and `-XDELETE` as
// ordinary GETs, while the class derivation labelled `-XDELETE` a GET. A
// destructive call could therefore be auto-approved as pure observation AND
// remembered under a read-only class name, which would then release further
// deletes.
//
// The spellings below are the same request written five ways. Every mutating
// one must be gated and must carry its real verb into the class; the read must
// stay an observation, or the fix has simply blocked `gh api` altogether.
func TestGhAPIVerbAgreesAcrossApprovalLayers(t *testing.T) {
	cases := []struct {
		command     string
		observation bool
		prefix      []string
	}{
		{`gh api -X DELETE repos/o/r/git/refs/heads/topic`, false, []string{"gh", "api", "delete", "repos/o/r/git"}},
		{`gh api -X=DELETE repos/o/r/git/refs/heads/topic`, false, []string{"gh", "api", "delete", "repos/o/r/git"}},
		{`gh api -XDELETE repos/o/r/git/refs/heads/topic`, false, []string{"gh", "api", "delete", "repos/o/r/git"}},
		{`gh api --method DELETE repos/o/r/git/refs/heads/topic`, false, []string{"gh", "api", "delete", "repos/o/r/git"}},
		{`gh api --method=DELETE repos/o/r/git/refs/heads/topic`, false, []string{"gh", "api", "delete", "repos/o/r/git"}},
		// Other verbs, other resources: the invariant is the verb, not this URL.
		{`gh api -XPATCH repos/o/r/pulls/7`, false, []string{"gh", "api", "patch", "repos/o/r/pulls"}},
		{`gh api --method=PUT orgs/acme/teams/x/repos`, false, []string{"gh", "api", "put", "orgs/acme/teams/x"}},
		{`gh api -X POST repos/o/r/dispatches`, false, []string{"gh", "api", "post", "repos/o/r/dispatches"}},
		// The constraint that must change the result: an actual read is still an
		// observation, and its class says so.
		{`gh api repos/o/r/pulls`, true, []string{"gh", "api", "get", "repos/o/r/pulls"}},
		{`gh api -X GET repos/o/r/pulls`, true, []string{"gh", "api", "get", "repos/o/r/pulls"}},
	}
	for _, tc := range cases {
		args := map[string]interface{}{"command": tc.command}
		if got := deterministicObservationExec("terminal", args); got != tc.observation {
			t.Errorf("observation(%q) = %v, want %v", tc.command, got, tc.observation)
		}
		prefix, ok := grantCommandPrefix("terminal", args)
		if !ok {
			t.Errorf("grantCommandPrefix(%q) is ineligible; a gh api call must still be rememberable by verb", tc.command)
			continue
		}
		if !reflect.DeepEqual(prefix, tc.prefix) {
			t.Errorf("prefix(%q) = %v, want %v", tc.command, prefix, tc.prefix)
		}
	}
}

// A resource path spelled like an HTTP verb must not be mistaken for the value
// of a method flag: dropping it leaves a class with no path, which covers every
// path that verb can reach.
func TestGhAPIPathSpelledLikeVerbSurvives(t *testing.T) {
	prefix, ok := grantCommandPrefix("terminal", map[string]interface{}{
		"command": `gh api -X POST repos/o/r/delete`,
	})
	if !ok {
		t.Fatal("expected an eligible class")
	}
	if want := []string{"gh", "api", "post", "repos/o/r/delete"}; !reflect.DeepEqual(prefix, want) {
		t.Fatalf("prefix = %v, want %v", prefix, want)
	}
}

// An unresolvable verb is a mutation. A trailing method flag with no value must
// not fall back to the GET default in either layer.
func TestGhAPIUnresolvableVerbFailsClosed(t *testing.T) {
	for _, command := range []string{`gh api repos/o/r -X`, `gh api repos/o/r --method`, `gh api repos/o/r --method=`} {
		args := map[string]interface{}{"command": command}
		if deterministicObservationExec("terminal", args) {
			t.Errorf("%q must not be an observation", command)
		}
		if prefix, ok := grantCommandPrefix("terminal", args); ok {
			t.Errorf("%q must not be grant-eligible, got %v", command, prefix)
		}
	}
}

// ghAPIRequest is the single parser both layers call. Pin its contract
// directly so a future caller cannot be added against a misremembered one.
func TestGhAPIRequest(t *testing.T) {
	cases := []struct {
		args     string
		method   string
		body     bool
		endpoint string
		ok       bool
	}{
		{"repos/o/r", "get", false, "repos/o/r", true},
		{"-X delete repos/o/r", "delete", false, "repos/o/r", true},
		{"-X=delete repos/o/r", "delete", false, "repos/o/r", true},
		{"-Xdelete repos/o/r", "delete", false, "repos/o/r", true},
		{"--method delete repos/o/r", "delete", false, "repos/o/r", true},
		{"--method=delete repos/o/r", "delete", false, "repos/o/r", true},
		{"repos/o/r -f a=1", "post", true, "repos/o/r", true},
		{"-fa=1 repos/o/r", "post", true, "repos/o/r", true},
		{"-if a=1 repos/o/r", "post", true, "repos/o/r", true},
		{"repos/o/r --input body.json", "post", true, "repos/o/r", true},
		{"-X GET -f a=1 repos/o/r", "get", true, "repos/o/r", true},
		{"-q .x -H Accept:json repos/o/r", "get", false, "repos/o/r", true},
		{"repos/o/r -X", "", false, "", false},
		{"repos/o/r -X -q", "", false, "", false},
		{"repos/o/r --method=", "", false, "", false},
		{"repos/o/r -z", "", false, "", false},
	}
	for _, tc := range cases {
		call, ok := ghAPIRequest(strings.Fields(tc.args))
		if ok != tc.ok || (ok && (call.method != tc.method || call.body != tc.body || call.endpoint != tc.endpoint)) {
			t.Errorf("ghAPIRequest(%q) = (%+v, %v), want method %q body %v endpoint %q ok %v",
				tc.args, call, ok, tc.method, tc.body, tc.endpoint, tc.ok)
		}
	}
}
