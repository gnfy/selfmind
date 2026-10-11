package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
	"selfmind/internal/tools/mcptest"
)

// parityHarness builds the daemon's agent workers through InitGateway against
// a scripted OpenAI-compatible endpoint, a real cron scheduler, and the stdio
// MCP stub. Nothing below InitGateway is replaced.
type parityHarness struct {
	deps     *GatewayDeps
	store    *control.Store
	model    *scriptedModelEndpoint
	startLog string
	echoTool string
}

func newParityHarness(t *testing.T, workers int, mcpServer string, script scriptedModelScript) *parityHarness {
	t.Helper()
	h, opts := parityGatewayOptions(t, workers, mcpServer, script)
	deps, err := InitGateway(opts)
	if err != nil {
		t.Fatalf("InitGateway: %v", err)
	}
	t.Cleanup(deps.Close)
	if len(deps.Workers) != workers {
		t.Fatalf("InitGateway built %d workers, want %d", len(deps.Workers), workers)
	}
	h.deps = deps
	return h
}

// parityGatewayOptions is the daemon configuration the parity harness starts:
// a scripted model, cron, Skills, and the MCP stub as mcpServer. An empty
// mcpServer configures no MCP server.
func parityGatewayOptions(t *testing.T, workers int, mcpServer string, script scriptedModelScript) (*parityHarness, GatewayOptions) {
	t.Helper()
	root := t.TempDir()
	model := newScriptedModelEndpoint(t, script)
	cfg := &config.Config{}
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Evolution.SkillsDir = filepath.Join(root, "skills")
	cfg.Evolution.Enabled = true
	cfg.Cron.Enabled = true
	cfg.Providers.Custom = []config.CustomProvider{{
		Name: "parity", BaseURL: model.server.URL + "/v1", Protocol: "openai-compatible", Auth: "none", Model: "parity-model",
	}}
	cfg.SetPrimaryModel("parity", "parity-model", "")
	h := &parityHarness{model: model, store: controltest.NewStore(t)}
	if mcpServer != "" {
		h.startLog = filepath.Join(root, "mcp-starts.log")
		h.echoTool = tools.MCPToolLocalName(mcpServer, mcptest.EchoTool)
		cfg.MCP.Servers = []config.MCP_SERVER{{
			Name: mcpServer, Transport: "stdio", Command: mcptest.Build(t), Args: mcptest.StartLogArgs(h.startLog),
		}}
	}
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return h, GatewayOptions{
		DataDir: cfg.Storage.DataDir, Memory: memory.NewMemoryManager(nil), Config: cfg,
		TenantID: control.DefaultTenantID, Control: h.store, Workers: workers,
	}
}

// Every worker exposes the same catalogue, including the cron tool and the
// MCP tools, and all of them share one server process.
func TestInitGatewayBuildsWorkersWithIdenticalCatalogsAndOneMCPConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workers int
		server  string
	}{
		{"two workers", 2, "stub"},
		{"three workers with another server name", 3, "parity-probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParityHarness(t, tc.workers, tc.server, answerEveryRequest)
			primary := workerCatalog(h.deps.Workers[0].Dispatcher)
			for _, name := range []string{"cron", h.echoTool, tools.MCPToolLocalName(tc.server, mcptest.RendezvousTool)} {
				if _, ok := primary[name]; !ok {
					t.Fatalf("primary catalogue lacks %s", name)
				}
			}
			for index, worker := range h.deps.Workers[1:] {
				if diff := describeCatalogDifference(primary, workerCatalog(worker.Dispatcher)); diff != "" {
					t.Fatalf("worker %d catalogue differs from the primary: %s", index+2, diff)
				}
			}
			if starts := mcptest.Starts(t, h.startLog); len(starts) != 1 {
				t.Fatalf("MCP server processes for %d workers = %d (%v), want one shared process", tc.workers, len(starts), starts)
			}
			if health := h.deps.MCP.Health(); health.Configured != 1 || health.Connected != 1 {
				t.Fatalf("MCP health = %+v", health)
			}
		})
	}
}

