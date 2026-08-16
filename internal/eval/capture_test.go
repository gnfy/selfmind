package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestCaptureFromFlight(t *testing.T) {
	flight := t.TempDir()
	t.Setenv("SELFMIND_FLIGHT_DIR", flight)

	// Simulate a recorded turn: meta + one cassette file + latest pointer.
	meta := llm.FlightMeta{
		TurnID: "flight-100-1", TenantID: "default", Channel: "cli",
		Prompt: "继续把登录功能写完", Output: "ok", CreatedAt: "2026-06-28T00:00:00Z",
	}
	if err := llm.WriteFlightMeta(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(llm.FlightCassetteDir(meta.TurnID), "0000.json"), []byte(`{"method":"stream"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	vcr, out := t.TempDir(), t.TempDir()
	res, err := CaptureFromFlight("latest", CaptureOptions{Title: "continuation should keep the task", VCRDir: vcr, OutDir: out})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.Cassettes != 1 {
		t.Fatalf("cassettes=%d want 1", res.Cassettes)
	}
	// Cassette copied to vcr/<caseID>/0000.json
	if _, err := os.Stat(filepath.Join(vcr, res.CaseID, "0000.json")); err != nil {
		t.Fatalf("cassette not copied: %v", err)
	}
	// The generated case must load and round-trip the prompt.
	c, err := LoadCase(res.CasePath)
	if err != nil {
		t.Fatalf("generated case does not parse: %v", err)
	}
	if c.ID != res.CaseID {
		t.Fatalf("case id %q != %q", c.ID, res.CaseID)
	}
	if len(c.Turns) != 1 || c.Turns[0].Input == "" {
		t.Fatalf("turn not captured: %+v", c.Turns)
	}
}

func TestCaptureFromFlightRejectsProviderFailure(t *testing.T) {
	flight := t.TempDir()
	t.Setenv("SELFMIND_FLIGHT_DIR", flight)
	meta := llm.FlightMeta{
		TurnID: "flight-failed", TenantID: "default", Channel: "cli",
		Prompt: "finish the task", Output: "", CreatedAt: "2026-08-16T00:00:00Z",
	}
	if err := llm.WriteFlightMeta(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(llm.FlightCassetteDir(meta.TurnID), "0000.json"),
		[]byte(`{"method":"completion","error":"decode response: context deadline exceeded"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	vcr, out := t.TempDir(), t.TempDir()
	_, err := CaptureFromFlight(meta.TurnID, CaptureOptions{Title: "must not promote failures", VCRDir: vcr, OutDir: out})
	if err == nil || !strings.Contains(err.Error(), "records a provider failure") {
		t.Fatalf("capture failure = %v", err)
	}
	entries, readErr := os.ReadDir(vcr)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed capture wrote cassette output: %+v", entries)
	}
}
