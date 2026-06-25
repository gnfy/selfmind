package eval

import (
	"strings"
	"testing"
)

func TestReadReportAggregatesCaseResults(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"model_stream_first_token","case_id":"a","elapsed_ms":250}`,
		`{"type":"case_finished","case_id":"a","status":"passed","duration_ms":1000,"input_tokens":10,"output_tokens":3,"tool_calls":1,"action_tool_calls":0}`,
		`{"type":"tool_failed","case_id":"b","error_category":"command_failed"}`,
		`{"type":"case_finished","case_id":"b","status":"failed","duration_ms":2000,"input_tokens":20,"output_tokens":5,"tool_calls":2,"action_tool_calls":2,"tool_errors":1,"check_results":[{"name":"no_mojibake","ok":false}]}`,
	}, "\n")
	report, err := ReadReport("inline.jsonl", strings.NewReader(input))
	if err != nil {
		t.Fatalf("ReadReport failed: %v", err)
	}
	if report.CaseCount != 2 || report.Passed != 1 || report.Failed != 1 {
		t.Fatalf("unexpected counts: %+v", report)
	}
	if report.TotalInputTokens != 30 || report.TotalOutputTokens != 8 {
		t.Fatalf("unexpected tokens: %+v", report)
	}
	if report.FirstTokenCount != 1 || report.TotalFirstTokenMS != 250 {
		t.Fatalf("unexpected first-token stats: %+v", report)
	}
	if report.TotalToolCalls != 3 || report.TotalToolErrors != 1 {
		t.Fatalf("unexpected tool stats: %+v", report)
	}
	if report.TotalActionToolCalls != 2 {
		t.Fatalf("unexpected action tool stats: %+v", report)
	}
	if report.ErrorCategories["command_failed"] != 1 {
		t.Fatalf("missing command_failed category: %+v", report.ErrorCategories)
	}
	if report.ErrorCategories["check:no_mojibake"] != 1 {
		t.Fatalf("missing check category: %+v", report.ErrorCategories)
	}
}