// The startup comparison rejects a worker whose catalogue differs in names or
// in schemas, and names the worker and the tool.
func TestWorkerCatalogValidationRejectsMissingExtraAndChangedTools(t *testing.T) {
	h := newParityHarness(t, 2, "stub", answerEveryRequest)
	if err := validateIdenticalWorkers(h.deps.Workers, h.deps.MCP, h.deps.skills); err != nil {
		t.Fatalf("identical workers rejected: %v", err)
	}
	worker := h.deps.Workers[1].Dispatcher

	worker.UnregisterTool("cron")
	err := validateIdenticalWorkers(h.deps.Workers, h.deps.MCP, h.deps.skills)
	if err == nil || !strings.Contains(err.Error(), "worker 2") || !strings.Contains(err.Error(), "missing: cron") {
		t.Fatalf("missing cron tool error = %v", err)
	}
	original, _ := h.deps.Workers[0].Dispatcher.GetTool("cron")
	worker.RegisterTool(original)

	worker.RegisterTool(catalogProbeTool{name: "worker_only_probe", property: "value"})
	err = validateIdenticalWorkers(h.deps.Workers, h.deps.MCP, h.deps.skills)
	if err == nil || !strings.Contains(err.Error(), "unexpected: worker_only_probe") {
		t.Fatalf("extra tool error = %v", err)
	}
	worker.UnregisterTool("worker_only_probe")

	// Same name, different schema: names alone would not catch this.
	h.deps.Workers[0].Dispatcher.RegisterTool(catalogProbeTool{name: "shared_probe", property: "value"})
	worker.RegisterTool(catalogProbeTool{name: "shared_probe", property: "renamed"})
	err = validateIdenticalWorkers(h.deps.Workers, h.deps.MCP, h.deps.skills)
	if err == nil || !strings.Contains(err.Error(), "definition differs: shared_probe") {
		t.Fatalf("changed schema error = %v", err)
	}
}

// InitGateway itself refuses a pool in which one worker differs from the
// primary, the way a worker would if a construction path skipped or changed
// one of its parts. Removing the startup comparison fails this test.
func TestInitGatewayRefusesToStartWhenOneWorkerDiffers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workers int
		alter   func(index int, worker AgentWorker)
		want    []string
	}{
		{"second worker lacks the cron tool", 2, func(index int, worker AgentWorker) {
			if index == 1 {
				worker.Dispatcher.UnregisterTool("cron")
			}
		}, []string{"agent worker 2", "missing: cron"}},
		{"third worker has an extra tool", 3, func(index int, worker AgentWorker) {
			if index == 2 {
				worker.Dispatcher.RegisterTool(catalogProbeTool{name: "worker_only_probe", property: "value"})
			}
		}, []string{"agent worker 3", "unexpected: worker_only_probe"}},
		{"second worker has a changed definition", 2, func(index int, worker AgentWorker) {
			property := "value"
			if index == 1 {
				property = "renamed"
			}
			worker.Dispatcher.RegisterTool(catalogProbeTool{name: "shared_probe", property: property})
		}, []string{"agent worker 2", "definition differs: shared_probe"}},
		{"second worker has a different context budget", 2, func(index int, worker AgentWorker) {
			if index == 1 {
				worker.Agent.SetContextWindow(12345)
			}
		}, []string{"worker 2 runtime context budget differs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, opts := parityGatewayOptions(t, tc.workers, "", answerEveryRequest)
			opts.workerBuilt = tc.alter
			deps, err := InitGateway(opts)
			if err == nil {
				deps.Close()
				t.Fatalf("InitGateway started %d workers although one differs from the primary", tc.workers)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("InitGateway error %q does not name %q", err, want)
				}
			}
			if deps != nil {
				t.Fatalf("InitGateway returned workers with its refusal: %+v", deps)
			}
		})
	}

	// The same construction with every worker left as built starts.
	_, opts := parityGatewayOptions(t, 3, "", answerEveryRequest)
	opts.workerBuilt = func(int, AgentWorker) {}
	deps, err := InitGateway(opts)
	if err != nil {
		t.Fatalf("InitGateway refused identical workers: %v", err)
	}
	deps.Close()
}

