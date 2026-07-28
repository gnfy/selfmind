package tools

import (
	"testing"
	"time"
)

func TestResolveToolProfileDefaultsAndBounds(t *testing.T) {
	standard, err := resolveToolProfile(map[string]interface{}{}, 45)
	if err != nil {
		t.Fatal(err)
	}
	if standard.Class != ToolExecutionStandard || standard.Timeout != 45*time.Second {
		t.Fatalf("standard profile = %#v", standard)
	}

	longRunning, err := resolveToolProfile(map[string]interface{}{
		"execution_class": "long-running",
	}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if longRunning.Timeout != 30*time.Minute || longRunning.HeartbeatInterval != 10*time.Second {
		t.Fatalf("long-running profile = %#v", longRunning)
	}

	clamped, err := resolveToolProfile(map[string]interface{}{
		"execution_class": "long-running",
		"timeout":         float64(24 * 60 * 60),
	}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if clamped.Timeout != 2*time.Hour {
		t.Fatalf("long-running timeout = %s, want 2h", clamped.Timeout)
	}
	if !clamped.TimeoutClamped || clamped.RequestedTimeout != 24*time.Hour {
		t.Fatalf("clamp metadata = %#v", clamped)
	}
	if summary := timeoutSummary(clamped); summary != "7200 seconds (requested 86400 seconds; clamped by long-running execution_class maximum)" {
		t.Fatalf("timeout summary = %q", summary)
	}
}

func TestResolveToolProfileRejectsUnknownClass(t *testing.T) {
	if _, err := resolveToolProfile(map[string]interface{}{
		"execution_class": "agent-vendor-specific",
	}, 30); err == nil {
		t.Fatal("unknown execution class should fail closed")
	}
}

func TestToolExecutionClassPropertyIsVendorNeutral(t *testing.T) {
	property := toolExecutionClassProperty()
	if property.Default != "standard" {
		t.Fatalf("default = %#v", property.Default)
	}
	if len(property.Enum) != 3 {
		t.Fatalf("execution class enum = %#v", property.Enum)
	}
}
