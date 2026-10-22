package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/runpool"
	"selfmind/internal/tools"
	"selfmind/internal/tools/mcptest"
)

// loadCaseYAML writes content as a case file and loads it through the same
// parser and validation as the release corpus.
func loadCaseYAML(t *testing.T, id, content string) *Case {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	return c
}

func runLoadedCase(t *testing.T, cfgPath string, c *Case) (*RunResult, string) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "out.jsonl")
	result, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfgPath, OutputPath: output})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	return result, output
}

func failedChecks(result *RunResult) []string {
	var failed []string
	for _, check := range result.Checks {
		if !check.OK {
			failed = append(failed, check.Name+": "+check.Message)
		}
	}
	return failed
}

func checkByName(result *RunResult, name string) (CheckResult, bool) {
	for _, check := range result.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return CheckResult{}, false
}

// overlapCase is two CLI sessions, each holding a worker for its own Run,
// stopped from its own session. capacity and workers vary per test.
func overlapCase(id, capacity, workers, sessionA, sessionB string) string {
	return "id: " + id + "\n" +
		"title: \"two CLI sessions overlap\"\n" +
		"suite: reliability\n" +
		"workspace: isolated\n" +
		"model_required: false\n" +
		"runtime:\n  max_active_work_runs: " + capacity + "\n  workers: " + workers + "\n" +
		"turns:\n" +
		"  - input: \"Draft the release notes for the parser\"\n    platform: cli\n    channel: " + sessionA + "\n    cwd: " + sessionA + "\n    hold_worker: true\n" +
		"  - input: \"Draft the release notes for the renderer\"\n    platform: cli\n    channel: " + sessionB + "\n    cwd: " + sessionB + "\n    hold_worker: true\n" +
		"    assert_state:\n      - on: worker_overlap\n        count_gte: 2\n" +
		"  - input: \"/status\"\n    platform: cli\n    channel: " + sessionA + "\n" +
		"  - input: \"/stop\"\n    platform: cli\n    channel: " + sessionA + "\n    turn_run_ordinal: 1\n" +
		"  - input: \"/stop\"\n    platform: cli\n    channel: " + sessionB + "\n    turn_run_ordinal: 2\n" +
		"expect:\n  status: completed\n  max_tool_calls: 0\n  max_duration_seconds: 60\n  contains:\n    - \"2 runs active:\"\n    - \"Stopping run\"\n" +
		"assert_state:\n" +
		"  - on: worker_leases\n    count_gte: 2\n    count_lte: 2\n" +
		"  - on: leased_workers\n    count_gte: 2\n" +
		"  - on: worker_overlap\n    count_gte: 2\n" +
		"  - on: runs\n    status: cancelled\n    count_gte: 2\n" +
		"checks:\n  no_mojibake: true\n  no_empty_response: true\n  no_raw_json_leak: true\n"
}

// Two CLI sessions' Runs are admitted together at capacity two, each leases its
// own worker of the production pool, they overlap, and each session stops its
// own Run without a model call.
func TestRunCaseOverlapsTwoCLISessionsOnTwoWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	for _, tc := range []struct {
		name, capacity, workers, a, b string
	}{
		{"capacity two", "2", "2", "session-a", "session-b"},
		// Another capacity and other session names keep the same invariant.
		{"capacity three", "3", "3", "left-terminal", "right-terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgPath := writeRunnerFixtures(t)
			c := loadCaseYAML(t, "cli_overlap", overlapCase("cli_overlap", tc.capacity, tc.workers, tc.a, tc.b))
			result, output := runLoadedCase(t, cfgPath, c)
			if result.Status != "passed" {
				data, _ := os.ReadFile(output)
				t.Fatalf("overlap case failed: %v\n%s", failedChecks(result), data)
			}
			for _, name := range []string{"turn-1:hold_leased_worker", "turn-1:hold_stopped", "turn-2:hold_stopped", "turn-2:state:worker_overlap"} {
				if check, ok := checkByName(result, name); !ok || !check.OK {
					t.Fatalf("check %s = %+v (present %v)", name, check, ok)
				}
			}
			if result.InputTokens != 0 || result.OutputTokens != 0 {
				t.Fatalf("held Runs reached a model: %+v", result)
			}
		})
	}
}

