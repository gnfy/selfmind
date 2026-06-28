package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrCassetteMiss is returned in strict offline mode (SELFMIND_EVAL_OFFLINE) when
// a call has no recorded cassette, instead of silently falling back to the live
// provider. This keeps CI and `selfmind selfcheck` deterministic and incapable
// of burning provider quota.
var ErrCassetteMiss = errors.New("vcr: cassette miss in offline mode (record it first)")

// VCR (record/replay) makes real-provider eval runs reproducible and free:
// record once against the live model, then replay deterministically with no
// network and no rate limits.
//
// Keying is by call SEQUENCE within a session, not by request content. An
// agent's requests embed volatile data (random task ids, timestamps), so a
// content hash would miss on every replay. The Nth model call of a session
// instead maps to cassette file N — and because replay returns identical model
// outputs, the agent makes the identical sequence of calls, so positions line
// up. The eval harness sets the session id (the case id) on the turn context;
// background/aux calls without a session fall through to the live provider.

type vcrSessionCtxKey struct{}

// WithVCRSession tags a context so provider calls made under it are recorded or
// replayed against the named session.
func WithVCRSession(ctx context.Context, session string) context.Context {
	if strings.TrimSpace(session) == "" {
		return ctx
	}
	return context.WithValue(ctx, vcrSessionCtxKey{}, session)
}

func vcrSessionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(vcrSessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}

var vcrCounters sync.Map // session -> *atomic.Int64

func vcrMode() string { return strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_EVAL_VCR"))) }
func vcrDir() string {
	if d := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_VCR_DIR")); d != "" {
		return d
	}
	return ".vcr"
}

// vcrOffline reports strict offline mode: a cassette miss is an error, never a
// live call.
func vcrOffline() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_EVAL_OFFLINE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// HasCassetteSession reports whether a recorded cassette exists for a session
// (case id) under dir. selfcheck uses it to replay recorded cases and skip the
// rest rather than going live. An empty dir means the default ".vcr".
func HasCassetteSession(dir, session string) bool {
	if strings.TrimSpace(dir) == "" {
		dir = ".vcr"
	}
	_, err := os.Stat(filepath.Join(dir, sanitizeVCR(session), "0000.json"))
	return err == nil
}

// MaybeWrapVCR wraps a provider when SELFMIND_EVAL_VCR is record|replay (eval
// harness), or — failing that — when the flight recorder is on (records normal
// runs into the flight dir for later `eval capture`). No-op otherwise, so
// production paths are untouched.
func MaybeWrapVCR(inner Provider) Provider {
	mode := vcrMode()
	if mode == "record" || mode == "replay" {
		return &vcrProvider{inner: inner, mode: mode, dir: vcrDir(), offline: vcrOffline()}
	}
	if FlightEnabled() {
		// Flight recording is VCR record mode writing to the flight dir, keyed by
		// the per-turn session the kernel sets.
		return &vcrProvider{inner: inner, mode: "record", dir: FlightDir()}
	}
	return inner
}

type vcrProvider struct {
	inner   Provider
	mode    string
	dir     string
	offline bool // strict: cassette miss errors instead of falling back to live
}

type recordedEvent struct {
	Content         string                 `json:"content,omitempty"`
	ToolCalls       []ToolCall             `json:"tool_calls,omitempty"`
	Usage           *UsageStats            `json:"usage,omitempty"`
	FinishReason    string                 `json:"finish_reason,omitempty"`
	EventType       string                 `json:"event_type,omitempty"`
	ToolName        string                 `json:"tool_name,omitempty"`
	ToolCallID      string                 `json:"tool_call_id,omitempty"`
	ToolArgs        string                 `json:"tool_args,omitempty"`
	ToolResult      string                 `json:"tool_result,omitempty"`
	DurationSeconds float64                `json:"duration_seconds,omitempty"`
	Payload         map[string]interface{} `json:"payload,omitempty"`
	Err             string                 `json:"err,omitempty"`
}

