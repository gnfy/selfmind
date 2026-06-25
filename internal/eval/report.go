package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type Report struct {
	Path                 string
	CaseCount            int
	Passed               int
	Failed               int
	TotalDurationMS      int64
	TotalInputTokens     int
	TotalOutputTokens    int
	FirstTokenCount      int
	TotalFirstTokenMS    int64
	TotalToolCalls       int
	TotalActionToolCalls int
	TotalToolErrors      int
	ErrorCategories      map[string]int
	Failures             []string
}

func LoadReport(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadReport(path, f)
}

func ReadReport(path string, r io.Reader) (*Report, error) {
	report := &Report{Path: path, ErrorCategories: map[string]int{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event JSONLEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse report event: %w", err)
		}
		switch event.Type {
		case "case_finished":
			report.CaseCount++
			report.TotalDurationMS += event.DurationMS
			report.TotalInputTokens += event.InputTokens
			report.TotalOutputTokens += event.OutputTokens
			report.TotalToolCalls += event.ToolCalls
			report.TotalActionToolCalls += event.ActionToolCalls
			report.TotalToolErrors += event.ToolErrors
			if event.Status == "passed" {
				report.Passed++
			} else {
				report.Failed++
				report.Failures = append(report.Failures, event.CaseID)
			}
			for _, check := range event.CheckResults {
				if !check.OK {
					report.ErrorCategories["check:"+check.Name]++
				}
			}
		case "tool_failed", "stream_error", "turn_finished":
			if event.ErrorCategory != "" {
				report.ErrorCategories[event.ErrorCategory]++
			}
		case "model_stream_first_token":
			if event.ElapsedMS > 0 {
				report.FirstTokenCount++
				report.TotalFirstTokenMS += event.ElapsedMS
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return report, nil
}

func (r *Report) String() string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Eval report: %s\n", r.Path)
	fmt.Fprintf(&sb, "Cases: %d  Passed: %d  Failed: %d", r.CaseCount, r.Passed, r.Failed)
	if r.CaseCount > 0 {
		fmt.Fprintf(&sb, "  Pass rate: %.1f%%", float64(r.Passed)*100/float64(r.CaseCount))
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "Duration: %s  Tokens: input=%d output=%d\n", time.Duration(r.TotalDurationMS)*time.Millisecond, r.TotalInputTokens, r.TotalOutputTokens)
	if r.FirstTokenCount > 0 || r.TotalToolCalls > 0 || r.TotalToolErrors > 0 {
		sb.WriteString("Runtime: ")
		if r.FirstTokenCount > 0 {
			fmt.Fprintf(&sb, "avg_first_token=%s", time.Duration(r.TotalFirstTokenMS/int64(r.FirstTokenCount))*time.Millisecond)
		}
		if r.TotalToolCalls > 0 || r.TotalToolErrors > 0 {
			if r.FirstTokenCount > 0 {
				sb.WriteString("  ")
			}
			fmt.Fprintf(&sb, "tools=%d action_tools=%d errors=%d", r.TotalToolCalls, r.TotalActionToolCalls, r.TotalToolErrors)
		}
		sb.WriteString("\n")
	}
	if len(r.ErrorCategories) > 0 {
		sb.WriteString("\nFailure categories:\n")
		for _, item := range sortedCounts(r.ErrorCategories) {
			fmt.Fprintf(&sb, "  - %s: %d\n", item.Key, item.Value)
		}
	}
	if len(r.Failures) > 0 {
		sb.WriteString("\nFailed cases:\n")
		for _, id := range r.Failures {
			fmt.Fprintf(&sb, "  - %s\n", id)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

type countItem struct {
	Key   string
	Value int
}

func sortedCounts(counts map[string]int) []countItem {
	out := make([]countItem, 0, len(counts))
	for k, v := range counts {
		out = append(out, countItem{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value == out[j].Value {
			return out[i].Key < out[j].Key
		}
		return out[i].Value > out[j].Value
	})
	return out
}