// At capacity one the second session's work queues behind the first instead
// of taking the second worker, so the same case cannot overlap.
func TestRunCaseCapacityOneKeepsCLISessionsFromOverlapping(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "cli_overlap_capacity_one", overlapCase("cli_overlap_capacity_one", "1", "2", "session-a", "session-b"))
	result, _ := runLoadedCase(t, cfgPath, c)
	if result.Status == "passed" {
		t.Fatal("two sessions overlapped although capacity one admits a single Run")
	}
	if check, ok := checkByName(result, "turn-2:hold_leased_worker"); !ok || check.OK {
		t.Fatalf("the queued second session must not lease a worker: %+v present=%v", check, ok)
	}
	if check, ok := checkByName(result, "state:worker_overlap"); !ok || check.OK {
		t.Fatalf("overlap check = %+v present=%v, want a failure", check, ok)
	}
	if check, ok := checkByName(result, "turn-1:hold_stopped"); !ok || !check.OK {
		t.Fatalf("the first session's held Run was not stopped cleanly: %+v", check)
	}
	// The second session's stop names a turn that started no Run, so it is
	// reported instead of being sent as a bare /stop.
	if check, ok := checkByName(result, "turn-5:turn_run_ordinal"); !ok || check.OK {
		t.Fatalf("stop of a queued turn = %+v present=%v, want a failed check", check, ok)
	}
}

// unstoppedHoldCase holds one CLI Run and never stops it. Its case-level
// assertions describe how the harness must end that Run.
func unstoppedHoldCase(id string) string {
	return "id: " + id + "\ntitle: \"unstopped hold\"\nsuite: reliability\nworkspace: isolated\nmodel_required: false\n" +
		"turns:\n  - input: \"Draft the release notes\"\n    platform: cli\n    channel: session-a\n    hold_worker: true\n" +
		"  - input: \"/status\"\n    platform: cli\n    channel: session-a\n" +
		"expect:\n  max_duration_seconds: 60\n" +
		"assert_state:\n  - on: runs\n    status: cancelled\n    count_gte: 1\n  - on: queued\n    count_lte: 0\n"
}

// A held Run that the case never stops is released when the case ends: the
// case fails instead of keeping the worker, and the Run ends as a cancellation
// rather than as an interrupted model connection with an automatic recovery
// Run queued behind it.
func TestRunCaseFailsAHoldThatTheCaseNeverStops(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	grace := heldTurnGrace
	heldTurnGrace = 200 * time.Millisecond
	t.Cleanup(func() { heldTurnGrace = grace })
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "unstopped_hold", unstoppedHoldCase("unstopped_hold"))
	result, output := runLoadedCase(t, cfgPath, c)
	check, ok := checkByName(result, "turn-1:hold_stopped")
	if !ok || check.OK || !strings.Contains(check.Message, "released") {
		t.Fatalf("unstopped hold check = %+v present=%v, want a release failure", check, ok)
	}
	if result.Status == "passed" {
		t.Fatal("a case that never stopped its held Run passed")
	}
	for _, name := range []string{"state:runs:cancelled", "state:queued"} {
		if check, ok := checkByName(result, name); !ok || !check.OK {
			data, _ := os.ReadFile(output)
			t.Fatalf("released hold: %s = %+v present=%v\n%s", name, check, ok, data)
		}
	}
}

// The run watchdog cancels a held Run that stays quiet past the idle timeout.
// That is a stall, not a stop by the case, so the hold must fail.
func TestRunCaseHoldEndedByTheWatchdogIsNotAStop(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	t.Setenv("SELFMIND_RUN_IDLE_TIMEOUT", "1s")
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "stalled_hold", unstoppedHoldCase("stalled_hold"))
	result, output := runLoadedCase(t, cfgPath, c)
	check, ok := checkByName(result, "turn-1:hold_stopped")
	if !ok || check.OK || !strings.Contains(check.Message, "stall") {
		data, _ := os.ReadFile(output)
		t.Fatalf("stalled hold check = %+v present=%v, want a stall failure\n%s", check, ok, data)
	}
	if result.Status == "passed" {
		t.Fatal("a case whose held Run stalled passed")
	}
}

