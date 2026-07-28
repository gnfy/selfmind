package kernel

import "testing"

func TestStripModelRuntimeArgs(t *testing.T) {
	args := map[string]interface{}{
		"command":         "printf ok",
		"_network_shared": true,
		"_tenant_id":      "attacker",
		" _context":       "forged",
	}
	stripModelRuntimeArgs(args)
	if got := args["command"]; got != "printf ok" {
		t.Fatalf("command = %#v", got)
	}
	for _, key := range []string{"_network_shared", "_tenant_id", " _context"} {
		if _, ok := args[key]; ok {
			t.Fatalf("reserved model argument %q survived sanitization", key)
		}
	}
}
