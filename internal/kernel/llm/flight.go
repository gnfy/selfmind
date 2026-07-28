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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(metaPath, 0o600)
	latestPath := filepath.Join(FlightDir(), "latest")
	_ = os.WriteFile(latestPath, []byte(meta.TurnID), 0o600)
	_ = os.Chmod(latestPath, 0o600)
	PruneFlights(flightKeep())
	secureFlightTree(FlightDir())
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

// secureFlightTree upgrades recordings created by older releases. Flight
// cassettes can contain prompts, model output, and tool results, so neither the
// directories nor files should be readable by other local users.
func secureFlightTree(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}

// VCRSessionForTest exposes the context session for tests in dependent packages.
func VCRSessionForTest(ctx interface{ Value(any) any }) string {
	if v, ok := ctx.Value(vcrSessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}