func TestHoldEndReasonCountsOnlyAStopAsStopped(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{context.Canceled, holdStopped},
		{fmt.Errorf("%w: user stop", context.Canceled), holdStopped},
		{context.DeadlineExceeded, holdDeadline},
		{runpool.ErrStalled, holdStalled},
		{errors.New("gateway shutdown"), "cancellation (gateway shutdown)"},
	} {
		if got := holdEndReason(tc.cause); got != tc.want {
			t.Fatalf("holdEndReason(%v) = %q, want %q", tc.cause, got, tc.want)
		}
	}
}

// A hold that ended on a stop still fails unless the Run is stored as
// cancelled.
func TestHeldRunNeedsAStoredCancellation(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "eval-tenant", "cli", "eval-held", "SelfMind Eval")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "held", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	h := &runtimeHarness{controlStore: store, tenantID: identity.TenantID}
	for _, tc := range []struct {
		status, want string
	}{
		{"cancelled", ""},
		{"interrupted", `stored status is "interrupted"`},
	} {
		run, err := store.StartRun(ctx, task, "cli", "held run")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, tc.status); err != nil {
			t.Fatal(err)
		}
		got := h.heldRunNotStopped(ctx, workerLease{runID: run.ID, endedBy: holdStopped})
		if (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Fatalf("stopped hold with a %s Run = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestRuntimeAndEndpointTurnValidation(t *testing.T) {
	base := "id: fields\nworkspace: isolated\n"
	turn := "turns:\n  - input: \"draft notes\"\n"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"capacity above three", "runtime:\n  max_active_work_runs: 4\n" + turn, "between 1 and 3"},
		{"workers below capacity", "runtime:\n  max_active_work_runs: 3\n  workers: 2\n" + turn, "below runtime.max_active_work_runs"},
		{"too many workers", "runtime:\n  workers: 17\n" + turn, "between 1 and 16"},
		{"invalid stub name", "runtime:\n  mcp_stubs:\n    - name: \"Bad Name\"\n" + turn, "lowercase letters"},
		{"repeated stub", "runtime:\n  mcp_stubs:\n    - name: stub\n    - name: stub\n" + turn, "repeats"},
		{"cwd outside the CLI", turn + "    cwd: parser\n", "platform: cli"},
		{"cwd escaping the workspace", turn + "    platform: cli\n    cwd: ../outside\n", "subdirectory"},
		{"invalid platform", turn + "    platform: \"We Chat\"\n", "not a platform name"},
		{"run of a later turn", turn + "    turn_run_ordinal: 1\n", "earlier turn"},
		{"held turn waiting for maintenance", turn + "    hold_worker: true\n    wait_for_maintenance: true\n", "cannot wait"},
		// A mistyped status, server or tool would count zero and pass a
		// negative assertion; a count without an operator asserts nothing.
		{"mistyped queue status", turn + "assert_state:\n  - on: queued\n    status: queud\n    count_lte: 0\n", `status "queud"`},
		{"mistyped run status", turn + "assert_state:\n  - on: runs\n    status: canceled\n    count_lte: 0\n", `status "canceled"`},
		{"mistyped choice status", turn + "assert_state:\n  - on: turn_choices\n    status: pendng\n    count_lte: 0\n", `status "pendng"`},
		{"mistyped stub server", "runtime:\n  mcp_stubs:\n    - name: stub\n" + turn + "assert_state:\n  - on: mcp_server_starts\n    target: stbu\n    count_lte: 0\n", "names no runtime.mcp_stubs server"},
		{"undeclared stub in a tool target", "runtime:\n  mcp_stubs:\n    - name: stub\n" + turn + "assert_state:\n  - on: worker_tool\n    target: mcp:other/echo\n    count_lte: 0\n", "names no runtime.mcp_stubs server"},
		{"mistyped stub tool", "runtime:\n  mcp_stubs:\n    - name: stub\n" + turn + "assert_state:\n  - on: worker_tool_calls\n    target: mcp:stub/ecoh\n    count_lte: 0\n", "names no tool of the MCP stub"},
		{"tool count without a target", turn + "assert_state:\n  - on: worker_tool\n    count_gte: 1\n", "needs a target"},
		{"count without an operator", turn + "assert_state:\n  - on: worker_leases\n", "needs count_gte, count_lte or exists"},
		{"turn count without an operator", turn + "    assert_state:\n      - on: queued\n", "turn 1: assert_state[0]"},
		{"status on a count without statuses", turn + "assert_state:\n  - on: worker_overlap\n    status: held\n    count_gte: 1\n", "takes no status"},
		{"target on a count without targets", turn + "assert_state:\n  - on: runs\n    target: cron\n    count_gte: 1\n", "takes no target"},
		{"string operator on a count", turn + "assert_state:\n  - on: runs\n    eq: \"1\"\n    count_gte: 1\n", "use only status, target"},
		// The tool name is hashed exactly as written, so a server spelled with
		// other case names another tool and would count zero.
		{"stub server in other case", "runtime:\n  mcp_stubs:\n    - name: stub\n" + turn + "assert_state:\n  - on: worker_tool\n    target: mcp:Stub/echo\n    count_lte: 0\n", "names no runtime.mcp_stubs server"},
		{"stub start log in other case", "runtime:\n  mcp_stubs:\n    - name: stub\n" + turn + "assert_state:\n  - on: mcp_server_starts\n    target: Stub\n    count_lte: 0\n", "names no runtime.mcp_stubs server"},
		// A lower bound of zero or less is met by every count.
		{"zero lower bound alone", turn + "assert_state:\n  - on: worker_leases\n    count_gte: 0\n", "cannot fail"},
		{"negative lower bound", turn + "assert_state:\n  - on: queued\n    count_gte: -1\n", "negative"},
		{"negative upper bound", turn + "assert_state:\n  - on: runs\n    count_lte: -1\n", "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "case.yaml")
			if err := os.WriteFile(path, []byte(base+tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCase error = %v, want %q", err, tc.want)
			}
		})
	}
	shared := "id: shared\nshared_data: true\n"
	for _, body := range []string{
		"runtime:\n  cron: true\n" + turn,
		"runtime:\n  mcp_stubs:\n    - name: stub\n" + turn,
		turn + "    platform: weixin\n",
		turn + "    hold_worker: true\n",
	} {
		path := filepath.Join(t.TempDir(), "case.yaml")
		if err := os.WriteFile(path, []byte(shared+body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "isolated data directory") {
			t.Fatalf("shared_data with %q: error = %v", body, err)
		}
	}
	c := loadCaseYAML(t, "defaults", base+"runtime:\n  max_active_work_runs: 2\n"+turn)
	if c.Runtime.capacity() != 2 || c.Runtime.workers() != 2 {
		t.Fatalf("runtime defaults: capacity=%d workers=%d, want the worker count to follow the capacity", c.Runtime.capacity(), c.Runtime.workers())
	}
	var none *RuntimeSettings
	if none.capacity() != 1 || none.workers() != 1 {
		t.Fatal("a case without runtime must keep one Run on one worker")
	}
}

