package router

import "testing"

// The `$` completion is a TUI affordance only. IM input is the least structured
// surface and has no completion to confirm a choice, so a message that merely
// contains `$name` stays ordinary text and never routes to a Skill.
func TestDollarMentionIsNotASkillRoute(t *testing.T) {
	classifier := NewIntentClassifier()
	for _, input := range []string{
		"$grilling",
		"$grilling stress-test my plan",
		"the build costs $500 to run",
		"export $PATH before you start",
	} {
		t.Run(input, func(t *testing.T) {
			if got := classifier.ClassifyDetailed(input); got.Intent == IntentSkill {
				t.Fatalf("dollar text routed to a Skill: %+v", got)
			}
		})
	}
}
