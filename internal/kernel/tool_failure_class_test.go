package kernel

import "testing"

func TestClassifyToolFailurePrefersStructuredClass(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"Error executing terminal: command failed: exit status 1\nerror_class: credential_state_readonly; hint: The tool needs to write its OWN state", "credential_state_readonly"},
		{"Error executing terminal: command failed: exit status 1\nerror_class: sandbox_fs_denied; hint: The isolated sandbox denied a write", "sandbox_fs_denied"},
		// Regression: the generic hint text contains the word "permission", which
		// used to hijack the category to workspace_scope.
		{"open /etc/shadow: permission denied\nerror_class: permission; hint: The current user lacks permission for this path", "permission"},
		{"path /x escapes workspace allowed roots", "workspace_scope"},
		{"command failed: exit status 1", "command_failed"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		got, hint := classifyToolFailure(tc.msg)
		if got != tc.want {
			t.Fatalf("classifyToolFailure(%q) = %q, want %q", tc.msg, got, tc.want)
		}
		if hint == "" {
			t.Fatalf("class %q has empty hint", got)
		}
	}
}