func TestPeakLeaseOverlap(t *testing.T) {
	at := func(seconds int) time.Time { return time.Unix(int64(1000+seconds), 0) }
	for _, tc := range []struct {
		name   string
		leases []workerLease
		want   int
	}{
		{"none", nil, 0},
		{"sequential on one worker", []workerLease{{worker: 0, start: at(0), end: at(1)}, {worker: 0, start: at(1), end: at(2)}}, 1},
		{"overlapping on two workers", []workerLease{{worker: 0, start: at(0), end: at(5)}, {worker: 1, start: at(1), end: at(3)}}, 2},
		{"still open", []workerLease{{worker: 0, start: at(0)}, {worker: 1, start: at(2)}, {worker: 0, start: at(3), end: at(4)}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peakLeaseOverlap(tc.leases); got != tc.want {
				t.Fatalf("peak overlap = %d, want %d", got, tc.want)
			}
		})
	}
	if got := leasedWorkers([]workerLease{{worker: 0}, {worker: 1}, {worker: 0}}); got != 2 {
		t.Fatalf("leased workers = %d, want 2", got)
	}
}

// A person count without a resolved person fails instead of reading zero,
// which would satisfy a count_lte assertion vacuously.
func TestPersonCountsWithoutAPersonFail(t *testing.T) {
	h := &runtimeHarness{leases: newWorkerLeaseLog(), choices: map[string]string{}}
	zero := 0
	for _, on := range []string{"runs", "queued", "turn_choices"} {
		preds := []StatePredicate{{On: on, CountLte: &zero}}
		world := WorldState{runtime: h.collectRuntimeFacts(context.Background(), nil, preds)}
		if check := EvaluateStatePredicates(preds, world)[0]; check.OK || !strings.Contains(check.Message, "no person") {
			t.Fatalf("%s without a person = %+v, want a failure", on, check)
		}
	}
}

