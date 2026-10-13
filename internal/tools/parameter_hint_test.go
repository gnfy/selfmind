package tools

import (
	"errors"
	"strings"
	"testing"
)

func planEntryProperties() map[string]PropertyDef {
	return NewUpdatePlanTool().Schema().Properties["plan"].Items.Properties
}

// An unknown name close to a declared one asks whether it was meant, and never
// renames the argument: the model still has to resend a corrected call.
func TestUnknownParameterAsksWhichNearDeclaredNameWasMeant(t *testing.T) {
	properties := planEntryProperties()
	for _, tc := range []struct {
		name string
		want string // empty: no hint
	}{
		{"criterion", "did you mean success_criteria? "},
		{"criteria", "did you mean success_criteria? "},
		{"successCriteria", "did you mean success_criteria? "},
		{"success-criteria", "did you mean success_criteria? "},
		{"reason", "did you mean cancellation_reason or reuse_reason? "},
		{"statuses", "did you mean status? "},
		{"stepId", "did you mean step_id? "},
		// The same name run together: no word boundary to split on.
		{"stepid", "did you mean step_id? "},
		{"successcriteria", "did you mean success_criteria? "},
		{"step_ids", "did you mean step_id? "},
		{"work_units", "did you mean work_unit? "},
		// Synonyms and unrelated words stay unhinted: the hint is lexical.
		{"description", ""},
		{"title", ""},
		{"id", ""},
		{"cmd", ""},
	} {
		err := unknownParameterError("plan[1]", tc.name, properties)
		message := err.Error()
		prefix := "unknown parameter: plan[1]." + tc.name + " — "
		accepted := "this object accepts only " + acceptedParameters(properties)
		if want := prefix + tc.want + accepted; message != want {
			t.Errorf("%q:\n got  %s\n want %s", tc.name, message, want)
		}
	}
}

func TestUnknownParameterHintStaysQuietWhenAmbiguousOrNotAName(t *testing.T) {
	crowded := map[string]PropertyDef{}
	for _, prefix := range []string{"a", "b", "c", "d"} {
		crowded[prefix+"_reason"] = PropertyDef{Type: "string"}
	}
	if got := nearestParameterNames("reason", crowded); got != nil {
		t.Fatalf("four equally near names point nowhere: %v", got)
	}
	three := map[string]PropertyDef{"a_reason": {}, "b_reason": {}, "c_reason": {}}
	if got := nearestParameterNames("reason", three); strings.Join(got, ",") != "a_reason,b_reason,c_reason" {
		t.Fatalf("three near names are listed in order: %v", got)
	}
	if got := nearestParameterNames(strings.Repeat("reason", 12), planEntryProperties()); got != nil {
		t.Fatalf("an oversized name is not matched: %v", got)
	}
	if got := nearestParameterNames("", planEntryProperties()); got != nil {
		t.Fatalf("an empty name is not matched: %v", got)
	}
	// Short words carry no signal: "id" must not point at every *_id.
	if got := nearestParameterNames("id", planEntryProperties()); got != nil {
		t.Fatalf("a two-letter word matched %v", got)
	}
}

func TestNearWordKeepsTyposAndPluralsButNotDifferentWords(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"criterion", "criteria", true},
		{"reason", "reasoning", true},
		{"step", "steps", true},
		{"path", "patch", true},
		{"command", "commands", true},
		{"reason", "reuse", false},
		{"state", "status", false},
		{"target", "tag", false},
		{"files", "fields", false},
		{"unit", "unity", true},
		{"unit", "user", false},
		// Three edits is a different word at any length; a short prefix is not the same word.
		{"directory", "directive", false},
		{"plan", "planning", false},
		{"step", "stepwise", false},
	} {
		if got := nearWord(tc.a, tc.b); got != tc.want {
			t.Errorf("nearWord(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := nearWord(tc.b, tc.a); got != tc.want {
			t.Errorf("nearWord(%q, %q) = %v, want %v (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
	for input, want := range map[string]string{
		"successCriteria":  "success criteria",
		"success_criteria": "success criteria",
		"Success-Criteria": "success criteria",
		"stepID":           "step id",
		"work.unit id":     "work unit id",
	} {
		if got := strings.Join(parameterWords(input), " "); got != want {
			t.Errorf("parameterWords(%q) = %q, want %q", input, got, want)
		}
	}
}

// Both argument gates word the refusal the same way, and the stable facts that
// route recovery do not change: only the text gains the question.
func TestArgumentGatesHintNearNamesWithoutChangingTheRefusal(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewUpdatePlanTool())
	args := map[string]interface{}{"plan": []interface{}{
		map[string]interface{}{"step": "Check", "status": "pending", "criterion": "it works"},
	}}

	_, dispatchErr := registry.DispatchResult("update_plan", args)
	_, prepareErr := registry.PrepareToolArguments("update_plan", args)
	schema := NewUpdatePlanTool().Schema()
	_, coerceErr := CoerceArgs(schema, args)
	validateErr := ValidateArgs(schema, args)
	for gate, err := range map[string]error{"dispatch": dispatchErr, "prepare": prepareErr, "coerce": coerceErr, "validate": validateErr} {
		if err == nil {
			t.Fatalf("%s accepted an unknown plan field", gate)
		}
		for _, want := range []string{"unknown parameter: plan[0].criterion", "did you mean success_criteria? this object accepts only"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s refusal lacks %q: %s", gate, want, err)
			}
		}
		if gate == "coerce" || gate == "validate" {
			continue // the bare gates return the text; the registry wraps it with the stable facts
		}
		var recovery interface {
			ToolErrorCode() string
			ToolFailurePhase() string
			ToolEffectState() string
		}
		if !errors.As(err, &recovery) || recovery.ToolErrorCode() != "tool_arguments_invalid" ||
			recovery.ToolFailurePhase() != "preparation" || recovery.ToolEffectState() != "not_dispatched" {
			t.Errorf("%s refusal lost its stable pre-dispatch facts: %T %v", gate, err, err)
		}
	}
}
