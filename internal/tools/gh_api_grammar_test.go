package tools

import (
	"reflect"
	"testing"
)

// `gh api` sends POST, not GET, when the call carries a body: a field
// (-f/--raw-field, -F/--field) or --input. gh reads its options with pflag, so
// a field may be bundled with a boolean (`-if title=x`) or carry its value
// attached (`-ftitle=x`). A remembered GET class released such a POST, and the
// bundled and attached spellings also passed the observation catalog.
func TestGhAPIBodyIsAPostInEveryLayer(t *testing.T) {
	for _, command := range []string{
		"gh api repos/o/r/issues -f title=x",
		"gh api repos/o/r/issues -F title=x",
		"gh api repos/o/r/issues --field title=x",
		"gh api repos/o/r/issues --raw-field=title=x",
		"gh api repos/o/r/issues -ftitle=x",
		"gh api repos/o/r/issues -Ftitle=x",
		"gh api repos/o/r/issues -if title=x",
		"gh api -if title=x repos/o/r/issues",
		"gh api repos/o/r/issues --input body.json",
		"gh api repos/o/r/issues --silent --raw-field title=x",
	} {
		args := map[string]interface{}{"command": command}
		if deterministicObservationExec("terminal", args) {
			t.Errorf("%q sends a body and must not be an observation", command)
		}
		prefix, ok := grantCommandPrefix("terminal", args)
		if !ok || !reflect.DeepEqual(prefix, []string{"gh", "api", "post", "repos/o/r/issues"}) {
			t.Errorf("%q: class %v (%v), want gh api post repos/o/r/issues", command, prefix, ok)
		}
	}
	// The constraint that must change the result: an explicit method wins over
	// the body, and a call with neither is a GET observation.
	for _, tc := range []struct {
		command     string
		observation bool
		prefix      []string
	}{
		{"gh api repos/o/r/issues", true, []string{"gh", "api", "get", "repos/o/r/issues"}},
		{"gh api -i repos/o/r/issues --paginate", true, []string{"gh", "api", "get", "repos/o/r/issues"}},
		{"gh api repos/o/r/issues -X GET -f per_page=100", false, []string{"gh", "api", "get", "repos/o/r/issues"}},
		{"gh api repos/o/r/issues -XPATCH -f state=closed", false, []string{"gh", "api", "patch", "repos/o/r/issues"}},
		{"gh api repos/o/r/issues -X=DELETE", false, []string{"gh", "api", "delete", "repos/o/r/issues"}},
	} {
		args := map[string]interface{}{"command": tc.command}
		if got := deterministicObservationExec("terminal", args); got != tc.observation {
			t.Errorf("observation(%q) = %v, want %v", tc.command, got, tc.observation)
		}
		if prefix, ok := grantCommandPrefix("terminal", args); !ok || !reflect.DeepEqual(prefix, tc.prefix) {
			t.Errorf("%q: class %v (%v), want %v", tc.command, prefix, ok, tc.prefix)
		}
	}
	// An option gh api does not have fails closed in both layers; a host other
	// than the default can receive an enterprise token, so it is no observation.
	for _, command := range []string{"gh api repos/o/r --frobnicate", "gh api repos/o/r -z"} {
		args := map[string]interface{}{"command": command}
		if deterministicObservationExec("terminal", args) {
			t.Errorf("%q must not be an observation", command)
		}
		if prefix, ok := grantCommandPrefix("terminal", args); ok {
			t.Errorf("%q minted class %v", command, prefix)
		}
	}
	if deterministicObservationExec("terminal", map[string]interface{}{"command": "gh api --hostname ghe.example.test repos/o/r"}) {
		t.Error("a call to another host must not be an observation")
	}
}

// End to end: a remembered `gh api get` class does not release a POST spelled
// any of gh's ways.
func TestRememberedGhGetNeverReleasesAPost(t *testing.T) {
	asks := &classAsks{}
	person, _ := classKeyScope(t, ApprovalOnRequest, asks)
	if ran, _ := runClassKeyCall(person, "gh api repos/o/r/issues"); !ran {
		t.Fatal("first call must run after the ask")
	}
	if got := asks.requests[0].GrantClass; got != `"gh api get repos/o/r/issues" commands` {
		t.Fatalf("label = %q", got)
	}
	for _, command := range []string{"gh api repos/o/r/issues -f title=x", "gh api repos/o/r/issues -ftitle=x", "gh api repos/o/r/issues -if title=x"} {
		before := len(asks.requests)
		_, _ = runClassKeyCall(person, command)
		if len(asks.requests) != before+1 {
			t.Errorf("%q was released by the remembered GET class", command)
		}
	}
	before := len(asks.requests)
	if ran, _ := runClassKeyCall(person, "gh api repos/o/r/issues --paginate"); !ran || len(asks.requests) != before {
		t.Error("a GET of the same resource must be released")
	}
}