func TestResolveToolNameAddressesMCPToolsByServerAndTool(t *testing.T) {
	if got := resolveToolName("cron"); got != "cron" {
		t.Fatalf("built-in tool = %q", got)
	}
	if got, want := resolveToolName("mcp:stub/echo"), tools.MCPToolLocalName("stub", "echo"); got != want {
		t.Fatalf("MCP tool = %q, want %q", got, want)
	}
}

// catalogCase builds every worker with the cron tool and an MCP stub and
// asserts the workers offer identical catalogues through one stub process.
func catalogCase(id, workers, server string, cron bool) string {
	cronLine := ""
	if cron {
		cronLine = "  cron: true\n"
	}
	return "id: " + id + "\n" +
		"title: \"worker catalogue parity\"\n" +
		"suite: reliability\n" +
		"workspace: isolated\n" +
		"model_required: false\n" +
		"requires:\n  commands: [go]\n" +
		"runtime:\n  max_active_work_runs: 2\n  workers: " + workers + "\n" + cronLine +
		"  mcp_stubs:\n    - name: " + server + "\n" +
		"turns:\n  - input: \"/status\"\n" +
		"expect:\n  status: completed\n  max_duration_seconds: 120\n" +
		"assert_state:\n" +
		"  - on: workers\n    count_gte: " + workers + "\n    count_lte: " + workers + "\n" +
		"  - on: worker_tool\n    target: cron\n    count_gte: " + workers + "\n" +
		"  - on: worker_tool\n    target: mcp:" + server + "/echo\n    count_gte: " + workers + "\n" +
		"  - on: worker_tool\n    target: mcp:" + server + "/rendezvous\n    count_gte: " + workers + "\n" +
		"  - on: worker_catalogs\n    count_lte: 1\n" +
		"  - on: mcp_server_starts\n    target: " + server + "\n    count_gte: 1\n    count_lte: 1\n" +
		"checks:\n  no_mojibake: true\n  no_empty_response: true\n"
}

// Every worker of the production pool offers the same catalogue, including the
// cron tool and the MCP stub's tools, over one shared stub process.
func TestRunCaseWorkersOfferIdenticalCatalogsWithMCPStubAndCron(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness and builds the MCP stub")
	}
	for _, tc := range []struct{ workers, server string }{
		{"2", "stub"},
		// Another pool size and server name keep the same invariant.
		{"3", "parity-probe"},
	} {
		t.Run(tc.workers+" workers", func(t *testing.T) {
			_, cfgPath := writeRunnerFixtures(t)
			c := loadCaseYAML(t, "worker_catalog_parity", catalogCase("worker_catalog_parity", tc.workers, tc.server, true))
			result, output := runLoadedCase(t, cfgPath, c)
			if result.Status != "passed" {
				data, _ := os.ReadFile(output)
				t.Fatalf("catalogue parity case failed: %v\n%s", failedChecks(result), data)
			}
		})
	}
}

