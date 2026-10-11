package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// committedMessagesStorage keeps the messages the completed turn commits.
type committedMessagesStorage struct {
	mockStorage
	messages []llm.Message
}

func (s *committedMessagesStorage) IndexMessagesFromTrajectory(_ context.Context, _, _, _ string, messagesJSON []byte) error {
	var record struct {
		Messages []llm.Message `json:"messages"`
	}
	if err := json.Unmarshal(messagesJSON, &record); err != nil {
		return err
	}
	s.messages = record.Messages
	return nil
}

// The answer a refused call leaves when no repair hint is issued again.
const unavailableActionAnswer = "A necessary tool action was unavailable and was not executed. The remaining work is unresolved; inspect this run's refusal evidence before continuing."

// A final answer belongs to the assistant message that produced it. Refused
// calls are paired with tool results after that message, so "the last message"
// is a refusal, and writing the answer there erased the refusal evidence.
// Refused calls also end an answer cut by the output limit, which their
// response completes: the answer is the whole text, and a response that starts
// over replaces the cut part.
func TestFinalAnswerWithRefusalsKeepsTheirResults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nonStream bool
		refused   []string
		parts     [2]string
		want      string
	}{
		{"streamed continuation", false, []string{"w2"}, [2]string{"part one ", "part two"}, "part one part two"},
		{"non-streamed continuation, several refusals", true, []string{"w2", "w3"}, [2]string{"The deploy check ", "is still unfinished."}, "The deploy check is still unfinished."},
		{"non-streamed restart, several refusals", true, []string{"w2", "w3"}, [2]string{"The deploy check ", "The deploy check is still unfinished."}, "The deploy check is still unfinished."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := llm.ChatResponse{Content: tc.parts[1]}
			for _, id := range tc.refused {
				final.ToolCalls = append(final.ToolCalls, llm.ToolCall{ID: id, Function: "terminal", Args: `{}`})
			}
			provider := &boundaryProvider{nonStream: tc.nonStream, responses: []llm.ChatResponse{
				{ToolCalls: []llm.ToolCall{{ID: "w1", Function: "terminal", Args: `{}`}}},
				{Content: tc.parts[0], FinishReason: "length"},
				final,
			}}
			storage := &committedMessagesStorage{}
			agent := NewAgent(memory.NewMemoryManager(storage), &boundaryBackend{}, provider, "helpful", 6, 1, nil)
			strategy := DefaultTaskStrategy()
			strategy.AllowedTools = map[string]bool{"verify": true, "update_plan": true, "finish_run": true}
			answer, _, err := agent.RunConversation(WithTaskStrategy(context.Background(), strategy), "test", "cli", "check the deploy")
			if err != nil {
				t.Fatal(err)
			}
			if answer != tc.want {
				t.Fatalf("answer = %q, want %q", answer, tc.want)
			}
			assistant, refusals := -1, 0
			for i, message := range storage.messages {
				if message.Role == "assistant" && len(message.ToolCalls) == len(tc.refused) && message.ToolCalls[0].ID == tc.refused[0] {
					assistant = i
				}
				for _, id := range tc.refused {
					if message.Role == "tool" && message.ToolCallID == id {
						refusals++
						if strings.Contains(message.Content, tc.want) || !strings.Contains(message.Content, "not_dispatched") {
							t.Errorf("refusal %s was overwritten: %q", id, message.Content)
						}
					}
				}
			}
			if assistant < 0 || storage.messages[assistant].Content != tc.want {
				var tail []string
				for _, message := range storage.messages[1:] {
					tail = append(tail, message.Role+":"+message.ToolCallID+":"+message.Content)
				}
				t.Fatalf("the assistant message does not hold the answer: %q", tail)
			}
			if refusals != len(tc.refused) {
				t.Fatalf("committed %d refusals, want %d", refusals, len(tc.refused))
			}
		})
	}
}