// Strictly sequential Runs alternate between workers, and each Run sees and
// calls both the MCP tool and the cron tool on its own worker. Admission runs
// at the default capacity two, so every MCP call claims the person-wide
// external target: each Run calls the MCP stub twice, and a successful
// synchronous return settles each claim, so neither the Run's second call nor
// the next Run's calls wait on a stranded claim.
func TestSequentialRunsOnDifferentWorkersCallMCPAndCron(t *testing.T) {
	for _, tc := range []struct {
		name    string
		server  string
		markers []string
	}{
		{"two workers", "stub", []string{"alpha", "beta"}},
		{"three workers, renamed server", "parity-probe", []string{"red", "green", "blue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echoTool := tools.MCPToolLocalName(tc.server, mcptest.EchoTool)
			h := newParityHarness(t, len(tc.markers), tc.server, parityScript(echoTool))
			calls := recordWorkerToolCalls(h.deps.Workers)
			server := &httpapi.Server{
				Control: h.store, Gateway: h.deps.Gateway, DefaultTenantID: control.DefaultTenantID,
				ApprovalWait: 30 * time.Second, ApprovalWaitUnattended: 30 * time.Second,
			}
			if err := server.ConfigureWorkRunCapacity(2, len(tc.markers)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			identity, err := h.store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			// MCP tools are unclassified external tools: a person approves
			// each call. This goroutine is that person.
			stopApprover := approvePending(ctx, t, h.store, identity)
			defer stopApprover()

			for _, marker := range tc.markers {
				resp, code := server.ProcessMessage(ctx, api.MessageRequest{
					Platform: "cli", PlatformUserID: "local", Channel: "parity-" + marker,
					Content: "Check this worker's tools. parity-marker:" + marker, ApprovalMode: "full-auto",
				})
				if code != http.StatusOK || !strings.Contains(resp.Content, "Parity checked "+marker) {
					t.Fatalf("run %s: code=%d content=%q error=%q outcome=%+v", marker, code, resp.Content, resp.Error, resp.Outcome)
				}
				for _, want := range []string{"echo " + marker, "echo again " + marker, "(job-" + marker + ") added"} {
					if !strings.Contains(resp.Content, want) {
						t.Fatalf("run %s did not get a successful tool result %q: %q", marker, want, resp.Content)
					}
				}
			}
			if stranded, err := h.store.ListUnresolvedExternalEffects(ctx, identity.TenantID, identity.PersonID, 20); err != nil || len(stranded) != 0 {
				t.Fatalf("successful MCP calls stranded external effect claims: %+v %v", stranded, err)
			}

			seenWorkers := map[int]string{}
			for _, marker := range tc.markers {
				echoWorker, ok := calls.workerFor(echoTool, marker)
				if !ok {
					t.Fatalf("run %s never executed %s; calls=%v", marker, echoTool, calls.snapshot())
				}
				cronWorker, ok := calls.workerFor("cron", marker)
				if !ok || cronWorker != echoWorker {
					t.Fatalf("run %s executed cron on worker %d (found %v), MCP on worker %d", marker, cronWorker, ok, echoWorker)
				}
				if other, reused := seenWorkers[echoWorker]; reused {
					t.Fatalf("runs %s and %s both landed on worker %d; sequential runs must reach every worker", other, marker, echoWorker)
				}
				seenWorkers[echoWorker] = marker
				offered := h.model.toolsOfferedFor(marker)
				if !offered[echoTool] || !offered["cron"] {
					t.Fatalf("run %s model request did not offer %s and cron: %v", marker, echoTool, offered)
				}
			}

			jobs, err := h.deps.CronScheduler.ListJobs(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range tc.markers {
				if !cronJobNamed(jobs, "job-"+marker) {
					t.Fatalf("cron job from run %s is not in the shared scheduler: %+v", marker, jobs)
				}
			}
			if starts := mcptest.Starts(t, h.startLog); len(starts) != 1 {
				t.Fatalf("MCP server processes = %d, want one shared process", len(starts))
			}
		})
	}
}

// Two workers call the same MCP server at the same moment through their full
// production dispatchers. The race detector run of this test is the safety
// evidence; the rendezvous proves the calls overlapped.
func TestTwoWorkersCallTheSharedMCPServerInParallel(t *testing.T) {
	h := newParityHarness(t, 2, "stub", answerEveryRequest)
	const person = "person-parallel-mcp"
	cleanup := tools.SetExecutionScope(person, tools.ExecutionScope{
		TenantID: control.DefaultTenantID, PersonID: person, RunID: "run-parallel-mcp",
		ApprovalMode: tools.ApprovalFullAuto,
		Approval: func(context.Context, tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
			return tools.ToolApprovalDecision{Approved: true, ApprovalID: "apr-parallel"}, nil
		},
	})
	defer cleanup()
	name := tools.MCPToolLocalName("stub", mcptest.RendezvousTool)
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for index, worker := range h.deps.Workers {
		wg.Add(1)
		go func(index int, dispatcher *tools.Dispatcher) {
			defer wg.Done()
			results[index], errs[index] = dispatcher.Dispatch(name, map[string]interface{}{
				"_tenant_id": person, "_tool_call_id": fmt.Sprintf("parallel-%d", index),
				"key": "workers", "parties": 2, "timeout_ms": 10000,
			})
		}(index, worker.Dispatcher)
	}
	wg.Wait()
	for index := range results {
		if errs[index] != nil || !strings.Contains(results[index], "2 parties met") {
			t.Fatalf("worker %d rendezvous = %q, err=%v", index+1, results[index], errs[index])
		}
	}
	if starts := mcptest.Starts(t, h.startLog); len(starts) != 1 {
		t.Fatalf("MCP server processes = %d, want one shared process", len(starts))
	}
}

// Every worker hands background reviews to the durable maintenance queue.
// None runs a review in-process, including a repeat of an already-queued
// review, which the queue deduplicates.
func TestEveryWorkerQueuesBackgroundReviewDurably(t *testing.T) {
	h := newParityHarness(t, 3, "stub", answerEveryRequest)
	ctx := context.Background()
	for index, worker := range h.deps.Workers {
		if worker.Agent.ReviewEngine == nil {
			t.Fatalf("worker %d has no background review engine", index+1)
		}
		messages := []llm.Message{
			{Role: "user", Content: fmt.Sprintf("Remember that worker %d prefers concise answers.", index+1)},
			{Role: "assistant", Content: "Noted."},
		}
		worker.Agent.ReviewEngine.SpawnReview("person-review", "cli", messages, true, false)
		// The same review again: already durable, so it is not run now.
		worker.Agent.ReviewEngine.SpawnReview("person-review", "cli", messages, true, false)
	}
	jobs, err := h.store.ListRunnableMaintenanceJobs(ctx, httpapi.SkillReviewJobVersion, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(h.deps.Workers) {
		t.Fatalf("durable review jobs = %d, want one per worker (%d): %+v", len(jobs), len(h.deps.Workers), jobs)
	}
	// An in-process review would call the review model immediately.
	time.Sleep(300 * time.Millisecond)
	if got := h.model.requestCount(); got != 0 {
		t.Fatalf("a worker ran %d background review model calls in-process", got)
	}
}

type catalogProbeTool struct {
	name     string
	property string
}

func (p catalogProbeTool) Name() string        { return p.name }
func (p catalogProbeTool) Description() string { return "catalogue comparison probe" }
func (p catalogProbeTool) Schema() tools.ToolSchema {
	closed := false
	return tools.ToolSchema{Type: "object", AdditionalProperties: &closed, Properties: map[string]tools.PropertyDef{
		p.property: {Type: "string", Description: "probe value"},
	}}
}
func (p catalogProbeTool) Execute(map[string]interface{}) (string, error) { return "probe", nil }

func cronJobNamed(jobs interface{}, name string) bool {
	data, _ := json.Marshal(jobs)
	return strings.Contains(string(data), `"`+name+`"`)
}

// workerToolCalls records which worker executed each tool call, keyed by the
// marker the scripted model puts into the call's arguments.
type workerToolCalls struct {
	mu    sync.Mutex
	calls []string
}

func recordWorkerToolCalls(workers []AgentWorker) *workerToolCalls {
	record := &workerToolCalls{}
	for index, worker := range workers {
		index := index
		worker.Dispatcher.InjectMiddleware(func(next tools.ToolExecutor) tools.ToolExecutor {
			return func(args map[string]interface{}) (string, error) {
				name, _ := args["_tool_name"].(string)
				arguments, _ := json.Marshal(publicArgs(args))
				record.mu.Lock()
				record.calls = append(record.calls, fmt.Sprintf("%d|%s|%s", index, name, arguments))
				record.mu.Unlock()
				return next(args)
			}
		})
	}
	return record
}

func (r *workerToolCalls) workerFor(tool, marker string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		parts := strings.SplitN(call, "|", 3)
		if len(parts) == 3 && parts[1] == tool && strings.Contains(parts[2], marker) {
			var worker int
			fmt.Sscan(parts[0], &worker)
			return worker, true
		}
	}
	return -1, false
}

func (r *workerToolCalls) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func publicArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		if !strings.HasPrefix(key, "_") {
			out[key] = value
		}
	}
	return out
}