// Without runtime.cron the workers do not offer the cron tool, so the same
// assertion must fail: the predicate reads the built catalogues.
func TestRunCaseWithoutCronOffersNoCronTool(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness and builds the MCP stub")
	}
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "worker_catalog_without_cron", catalogCase("worker_catalog_without_cron", "2", "stub", false))
	result, _ := runLoadedCase(t, cfgPath, c)
	check, ok := checkByName(result, "state:worker_tool:cron")
	if !ok || check.OK || !strings.Contains(check.Message, "got 0") {
		t.Fatalf("cron check without runtime.cron = %+v present=%v, want zero workers", check, ok)
	}
	if check, ok := checkByName(result, "state:worker_tool:mcp:stub/echo"); !ok || !check.OK {
		t.Fatalf("MCP stub tool check = %+v present=%v", check, ok)
	}
}

// writeToolCallCassettes records, for each of turns sequential turns, a turn
// that calls tool once with args and then answers.
func writeToolCallCassettes(t *testing.T, session, tool, args string, turns int) {
	t.Helper()
	dir := filepath.Join(os.Getenv("SELFMIND_EVAL_VCR_DIR"), session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var records []map[string]interface{}
	for turn := 0; turn < turns; turn++ {
		records = append(records,
			map[string]interface{}{"method": "completion", "completion": "check scheduled jobs"},
			map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{
				"tool_calls": []map[string]interface{}{{"ID": "call_" + tool + "_" + string(rune('a'+turn)), "Function": tool, "Args": args}},
			}}},
			map[string]interface{}{"method": "stream", "events": []map[string]interface{}{{"content": "Checked."}}},
		)
	}
	for i, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.json", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Consecutive Runs take the pool's workers in turn, so a tool called once per
// Run is called on every worker; the predicate attributes each completed call
// to the worker its Run leased.
func TestRunCaseAttributesToolCallsToTheWorkersThatRanThem(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	const id = "worker_tool_calls"
	writeToolCallCassettes(t, id, "cron", `{"action":"list"}`, 2)
	content := "id: " + id + "\nsuite: reliability\nworkspace: isolated\n" +
		"runtime:\n  max_active_work_runs: 2\n  workers: 2\n  cron: true\n" +
		"turns:\n  - input: \"List my scheduled jobs.\"\n  - input: \"/new --run List my scheduled jobs again.\"\n" +
		"expect:\n  max_duration_seconds: 60\n" +
		"assert_state:\n" +
		"  - on: worker_leases\n    count_gte: 2\n" +
		"  - on: leased_workers\n    count_gte: 2\n" +
		"  - on: worker_overlap\n    count_lte: 1\n" +
		"  - on: worker_tool_calls\n    target: cron\n    count_gte: 2\n" +
		"checks:\n  no_empty_response: true\n"
	c := loadCaseYAML(t, id, content)
	result, output := runLoadedCase(t, cfgPath, c)
	if result.Status != "passed" {
		data, _ := os.ReadFile(output)
		t.Fatalf("tool call attribution case failed: %v\n%s", failedChecks(result), data)
	}
}

// Offline, the stub build may use only the module cache already on disk.
func TestMCPStubBuildEnvDisablesDownloadsOffline(t *testing.T) {
	base := []string{"PATH=/usr/bin", "GOPROXY=https://proxy.golang.org"}
	offline := mcpStubBuildEnv(base, "/home/real", true)
	if offline[len(offline)-1] != "GOPROXY=off" || !containsString(offline, "HOME=/home/real") {
		t.Fatalf("offline build env = %v, want the original HOME and GOPROXY=off last", offline)
	}
	if online := mcpStubBuildEnv(base, "/home/real", false); containsString(online, "GOPROXY=off") {
		t.Fatalf("online build env disabled module downloads: %v", online)
	}
}

// A stub that cannot be built offline, here because the module cache is
// empty and downloads are off, is an unavailable requirement with its reason,
// not a failed case.
func TestRunCaseReportsAnUnbuildableMCPStubAsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness and runs the Go tool")
	}
	_, cfgPath := writeRunnerFixtures(t)
	t.Setenv("GOMODCACHE", t.TempDir())
	c := loadCaseYAML(t, "unbuildable_stub", catalogCase("unbuildable_stub", "2", "stub", true))
	output := filepath.Join(t.TempDir(), "out.jsonl")
	started := time.Now()
	_, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfgPath, OutputPath: output})
	if !errors.Is(err, ErrRequirementUnavailable) || !strings.Contains(err.Error(), "build the MCP stub") {
		t.Fatalf("RunCase error = %v, want an unavailable MCP stub build", err)
	}
	if elapsed := time.Since(started); elapsed > time.Minute {
		t.Fatalf("an offline build without modules took %s; downloads may not be off", elapsed)
	}
}

