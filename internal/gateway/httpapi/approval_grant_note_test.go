package httpapi

import (
	"strings"
	"testing"
)

// A user who cannot see WHAT was remembered cannot notice when the class is
// wider than intended. The note must name the class, and must not claim a grant
// exists when the eligibility floor refused to persist one.
func TestGrantScopeNoteNamesTheClass(t *testing.T) {
	cases := []struct {
		name       string
		scope      string
		grantClass string
		want       string
		absent     string
	}{
		{
			name:       "person scope names class and window",
			scope:      "person",
			grantClass: `host execution of "gcloud" in this workspace`,
			want:       `remembered for you across tasks, 8h: host execution of "gcloud" in this workspace`,
		},
		{
			name:       "task scope names class",
			scope:      "task",
			grantClass: "invokes dangerous command: chmod",
			want:       "remembered for this task: invokes dangerous command: chmod",
		},
		{
			name:   "ineligible class is reported honestly",
			scope:  "person",
			want:   "not remembered",
			absent: "remembered for you across tasks",
		},
		{name: "once-only decision has no note", scope: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grantScopeNoteWithClass(tc.scope, tc.grantClass)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("expected no note, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("note %q must contain %q", got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("note %q must not contain %q", got, tc.absent)
			}
		})
	}
}