// A continued answer belongs to one answer attempt. An answer the plan gate
// sends back is not kept, and an attempt superseded by an action tool is not
// either; prose written beside only bookkeeping calls stays part of the final
// answer, in order.
func TestContinuedAnswerBelongsToOneAnswerAttempt(t *testing.T) {
	const draftPlan = `{"plan":[{"step":"draft","status":"completed"},{"step":"verify","status":"in_progress"}]}`
	const donePlan = `{"plan":[{"step":"draft","status":"completed"},{"step":"verify","status":"completed"}]}`
	for _, tc := range []struct {
		name      string
		responses []llm.ChatResponse
		want      string
	}{
		{"gate sends back, model reconciles", []llm.ChatResponse{
			{ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: draftPlan}}},
			{Content: "Draft answer that ", FinishReason: "length"},
			{Content: "the gate sends back."},
			{ToolCalls: []llm.ToolCall{{ID: "p2", Function: "update_plan", Args: donePlan}}},
			{Content: "Final answer."},
		}, "Final answer."},
		{"gate sends back, model answers again", []llm.ChatResponse{
			{ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: draftPlan}}},
			{Content: "Draft answer that ", FinishReason: "length"},
			{Content: "the gate sends back."},
			{Content: "The verify step is still open."},
			{Content: "The verify step is still open."},
		}, "The verify step is still open."},
		{"action tool supersedes the attempt", []llm.ChatResponse{
			{Content: "Partial analysis ", FinishReason: "length"},
			{ToolCalls: []llm.ToolCall{{ID: "r1", Function: "read_file", Args: `{"path":"notes.txt"}`}}},
			{Content: "Final answer."},
		}, "Final answer."},
		{"bookkeeping keeps the continued prose in order", []llm.ChatResponse{
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two.", ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: donePlan}}},
			{Content: "Done."},
		}, "Part one part two.\n\nDone."},
		// A textless plan call does not interrupt an answer cut mid-word.
		{"textless bookkeeping keeps the cut answer pending", []llm.ChatResponse{
			{Content: "The impl", FinishReason: "length"},
			{ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: donePlan}}},
			{Content: "ementation is done."},
		}, "The implementation is done."},
		{"textless bookkeeping keeps an unspaced answer pending", []llm.ChatResponse{
			{Content: "部署已", FinishReason: "length"},
			{ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: donePlan}}},
			{Content: "完成。"},
		}, "部署已完成。"},
		// A model that starts its answer over replaces the cut part.
		{"restart after a textless plan call", []llm.ChatResponse{
			{Content: "The impl", FinishReason: "length"},
			{ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: donePlan}}},
			{Content: "The implementation is done."},
		}, "The implementation is done."},
		{"restart right after the cut", []llm.ChatResponse{
			{Content: "部署已", FinishReason: "length"},
			{Content: "部署已完成。"},
		}, "部署已完成。"},
		// A restart may lay the cut part out with other whitespace.
		{"restart that differs only in blank lines", []llm.ChatResponse{
			{Content: "Steps:\n\n1. Build", FinishReason: "length"},
			{Content: "Steps:\n1. Build the binary."},
		}, "Steps:\n1. Build the binary."},
		// A filtered reply ends the turn with what the attempt holds.
		{"restart in a filtered reply", []llm.ChatResponse{
			{Content: "The impl", FinishReason: "length"},
			{Content: "The implementation is", FinishReason: "content_filter"},
		}, "The implementation is"},
		{"filtered restart that differs only in indentation", []llm.ChatResponse{
			{Content: "Steps:\n  1. Build", FinishReason: "length"},
			{Content: "Steps:\n1. Build the binary.", FinishReason: "content_filter"},
		}, "Steps:\n1. Build the binary."},
		{"continuation in a filtered reply", []llm.ChatResponse{
			{Content: "The impl", FinishReason: "length"},
			{Content: "ementation is", FinishReason: "content_filter"},
		}, "The implementation is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &boundaryProvider{responses: tc.responses}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &projectingPlanBackend{}, provider, "helpful", 10, 1, nil)
			ctx := WithTaskStrategy(context.Background(), TaskStrategy{
				Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
				MaxActionTools: 6, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 6, MaxBudgetExtensions: 1,
			})
			answer, _, err := agent.RunConversation(ctx, "user123", "cli", "draft the answer and verify it")
			if err != nil {
				t.Fatal(err)
			}
			if answer != tc.want {
				t.Fatalf("answer = %q, want %q", answer, tc.want)
			}
		})
	}
}