// A held Run joins the case's created work, but case-level run assertions
// still describe the last foreground Run: here a model turn that completes on
// the second worker while the first session holds the other.
func TestRunCaseRunAssertionsDescribeTheLastForegroundRun(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	const id = "foreground_run_subject"
	// Each new-work turn makes one auxiliary completion before its Run starts;
	// only the second turn's agent then answers.
	dir := filepath.Join(os.Getenv("SELFMIND_EVAL_VCR_DIR"), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, record := range []string{
		`{"method":"completion","completion":"parser release notes"}`,
		`{"method":"completion","completion":"renderer summary"}`,
		`{"method":"stream","events":[{"content":"READY"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.json", i)), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	content := "id: " + id + "\nsuite: reliability\nworkspace: isolated\n" +
		"runtime:\n  max_active_work_runs: 2\n  workers: 2\n" +
		"turns:\n" +
		"  - input: \"Draft the release notes for the parser\"\n    platform: cli\n    channel: session-a\n    cwd: parser\n    hold_worker: true\n" +
		"  - input: \"Summarize the renderer changes\"\n    platform: cli\n    channel: session-b\n    cwd: renderer\n" +
		"  - input: \"/stop\"\n    platform: cli\n    channel: session-a\n    turn_run_ordinal: 1\n" +
		"expect:\n  max_duration_seconds: 60\n  contains:\n    - \"READY\"\n" +
		"assert_state:\n" +
		"  - on: run\n    field: input_summary\n    contains: \"renderer\"\n" +
		"  - on: worker_overlap\n    count_gte: 2\n" +
		"  - on: runs\n    status: cancelled\n    count_gte: 1\n" +
		"checks:\n  no_empty_response: true\n"
	c := loadCaseYAML(t, id, content)
	result, output := runLoadedCase(t, cfgPath, c)
	if result.Status != "passed" {
		data, _ := os.ReadFile(output)
		t.Fatalf("foreground subject case failed: %v\n%s", failedChecks(result), data)
	}
}

// A tool count must name a tool the daemon can offer: a mistyped built-in name
// would count zero and pass a negative assertion. The cron tool stays a valid
// target when the case does not enable it, so its absence can be asserted.
func TestRuntimeToolCountsRejectUnknownTools(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	content := "id: unknown_tool_targets\nsuite: reliability\nworkspace: isolated\nmodel_required: false\n" +
		"runtime:\n  max_active_work_runs: 2\n  workers: 2\n" +
		"turns:\n  - input: \"/status\"\n" +
		"assert_state:\n" +
		"  - on: worker_tool\n    target: corn\n    count_lte: 0\n" +
		"  - on: worker_tool_calls\n    target: shel\n    count_lte: 0\n" +
		"  - on: worker_tool\n    target: cron\n    count_lte: 0\n" +
		"  - on: worker_tool\n    target: read_file\n    count_gte: 2\n"
	c := loadCaseYAML(t, "unknown_tool_targets", content)
	result, _ := runLoadedCase(t, cfgPath, c)
	for _, name := range []string{"state:worker_tool:corn", "state:worker_tool_calls:shel"} {
		if check, ok := checkByName(result, name); !ok || check.OK || !strings.Contains(check.Message, "no agent worker registers") {
			t.Fatalf("%s = %+v present=%v, want an unknown-tool failure", name, check, ok)
		}
	}
	for _, name := range []string{"state:worker_tool:cron", "state:worker_tool:read_file"} {
		if check, ok := checkByName(result, name); !ok || !check.OK {
			t.Fatalf("%s = %+v present=%v, want it to pass", name, check, ok)
		}
	}
}

// Only an environment that cannot supply the stub makes a case unavailable.
// A stub that does not compile is a regression in the source and fails.
func TestRunCaseReportsAStubCompileErrorAsAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness and runs the Go tool")
	}
	_, cfgPath := writeRunnerFixtures(t)
	root, err := selfmindModuleRoot("")
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "mcptest.go")
	if err := os.WriteFile(broken, []byte("package mcptest\n\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {filepath.Join(root, "internal", "tools", "mcptest", "mcptest.go"): broken},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "-overlay="+overlayPath)
	c := loadCaseYAML(t, "broken_stub", catalogCase("broken_stub", "2", "stub", true))
	_, err = RunCase(context.Background(), c, RunOptions{ConfigPath: cfgPath, OutputPath: filepath.Join(t.TempDir(), "out.jsonl")})
	if err == nil || errors.Is(err, ErrRequirementUnavailable) || !strings.Contains(err.Error(), "build the MCP stub") {
		t.Fatalf("RunCase error = %v, want a stub build failure that is not an unavailable requirement", err)
	}
}

// The harness launches the stub with the command contract of mcptest; this
// keeps the duplicated package path and flag in step with it.
func TestMCPStubContractMatchesMCPTest(t *testing.T) {
	if mcpStubPackage != mcptest.StubPackage {
		t.Fatalf("stub package %q, mcptest builds %q", mcpStubPackage, mcptest.StubPackage)
	}
	if got, want := []string{mcpStubStartLogFlag, "log"}, mcptest.StartLogArgs("log"); !reflect.DeepEqual(got, want) {
		t.Fatalf("start-log arguments %v, mcptest uses %v", got, want)
	}
	if want := []string{mcptest.EchoTool, mcptest.RendezvousTool, mcptest.RecordTool}; !reflect.DeepEqual(mcpStubTools, want) {
		t.Fatalf("stub tools %v, mcptest serves %v", mcpStubTools, want)
	}
}

// Like the daemon, the harness hands durable background reviews to the primary
// worker's engine, so a review a worker queued during a case is executed by the
// case's maintenance pass instead of waiting for an executor that never exists.
func TestNewRuntimeHarnessRunsQueuedReviewsLikeTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "review_executor", "id: review_executor\nsuite: reliability\nworkspace: isolated\nmodel_required: false\n"+
		"runtime:\n  max_active_work_runs: 2\nturns:\n  - input: \"/status\"\n")
	root, err := makeEvalTempRoot(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupEvalTempRoot(root) })
	harness, err := newRuntimeHarness(RunOptions{ConfigPath: cfgPath}, c, filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("newRuntimeHarness: %v", err)
	}
	defer harness.Close()
	primary := harness.gateway.Workers[0].Agent
	if primary.ReviewEngine == nil || harness.server.SkillReviewer != primary.ReviewEngine {
		t.Fatal("the harness did not wire the primary worker's review engine as the durable review executor")
	}
	ctx := context.Background()
	// Each worker queues one review, as its turn nudge would.
	for index, worker := range harness.gateway.Workers {
		worker.Agent.ReviewEngine.SpawnReview("person-review", "cli", []llm.Message{
			{Role: "user", Content: "worker review request " + string(rune('a'+index))},
			{Role: "assistant", Content: "done"},
		}, true, false)
	}
	queued, err := harness.controlStore.ListRunnableMaintenanceJobs(ctx, httpapi.SkillReviewJobVersion, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != len(harness.gateway.Workers) {
		t.Fatalf("queued reviews = %d, want one per worker (%d)", len(queued), len(harness.gateway.Workers))
	}
	harness.server.RunMaintenancePass(ctx)
	left, err := harness.controlStore.ListRunnableMaintenanceJobs(ctx, httpapi.SkillReviewJobVersion, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("maintenance pass left %d review job(s) without running them", len(left))
	}
}

func TestRunCaseFinishedTurnsReleaseCapacity(t *testing.T) {
	_, cfgPath := writeRunnerFixtures(t)
	c, err := LoadCase("../../evalcases/reliability/finished-turn-releases-capacity.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := runLoadedCase(t, cfgPath, c)
	if result.Status != "passed" {
		t.Fatalf("finished turns blocked admission or changed state: %v", failedChecks(result))
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatal("deterministic admission check called a model")
	}
}
