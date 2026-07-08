// Package client is the daemon-backed transport for CLI/TUI clients. It turns a
// remote `selfmind gateway run` daemon into the same MessageProcessor contract
// the in-process path uses, so the rich TUI can run as a thin client to one
// shared daemon (the codex/hermes model) instead of building its own in-process
// gateway and racing other terminals on control.db, auth refresh, and worker
// state. See docs/worker-pool-design.md and AGENTS.md ("selfmind gateway is the
// product entrypoint for multi-terminal work").
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
)

// Client talks to a gateway daemon over HTTP.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Presence honesty (docs/identity-continuity.md "Runtime attachment
	// model"): presence must mean "the person is at this terminal", not "a
	// terminal process is alive". IdleTimeout + LastInput let the client
	// compute input age and stamp active=0|1 on its presence-claiming
	// requests (the idle ping and the event polls); the daemon only touches
	// presence for active=1. IdleTimeout <= 0 or a nil LastInput disables the
	// idle check (every beat claims attachment — the old behavior).
	IdleTimeout time.Duration
	LastInput   func() time.Time
}

// InputTracker is a tiny concurrency-safe "last user input" timestamp shared
// between the TUI (keystrokes Touch it) and the Client (heartbeats read it).
// It is seeded with the construction time: launching the TUI is itself input.
type InputTracker struct {
	lastNanos atomic.Int64
}

// NewInputTracker returns a tracker seeded to now.
func NewInputTracker() *InputTracker {
	t := &InputTracker{}
	t.Touch()
	return t
}

// Touch records user input at time now.
func (t *InputTracker) Touch() {
	if t != nil {
		t.lastNanos.Store(time.Now().UnixNano())
	}
}

// Last returns the most recent input time (zero when never touched).
func (t *InputTracker) Last() time.Time {
	if t == nil {
		return time.Time{}
	}
	nanos := t.lastNanos.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// presenceActive reports whether this endpoint may still claim attachment:
// the last user input is younger than IdleTimeout. Missing wiring or a zero
// timeout fails open to active — never let a config gap silently mute the
// terminal's presence.
func (c *Client) presenceActive() bool {
	if c == nil || c.IdleTimeout <= 0 || c.LastInput == nil {
		return true
	}
	last := c.LastInput()
	if last.IsZero() {
		return true
	}
	return time.Since(last) <= c.IdleTimeout
}

// presenceActiveParam is the wire form of presenceActive for the active=0|1
// query parameter (absent or "1" = claim presence, "0" = watching only).
func (c *Client) presenceActiveParam() string {
	if c.presenceActive() {
		return "1"
	}
	return "0"
}

// New builds a Client. A nil http.Client falls back to a sensible default with
// no overall timeout (turns can be long); per-request timeouts are applied via
// context where needed.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{},
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) auth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// ProcessMessage implements the cli.MessageProcessor signature against a remote
// daemon. The synchronous POST is the source of truth for the final answer;
// concurrently, a best-effort poller replays the run's task events into the
// ctx stream observer so the TUI shows live tool/thinking progress. Streaming is
// strictly best-effort — the returned answer never depends on it — which keeps
// the client correct even if event polling lags or the task can't be resolved.
func (c *Client) ProcessMessage(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
	observer := httpapi.StreamObserverFromContext(ctx)

	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	// Slash/control commands ("/status", "/tasks", ...) return inline content
	// and create no run events, so don't bother polling for them.
	if observer != nil && !isControlCommand(req.Content) {
		go c.pollEvents(streamCtx, req, observer)
	}

	resp, status, err := c.postMessage(ctx, req)
	stopStream()
	if err != nil {
		return api.MessageResponse{Error: err.Error()}, http.StatusBadGateway
	}
	return resp, status
}

func (c *Client) postMessage(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/message", bytes.NewReader(body))
	if err != nil {
		return api.MessageResponse{}, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return api.MessageResponse{}, 0, err
	}
	defer httpResp.Body.Close()
	data, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode >= http.StatusBadRequest {
		var resp api.MessageResponse
		if json.Unmarshal(data, &resp) == nil && (resp.Error != "" || resp.Content != "") {
			return resp, httpResp.StatusCode, nil
		}
		return api.MessageResponse{}, httpResp.StatusCode, fmt.Errorf("gateway returned %s: %s", httpResp.Status, strings.TrimSpace(string(data)))
	}
	var resp api.MessageResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return api.MessageResponse{}, httpResp.StatusCode, err
	}
	return resp, httpResp.StatusCode, nil
}

