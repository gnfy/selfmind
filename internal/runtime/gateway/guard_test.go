package gateway

import "testing"

func TestGuardPublicBind(t *testing.T) {
	// Loopback without token: allowed.
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765"} {
		if err := guardPublicBind(addr, ""); err != nil {
			t.Errorf("loopback %q should be allowed without token: %v", addr, err)
		}
	}
	// Public / all-interfaces without token: refused.
	for _, addr := range []string{"0.0.0.0:8765", ":8765", "192.168.1.10:8765"} {
		if err := guardPublicBind(addr, ""); err == nil {
			t.Errorf("public %q must be refused without a token", addr)
		}
	}
	// Public WITH token: allowed (operator's choice).
	if err := guardPublicBind("0.0.0.0:8765", "secret"); err != nil {
		t.Errorf("public bind with token should be allowed: %v", err)
	}
}
