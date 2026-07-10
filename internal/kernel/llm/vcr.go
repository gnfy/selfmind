package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// ResetVCRSession clears the per-session call counter so the next recorded or
// replayed call for the session starts at 0000.json. The eval runner calls it
// at the start of EVERY case execution: the counter is process-global and was
// never reset, so a case re-run in the same process continued numbering where
// the previous run stopped — recording cassettes with a 0001+ hole (observed
// live: .vcr/continuity_task_attach/ held 0001-0003 and no 0000) and failing
// replays that probe indices past the recorded range.
func ResetVCRSession(session string) {
	if strings.TrimSpace(session) == "" {
		return
	}
	vcrCounters.Delete(session)
}

// WipeVCRSessionRecordings removes a session's cassette directory. Record mode
// calls it before recording a case so a re-record never interleaves files from
// a previous recording generation. An empty dir resolves like the recorder does
// (SELFMIND_EVAL_VCR_DIR, else ".vcr") so the wipe hits the same directory the
// new recording will write.
func WipeVCRSessionRecordings(dir, session string) error {
	if strings.TrimSpace(session) == "" {
		return nil
	}
	if strings.TrimSpace(dir) == "" {
		dir = vcrDir()
	}
	return os.RemoveAll(filepath.Join(dir, sanitizeVCR(session)))
}

// VCRRecordMode reports whether the eval VCR is in record mode (the runner
// wipes stale cassettes for a case before recording it).
func VCRRecordMode() bool { return vcrMode() == "record" }

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

// HasCassetteSession reports whether a COMPLETE recorded cassette exists for a
// session (case id) under dir. selfcheck uses it to replay recorded cases and
// skip the rest rather than going live. An empty dir means the default ".vcr".
//
// Complete means: 0000.json exists and the numbered files are gap-free
// (0000..max). Replay is position-keyed from 0000, so a directory missing 0000
// or holding a gap can never replay — treating "any *.json" as valid would mask
// exactly the counter-contamination corruption ResetVCRSession prevents.
func HasCassetteSession(dir, session string) bool {
	if strings.TrimSpace(dir) == "" {
		dir = ".vcr"
	}
	entries, err := os.ReadDir(filepath.Join(dir, sanitizeVCR(session)))
	if err != nil {
		return false
	}
	seen := make(map[int]bool)
	max := -1
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		seen[n] = true
		if n > max {
			max = n
		}
	}
	if max < 0 || !seen[0] {
		return false
	}
	for i := 0; i <= max; i++ {
		if !seen[i] {
			return false
		}
	}
	return true
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
	Error      string          `json:"error,omitempty"`
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

func cassetteMiss(path, method string, recorded *cassette, loadErr error) error {
	recordedMethod := ""
	if recorded != nil {
		recordedMethod = recorded.Method
	}
	if loadErr != nil {
		return fmt.Errorf("%w: method=%s path=%s: %v", ErrCassetteMiss, method, path, loadErr)
	}
	return fmt.Errorf("%w: method=%s path=%s recorded_method=%s", ErrCassetteMiss, method, path, recordedMethod)
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
	if v.mode == "replay" {
		key, ok := v.nextKey(ctx)
		if !ok {
			return v.inner.StreamChat(ctx, req)
		}
		c, loadErr := v.load(key)
		if loadErr == nil && c.Method == "stream" {
			if c.Error != "" {
				return nil, errors.New(c.Error)
			}
			return replayStream(c.Events), nil
		}
		if v.offline {
			return nil, cassetteMiss(key, "stream", c, loadErr)
		}
		return v.inner.StreamChat(ctx, req) // cassette miss → live fallback
	}
	key, ok := v.nextKey(ctx)
	in, err := v.inner.StreamChat(ctx, req)
	if err != nil {
		if ok {
			v.save(key, cassette{Method: "stream", Error: err.Error()})
		}
		return nil, err
	}
	if !ok {
		return in, nil
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
	if v.mode == "replay" {
		key, ok := v.nextKey(ctx)
		if !ok {
			return v.inner.Chat(ctx, req)
		}
		c, loadErr := v.load(key)
		if loadErr == nil && c.Method == "chat" {
			if c.Error != "" {
				return nil, errors.New(c.Error)
			}
			if c.Chat != nil {
				return c.Chat, nil
			}
		}
		if v.offline {
			return nil, cassetteMiss(key, "chat", c, loadErr)
		}
		return v.inner.Chat(ctx, req)
	}
	key, ok := v.nextKey(ctx)
	resp, err := v.inner.Chat(ctx, req)
	if err != nil {
		if ok {
			v.save(key, cassette{Method: "chat", Error: err.Error()})
		}
		return resp, err
	}
	if resp == nil {
		return resp, err
	}
	if ok {
		v.save(key, cassette{Method: "chat", Chat: resp})
	}
	return resp, err
}

func (v *vcrProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	if v.mode == "replay" {
		key, ok := v.nextKey(ctx)
		if !ok {
			return v.inner.ChatCompletion(ctx, messages)
		}
		c, loadErr := v.load(key)
		if loadErr == nil && c.Method == "completion" {
			if c.Error != "" {
				return "", errors.New(c.Error)
			}
			return c.Completion, nil
		}
		if v.offline {
			return "", cassetteMiss(key, "completion", c, loadErr)
		}
		return v.inner.ChatCompletion(ctx, messages)
	}
	key, ok := v.nextKey(ctx)
	text, err := v.inner.ChatCompletion(ctx, messages)
	if err != nil {
		if ok {
			v.save(key, cassette{Method: "completion", Error: err.Error()})
		}
		return text, err
	}
	if ok {
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

// SupportsNativeTools forwards the capability probe to the wrapped provider so
// VCR wrapping never changes prompt assembly.
func (v *vcrProvider) SupportsNativeTools() bool { return ProviderSupportsNativeTools(v.inner) }