// Dispatch runs a management tool on the daemon and returns its text result. It
// backs agent-backed slash commands (/skills, /memory subcommands, /bundles,
// /checkpoint) in client mode. The daemon enforces a tool safelist and the
// tenant scope, so this is not a general tool-execution path.
func (c *Client) Dispatch(tool string, args map[string]interface{}) (string, error) {
	req := api.DispatchRequest{
		Platform:       "cli",
		PlatformUserID: clientUserID(),
		Tool:           tool,
		Args:           args,
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/dispatch", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()
	data, _ := io.ReadAll(httpResp.Body)
	var resp api.DispatchResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("gateway returned %s", strings.TrimSpace(string(data)))
	}
	if resp.Error != "" {
		return "", fmt.Errorf("%s", resp.Error)
	}
	return resp.Result, nil
}

func clientUserID() string {
	for _, key := range []string{"SELFMIND_CLI_USER_ID", "SELF_CLI_USER_ID", "USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "local"
}

// eventsResponse mirrors the GET /v1/tasks/events payload.
type eventsResponse struct {
	Events []control.Event `json:"events"`
}

// pollEvents re-fetches the person's current-task events on a short cadence and
// forwards any newly-seen event (oldest-first) to the observer. It re-resolves
// "current task" each tick, so it correctly latches onto the run's task once the
// daemon creates it mid-request, without needing the task id up front.
func (c *Client) pollEvents(ctx context.Context, req api.MessageRequest, observer httpapi.StreamObserver) {
	seen := map[string]bool{}
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	// Baseline probe: mark every pre-existing event of the current task as
	// seen WITHOUT forwarding it. The person's current task can be an older
	// parked task, and replaying its history renders yesterday's
	// approval.requested as a live y/N prompt (observed live: a ghost chmod
	// approval from a previous session). Only events recorded after this
	// turn started may reach the observer; this turn's own events appear on
	// later ticks.
	c.drainEventsOnce(ctx, req, seen, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.drainEventsOnce(ctx, req, seen, observer)
		}
	}
}

// drainEventsOnce fetches the current task's events, forwards the renderable
// newly-seen ones to the observer, and returns every newly-seen raw event
// oldest-first (WatchActiveRun inspects raw types like run.finished that the
// live UI never renders). A nil observer marks events seen without forwarding
// (baseline probe).
func (c *Client) drainEventsOnce(ctx context.Context, req api.MessageRequest, seen map[string]bool, observer httpapi.StreamObserver) []control.Event {
	q := url.Values{}
	if req.TenantID != "" {
		q.Set("tenant_id", req.TenantID)
	}
	q.Set("platform", fallback(req.Platform, "cli"))
	q.Set("platform_user_id", fallback(req.PlatformUserID, "local"))
	if req.DisplayName != "" {
		q.Set("display_name", req.DisplayName)
	}
	// Event polls double as presence beats on the daemon; claim attachment
	// only while the person is actually typing here (input younger than the
	// idle timeout), so watching a long run from a vacated desk does not keep
	// muting IM pushes.
	q.Set("active", c.presenceActiveParam())
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/v1/tasks/events?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return nil
	}
	var payload eventsResponse
	if json.NewDecoder(httpResp.Body).Decode(&payload) != nil {
		return nil
	}
	var fresh []control.Event
	// ListTaskEvents returns newest-first; replay oldest-first for natural order.
	for i := len(payload.Events) - 1; i >= 0; i-- {
		ev := payload.Events[i]
		if ev.ID == "" || seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		fresh = append(fresh, ev)
		if observer == nil {
			continue
		}
		if se, ok := eventToStream(ev); ok {
			observer(se)
		}
	}
	return fresh
}

