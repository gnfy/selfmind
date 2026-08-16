package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

// CaptureOptions configures how a recorded flight turn becomes an eval case.
type CaptureOptions struct {
	Title  string // one-line human title / intent ("continuation should keep task")
	Suite  string // suite dir under evaldrafts/ (default "captured")
	VCRDir string // draft cassette root (default ".vcr-drafts")
	OutDir string // draft case output dir (default "evaldrafts/<suite>")
}

// CaptureResult reports where the new case + cassette landed.
type CaptureResult struct {
	CaseID    string
	CasePath  string
	VCRPath   string
	Cassettes int
}

// CaptureFromFlight promotes a recorded flight turn (see the flight recorder)
// into a replayable eval case: it copies the turn's cassette into the VCR dir
// and writes a case YAML draft seeded with the real prompt + P0 checks and a
// commented `assert_state` stub for the user to fill in ("what should have
// happened"). turnID may be "latest". This is the everyday "friction → permanent
// regression test" button.
func CaptureFromFlight(turnID string, opts CaptureOptions) (*CaptureResult, error) {
	if strings.TrimSpace(turnID) == "" || turnID == "latest" {
		turnID = llm.LatestFlightID()
	}
	if turnID == "" {
		return nil, fmt.Errorf("no recorded turn found; enable the flight recorder with SELFMIND_FLIGHT_RECORDER=1 and run at least one turn")
	}
	meta, err := llm.ReadFlightMeta(turnID)
	if err != nil {
		return nil, fmt.Errorf("read recorded turn %s: %w", turnID, err)
	}

	suite := firstNonEmpty(opts.Suite, "captured")
	vcrDir := firstNonEmpty(opts.VCRDir, ".vcr-drafts")
	outDir := firstNonEmpty(opts.OutDir, filepath.Join("evaldrafts", suite))
	caseID := slugifyCase(opts.Title, turnID)

	// Copy the recorded cassette (NNNN.json only, not meta.json) so the case
	// replays the EXACT model interaction offline.
	srcDir := llm.FlightCassetteDir(turnID)
	dstDir := filepath.Join(vcrDir, caseID)
	cassettes, err := copyCassettes(srcDir, dstDir)
	if err != nil {
		return nil, err
	}

	channel := firstNonEmpty(meta.Channel, "cli")
	yaml := renderCaseYAML(caseID, opts.Title, suite, channel, meta.Prompt)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	casePath := filepath.Join(outDir, caseID+".yaml")
	if err := os.WriteFile(casePath, []byte(yaml), 0o644); err != nil {
		return nil, err
	}
	return &CaptureResult{CaseID: caseID, CasePath: casePath, VCRPath: dstDir, Cassettes: cassettes}, nil
}

func copyCassettes(srcDir, dstDir string) (int, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, fmt.Errorf("read recorded cassette dir: %w", err)
	}
	type recording struct {
		name string
		data []byte
	}
	var recordings []recording
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "meta.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			return 0, err
		}
		var envelope struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return 0, fmt.Errorf("flight cassette %s is invalid JSON: %w", name, err)
		}
		if strings.TrimSpace(envelope.Error) != "" {
			return 0, fmt.Errorf("flight cassette %s records a provider failure (%s); rerun the task successfully before capture", name, envelope.Error)
		}
		recordings = append(recordings, recording{name: name, data: data})
	}
	if len(recordings) == 0 {
		return 0, fmt.Errorf("no cassette files in %s (was the turn recorded?)", srcDir)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return 0, err
	}
	for i, recording := range recordings {
		if err := os.WriteFile(filepath.Join(dstDir, recording.name), recording.data, 0o644); err != nil {
			return i, err
		}
	}
	return len(recordings), nil
}

func renderCaseYAML(caseID, title, suite, channel, prompt string) string {
	if strings.TrimSpace(title) == "" {
		title = "captured turn — describe the expected behavior"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Captured from a real run on %s. Edit assert_state/expect below to\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&b, "# encode WHAT SHOULD HAVE HAPPENED. Promote the reviewed YAML to evalcases/\n")
	fmt.Fprintf(&b, "# and its cassette to .vcr/ before it becomes release evidence.\n")
	fmt.Fprintf(&b, "id: %s\n", caseID)
	fmt.Fprintf(&b, "title: %q\n", title)
	fmt.Fprintf(&b, "suite: %s\n", suite)
	fmt.Fprintf(&b, "workspace: \".\"\n")
	fmt.Fprintf(&b, "channel: %s\n", channel)
	fmt.Fprintf(&b, "turns:\n  - channel: %s\n    input: |\n%s\n", channel, indentBlock(prompt, "      "))
	b.WriteString("expect:\n  status: completed\n  must_not_contain: [\"<tool>\", \"Process summary:\"]\n")
	b.WriteString("# assert_state lets you pin the desired terminal state, e.g.:\n")
	b.WriteString("# assert_state:\n")
	b.WriteString("#   - on: file\n#     path: \"hello.txt\"\n#     exists: true\n")
	b.WriteString("#   - on: task\n#     field: status\n#     exists: true\n")
	b.WriteString("checks:\n  no_mojibake: true\n  no_raw_json_leak: true\n  no_tool_xml_leak: true\n  no_empty_response: true\n")
	return b.String()
}

// slugifyCase derives a stable, VCR-safe case id from the title (preferred) or
// the turn id. The result uses only [a-z0-9_-.] so it survives VCR sanitization
// unchanged (cassette dir name == sanitized case id).
func slugifyCase(title, turnID string) string {
	base := strings.TrimSpace(strings.ToLower(title))
	var sb strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			sb.WriteByte('-')
		}
	}
	slug := strings.Trim(sb.String(), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	if slug == "" {
		return "captured-" + strings.TrimPrefix(turnID, "flight-")
	}
	return "captured-" + slug
}

func indentBlock(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}