// A refused or dropped call ends the answer attempt, whether or not its
// response earns a repair hint. A response with text of its own completes the
// pending cut answer, which stands as the whole text when no hint follows, as
// the same answer would without the cut; a textless one completes nothing, and
// the pending text is dropped. An incomplete call in a cut or filtered
// response also ends the attempt, and discards the pending text with that
// response: neither is a whole answer.
func TestRefusedDroppedOrIncompleteCallsEndTheAnswerAttempt(t *testing.T) {
	const donePlan = `{"plan":[{"step":"check","status":"completed"}]}`
	const overBudget = "I reached the tool budget before I could complete the remaining tool step. Based on the evidence already collected, I should stop here and state the next action instead of calling more tools."
	const filtered = "The model provider filtered the reply before its tool request was complete. No tools from that response were executed."
	refusing := DefaultTaskStrategy()
	refusing.AllowedTools = map[string]bool{"verify": true, "update_plan": true, "finish_run": true}
	budget := TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyOptional,
		MaxActionTools: 1, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 1,
	}
	open := TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyOptional,
		MaxActionTools: 6, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 6,
	}
	terminal := func(id string) []llm.ToolCall {
		return []llm.ToolCall{{ID: id, Function: "terminal", Args: `{"command":"make check"}`}}
	}
	read := func(id string) []llm.ToolCall {
		return []llm.ToolCall{{ID: id, Function: "read_file", Args: `{"path":"` + id + `.txt"}`}}
	}
	withPlan := func(calls []llm.ToolCall) []llm.ToolCall {
		return append([]llm.ToolCall{{ID: "p-" + calls[0].ID, Function: "update_plan", Args: donePlan}}, calls...)
	}
	for _, tc := range []struct {
		name      string
		strategy  TaskStrategy
		responses []llm.ChatResponse
		want      string
	}{
		{"refused", refusing, []llm.ChatResponse{
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two.", ToolCalls: terminal("t1")},
			{Content: "Final."},
		}, "Final."},
		{"dropped for budget", budget, []llm.ChatResponse{
			{ToolCalls: read("r1")},
			{Content: "Part one ", FinishReason: "length"},
			{ToolCalls: read("r2")},
			{Content: "Final."},
		}, "Final."},
		// No repair hint is issued a second time, so the response answers.
		{"refused after the repair hint", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two.", ToolCalls: terminal("t1")},
		}, "Part one part two."},
		{"dropped after the budget repair", budget, []llm.ChatResponse{
			{ToolCalls: read("r1")},
			{ToolCalls: read("r2")},
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two.", ToolCalls: read("r3")},
		}, "Part one part two."},
		{"deploy report cut once beside a stray refusal", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{Content: "Deploy report: staging is healthy; ", FinishReason: "length"},
			{Content: "production is still pending.", ToolCalls: terminal("t1")},
		}, "Deploy report: staging is healthy; production is still pending."},
		{"the same report uncut beside a stray refusal", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{Content: "Deploy report: staging is healthy; production is still pending.", ToolCalls: terminal("t1")},
		}, "Deploy report: staging is healthy; production is still pending."},
		// A textless response beside a plan call carries no fragment into
		// the later answer.
		{"textless plan call beside a refusal after a cut", refusing, []llm.ChatResponse{
			{Content: "Deploy report: staging is healthy; ", FinishReason: "length"},
			{ToolCalls: withPlan(terminal("t1"))},
			{Content: "I could not run make check here; the staging deploy is verified."},
		}, "I could not run make check here; the staging deploy is verified."},
		{"restart after a textless plan call beside a refusal", refusing, []llm.ChatResponse{
			{Content: "Deploy report: staging is healthy; ", FinishReason: "length"},
			{ToolCalls: withPlan(terminal("t1"))},
			{Content: "Deploy report: staging is healthy; production is still pending."},
		}, "Deploy report: staging is healthy; production is still pending."},
		{"textless plan call beside a dropped call after a cut", budget, []llm.ChatResponse{
			{ToolCalls: read("r1")},
			{Content: "Deploy report: staging is healthy; ", FinishReason: "length"},
			{ToolCalls: withPlan(read("r2"))},
			{Content: "Final."},
		}, "Final."},
		// The canned answer stands only for a response with nothing to say,
		// whatever cut text was pending.
		{"a textless refusal after the hint with a cut pending", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{Content: "Deploy report: staging is", FinishReason: "length"},
			{ToolCalls: terminal("t1")},
		}, unavailableActionAnswer},
		{"a textless drop after the budget repair with a cut pending", budget, []llm.ChatResponse{
			{ToolCalls: read("r1")},
			{ToolCalls: read("r2")},
			{Content: "Deploy report: staging is", FinishReason: "length"},
			{ToolCalls: read("r3")},
		}, overBudget},
		{"a textless refusal after the repair hint", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{ToolCalls: terminal("t1")},
		}, unavailableActionAnswer},
		{"a textless drop after the budget repair", budget, []llm.ChatResponse{
			{ToolCalls: read("r1")},
			{ToolCalls: read("r2")},
			{ToolCalls: read("r3")},
		}, overBudget},
		{"restarted beside a refused call", refusing, []llm.ChatResponse{
			{ToolCalls: terminal("t0")},
			{Content: "Part one ", FinishReason: "length"},
			{Content: "Part one, part two.", ToolCalls: terminal("t1")},
		}, "Part one, part two."},
		{"incomplete call in a cut response", open, []llm.ChatResponse{
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two ", ToolCalls: read("r1"), FinishReason: "length"},
			{Content: "Final."},
		}, "Final."},
		{"incomplete legacy call in a cut response", open, []llm.ChatResponse{
			{Content: "Part one ", FinishReason: "length"},
			{Content: `part two [TOOL:read_file:{"path":"`, FinishReason: "length"},
			{Content: "Final."},
		}, "Final."},
		{"incomplete call in a filtered response", open, []llm.ChatResponse{
			{Content: "Part one ", FinishReason: "length"},
			{Content: "part two ", ToolCalls: read("r1"), FinishReason: "content_filter"},
		}, filtered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &boundaryProvider{responses: tc.responses}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &planningBackend{}, provider, "helpful", 10, 1, nil)
			answer, _, err := agent.RunConversation(WithTaskStrategy(context.Background(), tc.strategy), "user123", "cli", "check the change")
			if err != nil {
				t.Fatal(err)
			}
			if answer != tc.want {
				t.Fatalf("answer = %q, want %q", answer, tc.want)
			}
		})
	}
}