// eventToStream maps a persisted control.Event back into the llm.StreamEvent the
// TUI's forwardGatewayEvent understands. It returns ok=false for event types the
// live UI does not render (outcome/turn bookkeeping), so they are skipped.
// plan.updated is forwarded (payload carries the full plan) so the client TUI
// renders the live checklist. The payload field names mirror
// httpapi.recordStreamEvent.
func eventToStream(ev control.Event) (llm.StreamEvent, bool) {
	p := decodePayload(ev.Payload)
	switch {
	case ev.Type == "tool.started":
		return llm.StreamEvent{
			EventType:  "tool.started",
			ToolName:   str(p["tool"]),
			ToolCallID: str(p["tool_call_id"]),
			ToolArgs:   str(p["args"]),
		}, true
	case ev.Type == "tool.completed":
		se := llm.StreamEvent{
			EventType:       "tool.completed",
			ToolName:        str(p["tool"]),
			ToolCallID:      str(p["tool_call_id"]),
			ToolResult:      str(p["result"]),
			DurationSeconds: num(p["duration_seconds"]),
		}
		if errText := str(p["error"]); errText != "" {
			se.Err = fmt.Errorf("%s", errText)
		}
		return se, true
	case ev.Type == "tool.output":
		return llm.StreamEvent{EventType: "tool.output", ToolName: str(p["tool"]), Content: str(p["message"])}, true
	case ev.Type == "agent.thinking" || ev.Type == "agent.step":
		return llm.StreamEvent{EventType: ev.Type, Content: str(p["message"])}, true
	case ev.Type == "approval.requested":
		// Surface the pending approval so the client TUI can prompt; approval_id
		// and the compact action target ride in Payload, tool in ToolName,
		// reason in Content.
		return llm.StreamEvent{
			EventType: "approval.requested",
			ToolName:  str(p["tool"]),
			Content:   str(p["reason"]),
			Payload: map[string]interface{}{
				"approval_id": str(p["approval_id"]),
				"target":      str(p["target"]),
			},
		}, true
	case ev.Type == "clarify.requested":
		return llm.StreamEvent{
			EventType: "clarify.requested",
			Content:   str(p["question"]),
			Payload: map[string]interface{}{
				"clarify_id": str(p["clarify_id"]),
				"choices":    p["choices"],
			},
		}, true
	case strings.HasPrefix(ev.Type, "learning."):
		return llm.StreamEvent{EventType: "learning.review", Content: str(p["message"])}, true
	case ev.Type == "token.updated":
		// Live cumulative usage snapshot for the run (kernel emits it after
		// every model response with run totals). Forward it as a typed Usage
		// so the client TUI ticks its run token counter mid-run — without this
		// the status bar sat at 0 until the final sync response (observed live).
		return llm.StreamEvent{
			EventType: "token.updated",
			Usage: &llm.UsageStats{
				InputTokens:  int(num(p["input_tokens"])),
				OutputTokens: int(num(p["output_tokens"])),
			},
		}, true
	case ev.Type == "plan.updated":
		// The daemon records the full structured plan (plan steps + explanation)
		// in the event payload. Forward it so the client TUI can render the live
		// Codex-style checklist via renderPlanCell — without this the plan step
		// list never reaches client-mode TUIs (the default path) and the user
		// sees only a stray "plan updated" line instead of [x]/[>]/[ ] progress.
		return llm.StreamEvent{EventType: "plan.updated", Payload: p}, true
	default:
		return llm.StreamEvent{}, false
	}
}

// Digest fetches the attach digest (GET /v1/digest): what finished, failed,
// or is still waiting since this CLI account's last presence. Callers must
// fetch it BEFORE the first presence beat — the beat stamps the very
// last_seen_at anchor the digest is computed from.
func (c *Client) Digest(ctx context.Context) (*api.DigestResponse, error) {
	q := url.Values{}
	q.Set("platform", "cli")
	q.Set("platform_user_id", clientUserID())
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/v1/digest?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("digest returned %s: %s", httpResp.Status, strings.TrimSpace(string(data)))
	}
	var digest api.DigestResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&digest); err != nil {
		return nil, err
	}
	return &digest, nil
}

// WatchActiveRun re-attaches this client to the person's mid-flight run as a
// pure watcher (observation layer, docs/identity-continuity.md "Runtime
// attachment model"): live events stream into the observer without starting a
// user turn, and the loop ends when the run does. The baseline probe marks
// every pre-attach event seen so history is never replayed (the same ghost-
// approval hazard pollEvents guards against). Run end is detected two ways: a
// fresh run.finished / run.cancelled event on the task (primary; carries the
// outcome summary), and a /v1/tasks/current probe reporting no active run —
// run every ~2s, immediately after a turn.completed event, and covering runs
// that finalize without a terminal event (failure paths) or on another task.
// Returns the finished run's outcome summary when one is available.
func (c *Client) WatchActiveRun(ctx context.Context, observer httpapi.StreamObserver) string {
	req := api.MessageRequest{Platform: "cli", PlatformUserID: clientUserID()}
	seen := map[string]bool{}
	c.drainEventsOnce(ctx, req, seen, nil)
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-ticker.C:
			probeNow := false
			for _, ev := range c.drainEventsOnce(ctx, req, seen, observer) {
				switch ev.Type {
				case "run.finished", "run.cancelled":
					return runOutcomeSummary(ev)
				case "turn.completed":
					probeNow = true
				}
			}
			ticks++
			if probeNow || ticks%6 == 0 {
				if gone, ok := c.activeRunGone(ctx, req); ok && gone {
					return ""
				}
			}
		}
	}
}

