package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failOnceVCRProvider struct {
	chatCalls       int
	streamCalls     int
	completionCalls int
}

func (p *failOnceVCRProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	p.chatCalls++
	if p.chatCalls == 1 {
		return nil, errors.New("temporary chat failure")
	}
	return &ChatResponse{Content: "chat ok"}, nil
}

func (p *failOnceVCRProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	p.streamCalls++
	if p.streamCalls == 1 {
		return nil, errors.New("temporary stream failure")
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Content: "stream ok"}
	close(ch)
	return ch, nil
}

func (p *failOnceVCRProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	p.completionCalls++
	if p.completionCalls == 1 {
		return "", errors.New("temporary completion failure")
	}
	return "completion ok", nil
}

func writeCassetteFiles(t *testing.T, dir, session string, names ...string) {
	t.Helper()
	sess := filepath.Join(dir, sanitizeVCR(session))
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(sess, n), []byte(`{"method":"chat"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHasCassetteSessionStrict pins the gate-integrity contract: only a
// complete, gap-free 0000..max recording counts. Replay is position-keyed from
// 0000, so anything else can never replay and must not pass the gate.
func TestHasCassetteSessionStrict(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"complete", []string{"0000.json", "0001.json", "0002.json"}, true},
		{"single", []string{"0000.json"}, true},
		{"missing 0000 (observed corruption)", []string{"0001.json", "0002.json", "0003.json"}, false},
		{"gap in the middle", []string{"0000.json", "0002.json"}, false},
		{"no json at all", []string{"notes.txt"}, false},
		{"empty dir", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := "case_" + sanitizeVCR(tc.name)
			if len(tc.files) > 0 {
				writeCassetteFiles(t, dir, session, tc.files...)
			} else if tc.name == "empty dir" {
				if err := os.MkdirAll(filepath.Join(dir, sanitizeVCR(session)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := HasCassetteSession(dir, session); got != tc.want {
				t.Fatalf("HasCassetteSession(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}

	if HasCassetteSession(dir, "never-recorded") {
		t.Fatal("nonexistent session must not have a cassette")
	}
}

// TestResetVCRSessionRestartsNumbering pins the root-cause fix for the
// continuity_task_attach corruption: without a reset, a second run of the same
// case in one process continues numbering past the first run's files.
func TestResetVCRSessionRestartsNumbering(t *testing.T) {
	dir := t.TempDir()
	v := &vcrProvider{inner: nil, mode: "record", dir: dir}
	ctx := WithVCRSession(context.Background(), "case-x")

	p1, ok := v.nextKey(ctx)
	if !ok || filepath.Base(p1) != "0000.json" {
		t.Fatalf("first call key = %s, want 0000.json", p1)
	}
	p2, _ := v.nextKey(ctx)
	if filepath.Base(p2) != "0001.json" {
		t.Fatalf("second call key = %s, want 0001.json", p2)
	}

	// Re-run of the same case WITHOUT reset would continue at 0002 — the bug.
	ResetVCRSession("case-x")
	p3, _ := v.nextKey(ctx)
	if filepath.Base(p3) != "0000.json" {
		t.Fatalf("after ResetVCRSession key = %s, want 0000.json (fresh numbering)", p3)
	}
}

// TestWipeVCRSessionRecordings verifies record-mode hygiene: wiping removes the
// previous generation's files so a re-record never interleaves.
func TestWipeVCRSessionRecordings(t *testing.T) {
	dir := t.TempDir()
	writeCassetteFiles(t, dir, "case-y", "0001.json", "0002.json") // corrupt leftover
	if err := WipeVCRSessionRecordings(dir, "case-y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sanitizeVCR("case-y"))); !os.IsNotExist(err) {
		t.Fatalf("session dir should be removed, stat err = %v", err)
	}
	// Wiping a nonexistent session is a no-op, not an error.
	if err := WipeVCRSessionRecordings(dir, "case-y"); err != nil {
		t.Fatal(err)
	}
	// Empty session is a no-op guard.
	if err := WipeVCRSessionRecordings(dir, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteCassetteMakesWorkspacePortable(t *testing.T) {
	recordedRoot := filepath.Join(string(filepath.Separator), "tmp", "recorded-workspace")
	replayRoot := filepath.Join(string(filepath.Separator), "tmp", "replay-workspace")
	original := cassette{
		Method: "stream",
		Events: []recordedEvent{{
			Content: "Editing " + filepath.Join(recordedRoot, "main.go"),
			ToolCalls: []ToolCall{{
				ID: "call-1", Function: "write_file",
				Args: `{"path":"` + filepath.Join(recordedRoot, "main.go") + `"}`,
			}},
			Payload: map[string]interface{}{"cwd": recordedRoot},
		}},
	}

	stored := rewriteCassette(original, recordedRoot, vcrWorkspacePlaceholder)
	if strings.Contains(stored.Events[0].Content, recordedRoot) || !strings.Contains(stored.Events[0].Content, vcrWorkspacePlaceholder) {
		data, _ := json.Marshal(stored)
		t.Fatalf("stored cassette is not portable: %s", data)
	}

	replayed := rewriteCassette(stored, vcrWorkspacePlaceholder, replayRoot)
	if strings.Contains(replayed.Events[0].Content, recordedRoot) || !strings.Contains(replayed.Events[0].Content, replayRoot) {
		data, _ := json.Marshal(replayed)
		t.Fatalf("replayed cassette did not use current workspace: %s", data)
	}
}

func TestRewriteCassetteMapsWorkUnitIDsFromCurrentRequest(t *testing.T) {
	recordedA := "wu_11111111-1111-4111-8111-111111111111"
	recordedB := "wu_22222222-2222-4222-8222-222222222222"
	recordMessages := []Message{{Role: "tool", Name: "update_plan", Content: `{"work_units":[{"id":"` + recordedA + `"},{"id":"` + recordedB + `"}]}`}}
	original := cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{{
		ID: "call-1", Function: "skill_select", Args: `{"work_unit_id":"` + recordedB + `"}`,
	}}}}}
	stored := original
	for i, id := range vcrWorkUnitIDs(recordMessages) {
		stored = rewriteCassette(stored, id, vcrWorkUnitPlaceholder(i))
	}
	data, _ := json.Marshal(stored)
	if strings.Contains(string(data), recordedA) || strings.Contains(string(data), recordedB) || !strings.Contains(string(data), vcrWorkUnitPlaceholder(1)) {
		t.Fatalf("stored work-unit identity is not portable: %s", data)
	}

	replayA := "wu_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	replayB := "wu_bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	replayMessages := []Message{{Role: "tool", Name: "update_plan", Content: `{"work_units":[{"id":"` + replayA + `"},{"id":"` + replayB + `"}]}`}}
	replayed := stored
	for i, id := range vcrWorkUnitIDs(replayMessages) {
		replayed = rewriteCassette(replayed, vcrWorkUnitPlaceholder(i), id)
	}
	got := replayed.Events[0].ToolCalls[0].Args
	if !strings.Contains(got, replayB) || strings.Contains(got, recordedB) {
		t.Fatalf("replayed work-unit identity = %s", got)
	}
}

func TestRewriteCassetteMapsSkillCandidateRefsFromCurrentRequest(t *testing.T) {
	recordedA := "skref_1111111111111111"
	recordedB := "skref_2222222222222222"
	recordMessages := []Message{{Role: "tool", Name: "update_plan", Content: `{"work_units":[{"skill_catalog":"- ` + recordedA + ` alpha\n- ` + recordedB + ` beta"}]}`}}
	original := cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{{
		ID: "call-1", Function: "skill_select", Args: `{"candidate_ref":"` + recordedB + `"}`,
	}}}}}
	stored := original
	for i, ref := range vcrSkillCandidateRefs(recordMessages) {
		stored = rewriteCassette(stored, ref, vcrSkillCandidateRefPlaceholder(i))
	}
	data, _ := json.Marshal(stored)
	if strings.Contains(string(data), recordedA) || strings.Contains(string(data), recordedB) || !strings.Contains(string(data), vcrSkillCandidateRefPlaceholder(1)) {
		t.Fatalf("stored Skill candidate identity is not portable: %s", data)
	}

	replayA := "skref_aaaaaaaaaaaaaaaa"
	replayB := "skref_bbbbbbbbbbbbbbbb"
	replayMessages := []Message{{Role: "tool", Name: "update_plan", Content: `{"work_units":[{"skill_catalog":"- ` + replayA + ` alpha\n- ` + replayB + ` beta"}]}`}}
	replayed := stored
	for i, ref := range vcrSkillCandidateRefs(replayMessages) {
		replayed = rewriteCassette(replayed, vcrSkillCandidateRefPlaceholder(i), ref)
	}
	got := replayed.Events[0].ToolCalls[0].Args
	if !strings.Contains(got, replayB) || strings.Contains(got, recordedB) {
		t.Fatalf("replayed Skill candidate identity = %s", got)
	}
}

func TestRewriteCassetteMapsContinuityCandidateIDsFromCurrentRequest(t *testing.T) {
	recordedTask := "task_11111111-1111-4111-8111-111111111111"
	recordedRun := "run_22222222-2222-4222-8222-222222222222"
	recordMessages := []Message{{Role: "user", Content: `{"candidate_cards_json":[{"task_id":"` + recordedTask + `","run_id":"` + recordedRun + `"}]}`}}
	original := cassette{Method: "chat", Chat: &ChatResponse{ToolCalls: []ToolCall{{
		Function: "resolve_continuity", Args: `{"target_task_id":"` + recordedTask + `","target_run_id":"` + recordedRun + `"}`,
	}}}}
	stored := original
	for i, id := range vcrTaskIDs(recordMessages) {
		stored = rewriteCassette(stored, id, vcrTaskIDPlaceholder(i))
	}
	for i, id := range vcrRunIDs(recordMessages) {
		stored = rewriteCassette(stored, id, vcrRunIDPlaceholder(i))
	}
	data, _ := json.Marshal(stored)
	if strings.Contains(string(data), recordedTask) || strings.Contains(string(data), recordedRun) {
		t.Fatalf("stored continuity identity is not portable: %s", data)
	}

	replayTask := "task_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	replayRun := "run_bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	replayMessages := []Message{{Role: "user", Content: `{"candidate_cards_json":[{"task_id":"` + replayTask + `","run_id":"` + replayRun + `"}]}`}}
	replayed := stored
	for i, id := range vcrTaskIDs(replayMessages) {
		replayed = rewriteCassette(replayed, vcrTaskIDPlaceholder(i), id)
	}
	for i, id := range vcrRunIDs(replayMessages) {
		replayed = rewriteCassette(replayed, vcrRunIDPlaceholder(i), id)
	}
	got := replayed.Chat.ToolCalls[0].Args
	if !strings.Contains(got, replayTask) || !strings.Contains(got, replayRun) {
		t.Fatalf("replayed continuity identity = %s", got)
	}
}

func TestRecordModePreservesImmediateFailuresInSequence(t *testing.T) {
	dir := t.TempDir()
	inner := &failOnceVCRProvider{}
	v := &vcrProvider{inner: inner, mode: "record", dir: dir}
	ctx := WithVCRSession(context.Background(), "failure-gap")
	ResetVCRSession("failure-gap")

	if _, err := v.Chat(ctx, ChatRequest{}); err == nil {
		t.Fatal("first chat should fail")
	}
	if _, err := v.Chat(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.StreamChat(ctx, ChatRequest{}); err == nil {
		t.Fatal("first stream should fail")
	}
	stream, err := v.StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if _, err := v.ChatCompletion(ctx, nil); err == nil {
		t.Fatal("first completion should fail")
	}
	if _, err := v.ChatCompletion(ctx, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"0000.json", "0001.json", "0002.json", "0003.json", "0004.json", "0005.json"} {
		if _, err := os.Stat(filepath.Join(dir, "failure-gap", name)); err != nil {
			t.Fatalf("missing contiguous cassette %s: %v", name, err)
		}
	}
	if !HasCassetteSession(dir, "failure-gap") {
		t.Fatal("failed and successful calls must form one complete cassette")
	}

	ResetVCRSession("failure-gap")
	replay := &vcrProvider{inner: nil, mode: "replay", dir: dir, offline: true}
	if _, err := replay.Chat(ctx, ChatRequest{}); err == nil || err.Error() != "temporary chat failure" {
		t.Fatalf("replayed chat failure = %v", err)
	}
	if resp, err := replay.Chat(ctx, ChatRequest{}); err != nil || resp == nil || resp.Content != "chat ok" {
		t.Fatalf("replayed chat success = %#v, %v", resp, err)
	}
	if _, err := replay.StreamChat(ctx, ChatRequest{}); err == nil || err.Error() != "temporary stream failure" {
		t.Fatalf("replayed stream failure = %v", err)
	}
	stream, err = replay.StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var streamed string
	for event := range stream {
		streamed += event.Content
	}
	if streamed != "stream ok" {
		t.Fatalf("replayed stream = %q", streamed)
	}
	if _, err := replay.ChatCompletion(ctx, nil); err == nil || err.Error() != "temporary completion failure" {
		t.Fatalf("replayed completion failure = %v", err)
	}
	if text, err := replay.ChatCompletion(ctx, nil); err != nil || text != "completion ok" {
		t.Fatalf("replayed completion success = %q, %v", text, err)
	}
}
