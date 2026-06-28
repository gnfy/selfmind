package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Flight recorder: when SELFMIND_FLIGHT_RECORDER is on, each real agent turn's
// model interaction is recorded (reusing the VCR record path) into a bounded,
// auto-pruned per-turn directory, alongside a meta.json describing the turn.
// `selfmind eval capture` later promotes a recorded turn into a replayable eval
// case — turning everyday friction into a permanent regression test, for free.
//
// This is distinct from SELFMIND_EVAL_VCR (which the eval harness drives): eval
// mode takes precedence; flight recording only kicks in for normal runs.

// FlightMeta describes one recorded turn.
type FlightMeta struct {
	TurnID    string `json:"turn_id"`
	TenantID  string `json:"tenant_id"`
	Channel   string `json:"channel"`
	Prompt    string `json:"prompt"`
	Output    string `json:"output"`
	CreatedAt string `json:"created_at"`
}

// FlightEnabled reports whether the flight recorder is on.
func FlightEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_FLIGHT_RECORDER"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// FlightDir is where recorded turns live (default ~/.selfmind/flight).
func FlightDir() string {
	if d := strings.TrimSpace(os.Getenv("SELFMIND_FLIGHT_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".flight"
	}
	return filepath.Join(home, ".selfmind", "flight")
}

func flightKeep() int {
	if v := strings.TrimSpace(os.Getenv("SELFMIND_FLIGHT_KEEP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 20
}

// WriteFlightMeta persists a turn's metadata and marks it as the latest, then
// prunes old turns. The cassette files (NNNN.json) are written separately by the
// VCR provider into the same <FlightDir>/<turnID>/ directory.
func WriteFlightMeta(meta FlightMeta) error {
	dir := filepath.Join(FlightDir(), sanitizeVCR(meta.TurnID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(FlightDir(), "latest"), []byte(meta.TurnID), 0o644)
	PruneFlights(flightKeep())
	return nil
}

// ReadFlightMeta loads a recorded turn's metadata.
func ReadFlightMeta(turnID string) (FlightMeta, error) {
	var m FlightMeta
	data, err := os.ReadFile(filepath.Join(FlightDir(), sanitizeVCR(turnID), "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// LatestFlightID returns the most recently recorded turn id, or "".
func LatestFlightID() string {
	data, err := os.ReadFile(filepath.Join(FlightDir(), "latest"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// FlightCassetteDir is the directory holding a turn's recorded cassette files.
func FlightCassetteDir(turnID string) string {
	return filepath.Join(FlightDir(), sanitizeVCR(turnID))
}

// PruneFlights keeps only the newest `keep` recorded turns (turn ids sort by
// time), deleting older directories. The `latest` pointer is preserved.
func PruneFlights(keep int) {
	root := FlightDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var turns []string
	for _, e := range entries {
		if e.IsDir() {
			turns = append(turns, e.Name())
		}
	}
	if len(turns) <= keep {
		return
	}
	sort.Strings(turns) // turn ids are monotonic (unix-nano based), so lexical == chronological
	for _, old := range turns[:len(turns)-keep] {
		_ = os.RemoveAll(filepath.Join(root, old))
	}
}

// VCRSessionForTest exposes the context session for tests in dependent packages.
func VCRSessionForTest(ctx interface{ Value(any) any }) string {
	if v, ok := ctx.Value(vcrSessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}
