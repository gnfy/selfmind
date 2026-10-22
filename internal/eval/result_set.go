package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ResultSet struct {
	Version int         `json:"version"`
	Samples []RunResult `json:"samples"`
}

func (s *ResultSet) Add(result RunResult) { s.Samples = append(s.Samples, SamplesOf(result)...) }

// LoadResultSet reads scorecard JSON or raw eval JSONL. Directories select only
// JSONL, avoiding double-counting the sidecar and its original evidence.
func LoadResultSet(path string) (*ResultSet, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		result := &ResultSet{Version: 1}
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			part, err := LoadResultSet(p)
			if err == nil {
				result.Samples = append(result.Samples, part.Samples...)
			}
			return err
		})
		if err == nil && len(result.Samples) == 0 {
			err = fmt.Errorf("no eval samples in %s", path)
		}
		return result, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".json") {
		var result ResultSet
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		if result.Version != 1 || len(result.Samples) == 0 {
			return nil, fmt.Errorf("unsupported or empty eval result set: %s", path)
		}
		return &result, nil
	}
	return readJSONLSamples(path, data)
}

func readJSONLSamples(path string, data []byte) (*ResultSet, error) {
	result := &ResultSet{Version: 1}
	var block bytes.Buffer
	var first, last JSONLEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 65536), 8*1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var e JSONLEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if e.Type == "case_started" {
			if block.Len() != 0 {
				result.Samples = append(result.Samples, incompleteSample(path, first, last))
			}
			block.Reset()
			first = e
		}
		block.Write(scanner.Bytes())
		block.WriteByte('\n')
		last = e
		if e.Type != "case_finished" {
			continue
		}
		report, err := ReadReport(path, bytes.NewReader(block.Bytes()))
		if err != nil {
			return nil, err
		}
		result.Samples = append(result.Samples, RunResult{CaseID: e.CaseID, Status: e.Status, OutputPath: path,
			Duration: time.Duration(e.DurationMS) * time.Millisecond, ToolCalls: e.ToolCalls, ActionToolCalls: e.ActionToolCalls,
			ToolErrors: e.ToolErrors, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens,
			Checks: e.CheckResults, Efficiency: report.Efficiency, Provenance: report.Provenance})
		block.Reset()
		first = JSONLEvent{}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if block.Len() != 0 {
		result.Samples = append(result.Samples, incompleteSample(path, first, last))
	}
	if len(result.Samples) == 0 {
		return nil, fmt.Errorf("no eval samples in %s", path)
	}
	return result, nil
}

func incompleteSample(path string, first, last JSONLEvent) RunResult {
	start, _ := time.Parse(time.RFC3339Nano, first.Time)
	end, _ := time.Parse(time.RFC3339Nano, last.Time)
	var duration time.Duration
	if !start.IsZero() && !end.Before(start) {
		duration = end.Sub(start)
	}
	return RunResult{CaseID: first.CaseID, Status: "incomplete", OutputPath: path, Duration: duration,
		Error: "missing case_finished; retained as an unfinished sample"}
}