func approvePending(ctx context.Context, t *testing.T, store *control.Store, identity *control.IdentityContext) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pending, err := store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20)
				if err != nil {
					continue
				}
				for _, approval := range pending {
					_, _ = store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approve", "cli", control.ApprovalDecisionInput{})
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// scriptedModelEndpoint is an OpenAI-compatible chat completions server.
// The production provider adapter talks to it over HTTP.
type scriptedModelEndpoint struct {
	server   *httptest.Server
	script   scriptedModelScript
	mu       sync.Mutex
	requests []scriptedModelRequest
}

type scriptedModelScript func(scriptedModelRequest) scriptedModelReply

type scriptedModelRequest struct {
	Tools    []string
	Messages []scriptedModelMessage
}

type scriptedModelMessage struct {
	Role       string
	Content    string
	ToolCallID string
}

type scriptedModelReply struct {
	Text      string
	ToolCalls []llm.ToolCall
}

func newScriptedModelEndpoint(t *testing.T, script scriptedModelScript) *scriptedModelEndpoint {
	t.Helper()
	endpoint := &scriptedModelEndpoint{script: script}
	endpoint.server = httptest.NewServer(http.HandlerFunc(endpoint.serve))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (e *scriptedModelEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request := scriptedModelRequest{}
	for _, tool := range body.Tools {
		request.Tools = append(request.Tools, tool.Function.Name)
	}
	for _, message := range body.Messages {
		request.Messages = append(request.Messages, scriptedModelMessage{
			Role: message.Role, Content: messageText(message.Content), ToolCallID: message.ToolCallID,
		})
	}
	e.mu.Lock()
	e.requests = append(e.requests, request)
	e.mu.Unlock()
	reply := e.script(request)

	if !body.Stream {
		message := map[string]interface{}{"content": reply.Text}
		if len(reply.ToolCalls) > 0 {
			message["tool_calls"] = openAIToolCalls(reply.ToolCalls, false)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"message": message, "finish_reason": scriptedFinishReason(reply)},
		}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk := func(delta map[string]interface{}, finish string) {
		choice := map[string]interface{}{"delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{choice}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if reply.Text != "" {
		writeChunk(map[string]interface{}{"content": reply.Text}, "")
	}
	if len(reply.ToolCalls) > 0 {
		writeChunk(map[string]interface{}{"tool_calls": openAIToolCalls(reply.ToolCalls, true)}, "")
	}
	writeChunk(map[string]interface{}{}, scriptedFinishReason(reply))
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (e *scriptedModelEndpoint) requestCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

// toolsOfferedFor returns the tools offered by the first model request of the
// Run whose input carried marker.
func (e *scriptedModelEndpoint) toolsOfferedFor(marker string) map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, request := range e.requests {
		if len(request.Tools) == 0 || runMarker(request.Messages) != marker {
			continue
		}
		offered := make(map[string]bool, len(request.Tools))
		for _, name := range request.Tools {
			offered[name] = true
		}
		return offered
	}
	return nil
}

func openAIToolCalls(calls []llm.ToolCall, streamed bool) []interface{} {
	out := make([]interface{}, 0, len(calls))
	for index, call := range calls {
		entry := map[string]interface{}{
			"id": call.ID, "type": "function",
			"function": map[string]interface{}{"name": call.Function, "arguments": call.Args},
		}
		if streamed {
			entry["index"] = index
		}
		out = append(out, entry)
	}
	return out
}

func scriptedFinishReason(reply scriptedModelReply) string {
	if len(reply.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func messageText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return string(raw)
}

var parityMarkerPattern = regexp.MustCompile(`parity-marker:([a-z0-9-]+)`)

func runMarker(messages []scriptedModelMessage) string {
	marker := ""
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		if found := parityMarkerPattern.FindAllStringSubmatch(message.Content, -1); len(found) > 0 {
			marker = found[len(found)-1][1]
		}
	}
	return marker
}

// answerEveryRequest is a model that never asks for a tool.
func answerEveryRequest(scriptedModelRequest) scriptedModelReply {
	return scriptedModelReply{Text: "ok"}
}

// parityScript calls the MCP echo tool twice and adds a cron job, all tagged
// with the Run's marker, then answers with the three tool results.
func parityScript(echoTool string) scriptedModelScript {
	return func(request scriptedModelRequest) scriptedModelReply {
		marker := runMarker(request.Messages)
		if len(request.Tools) == 0 || marker == "" {
			return scriptedModelReply{Text: "ok"}
		}
		results := map[string]string{}
		for _, message := range request.Messages {
			if message.Role == "tool" && strings.HasSuffix(message.ToolCallID, "-"+marker) {
				results[strings.TrimSuffix(message.ToolCallID, "-"+marker)] = message.Content
			}
		}
		if results["echo"] != "" && results["echo2"] != "" && results["cron"] != "" {
			return scriptedModelReply{Text: fmt.Sprintf("Parity checked %s: %s / %s / %s", marker, results["echo"], results["echo2"], results["cron"])}
		}
		return scriptedModelReply{ToolCalls: []llm.ToolCall{
			{ID: "echo-" + marker, Function: echoTool, Args: fmt.Sprintf(`{"text":"echo %s"}`, marker)},
			{ID: "echo2-" + marker, Function: echoTool, Args: fmt.Sprintf(`{"text":"echo again %s"}`, marker)},
			{ID: "cron-" + marker, Function: "cron", Args: fmt.Sprintf(
				`{"action":"add","name":"job-%s","cron":"0 9 * * *","prompt":"parity reminder %s"}`, marker, marker)},
		}}
	}
}