// activeRunGone probes GET /v1/tasks/current and reports whether the person
// has no active run anymore. ok=false means the probe itself failed (keep
// watching rather than mistaking a network blip for run completion).
func (c *Client) activeRunGone(ctx context.Context, req api.MessageRequest) (gone, ok bool) {
	q := url.Values{}
	if req.TenantID != "" {
		q.Set("tenant_id", req.TenantID)
	}
	q.Set("platform", fallback(req.Platform, "cli"))
	q.Set("platform_user_id", fallback(req.PlatformUserID, "local"))
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/v1/tasks/current?"+q.Encode(), nil)
	if err != nil {
		return false, false
	}
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return false, false
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return false, false
	}
	var payload struct {
		ActiveRun *api.ActiveRunStatus `json:"active_run"`
	}
	if json.NewDecoder(httpResp.Body).Decode(&payload) != nil {
		return false, false
	}
	return payload.ActiveRun == nil, true
}

// runOutcomeSummary extracts the outcome summary a run.finished event carries
// (payload {"outcome": api.RunOutcome, ...}); empty when absent.
func runOutcomeSummary(ev control.Event) string {
	p := decodePayload(ev.Payload)
	if outcome, ok := p["outcome"].(map[string]interface{}); ok {
		return str(outcome["summary"])
	}
	return ""
}

// PingPresence marks this CLI endpoint attached on the daemon (GET
// /v1/presence/ping). Presence gates conversation-layer routing: while the
// TUI is attached, CLI-origin approval prompts stay inline instead of also
// pushing to IM. Best-effort by design — a failed ping just lets presence
// expire, which reads as detached.
func (c *Client) PingPresence(ctx context.Context) error {
	q := url.Values{}
	q.Set("platform", "cli")
	q.Set("platform_user_id", clientUserID())
	// active=0 turns the beat into a no-op on the daemon's presence registry:
	// the loop keeps running (a keystroke re-activates the NEXT beat within
	// 30s) but a vacated terminal stops claiming attachment.
	q.Set("active", c.presenceActiveParam())
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/v1/presence/ping?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, _ = io.Copy(io.Discard, httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("presence ping returned %s", httpResp.Status)
	}
	return nil
}

// StartPresencePing runs the idle-TUI heartbeat loop: an immediate ping, then
// one every 30 seconds until the returned stop function is called (or ctx is
// cancelled). Without it an open-but-idle TUI would look detached — the event
// poller only runs during a turn. Failures are silent (best-effort presence).
func (c *Client) StartPresencePing(ctx context.Context) func() {
	loopCtx, cancel := context.WithCancel(ctx)
	go func() {
		_ = c.PingPresence(loopCtx)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				_ = c.PingPresence(loopCtx)
			}
		}
	}()
	return cancel
}

// RespondApproval answers a pending tool-approval request on the daemon
// (decision "approved" or "rejected"), unblocking the waiting run. It backs the
// client TUI's approval panel. scope carries class-grant memory on an approve:
// "" (once), "task", or "person" — same grammar as `/approve [n] task|always`.
func (c *Client) RespondApproval(approvalID, decision, scope string) error {
	req := api.ApprovalRespondRequest{
		Platform:       "cli",
		PlatformUserID: clientUserID(),
		Channel:        "cli",
		ApprovalID:     approvalID,
		Decision:       decision,
		Scope:          scope,
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/approvals/respond", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= http.StatusBadRequest {
		data, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("approval respond failed: %s: %s", httpResp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

// Typed steering failures so the TUI can render specific, honest notices
// without parsing HTTP bodies.
var (
	// ErrNoActiveRun: the daemon has no run to steer (409) — it may have
	// finished between the user's keystroke and the request.
	ErrNoActiveRun = errors.New("no active run to steer")
	// ErrSteerBusy: the run's steering buffer is full (429); retry shortly.
	ErrSteerBusy = errors.New("steering buffer is full; try again in a moment")
)

// SteerRun forwards mid-turn user guidance to the daemon's active run
// (POST /v1/runs/steer). In client mode the run executes inside the daemon
// process, so the TUI's local steering channel can never reach it — this call
// is the only path by which mid-run input reaches the agent loop.
func (c *Client) SteerRun(text string) error {
	req := api.RunSteerRequest{
		Platform:       "cli",
		PlatformUserID: clientUserID(),
		Channel:        "cli",
		Text:           text,
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/runs/steer", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.auth(httpReq)
	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	switch {
	case httpResp.StatusCode == http.StatusConflict:
		return ErrNoActiveRun
	case httpResp.StatusCode == http.StatusTooManyRequests:
		return ErrSteerBusy
	case httpResp.StatusCode >= http.StatusBadRequest:
		data, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("steer failed: %s: %s", httpResp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func decodePayload(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	m := map[string]interface{}{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func str(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func isControlCommand(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "/")
}