type cassette struct {
	Method     string          `json:"method"`
	Events     []recordedEvent `json:"events,omitempty"`
	Chat       *ChatResponse   `json:"chat,omitempty"`
	Completion string          `json:"completion,omitempty"`
}

func (v *vcrProvider) nextKey(ctx context.Context) (string, bool) {
	session := vcrSessionFromContext(ctx)
	if session == "" {
		return "", false
	}
	ctr, _ := vcrCounters.LoadOrStore(session, new(atomic.Int64))
	n := ctr.(*atomic.Int64).Add(1) - 1
	return filepath.Join(v.dir, sanitizeVCR(session), fmt.Sprintf("%04d.json", n)), true
}

func (v *vcrProvider) load(path string) (*cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (v *vcrProvider) save(path string, c cassette) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func (v *vcrProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	key, ok := v.nextKey(ctx)
	if !ok {
		return v.inner.StreamChat(ctx, req)
	}
	if v.mode == "replay" {
		if c, err := v.load(key); err == nil && c.Method == "stream" {
			return replayStream(c.Events), nil
		}
		if v.offline {
			return nil, ErrCassetteMiss
		}
		return v.inner.StreamChat(ctx, req) // cassette miss → live fallback
	}
	in, err := v.inner.StreamChat(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		var rec []recordedEvent
		for ev := range in {
			rec = append(rec, toRecorded(ev))
			out <- ev
		}
		v.save(key, cassette{Method: "stream", Events: rec})
	}()
	return out, nil
}

func (v *vcrProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	key, ok := v.nextKey(ctx)
	if !ok {
		return v.inner.Chat(ctx, req)
	}
	if v.mode == "replay" {
		if c, err := v.load(key); err == nil && c.Method == "chat" && c.Chat != nil {
			return c.Chat, nil
		}
		if v.offline {
			return nil, ErrCassetteMiss
		}
		return v.inner.Chat(ctx, req)
	}
	resp, err := v.inner.Chat(ctx, req)
	if err == nil && resp != nil {
		v.save(key, cassette{Method: "chat", Chat: resp})
	}
	return resp, err
}

func (v *vcrProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	key, ok := v.nextKey(ctx)
	if !ok {
		return v.inner.ChatCompletion(ctx, messages)
	}
	if v.mode == "replay" {
		if c, err := v.load(key); err == nil && c.Method == "completion" {
			return c.Completion, nil
		}
		if v.offline {
			return "", ErrCassetteMiss
		}
		return v.inner.ChatCompletion(ctx, messages)
	}
	text, err := v.inner.ChatCompletion(ctx, messages)
	if err == nil {
		v.save(key, cassette{Method: "completion", Completion: text})
	}
	return text, err
}

func replayStream(events []recordedEvent) <-chan StreamEvent {
	ch := make(chan StreamEvent, len(events)+1)
	for _, e := range events {
		ch <- fromRecorded(e)
	}
	close(ch)
	return ch
}

func toRecorded(e StreamEvent) recordedEvent {
	r := recordedEvent{
		Content: e.Content, ToolCalls: e.ToolCalls, Usage: e.Usage, FinishReason: e.FinishReason,
		EventType: e.EventType, ToolName: e.ToolName, ToolCallID: e.ToolCallID, ToolArgs: e.ToolArgs,
		ToolResult: e.ToolResult, DurationSeconds: e.DurationSeconds, Payload: e.Payload,
	}
	if e.Err != nil {
		r.Err = e.Err.Error()
	}
	return r
}

func fromRecorded(r recordedEvent) StreamEvent {
	e := StreamEvent{
		Content: r.Content, ToolCalls: r.ToolCalls, Usage: r.Usage, FinishReason: r.FinishReason,
		EventType: r.EventType, ToolName: r.ToolName, ToolCallID: r.ToolCallID, ToolArgs: r.ToolArgs,
		ToolResult: r.ToolResult, DurationSeconds: r.DurationSeconds, Payload: r.Payload,
	}
	if r.Err != "" {
		e.Err = fmt.Errorf("%s", r.Err)
	}
	return e
}

func sanitizeVCR(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "session"
	}
	return out
}
