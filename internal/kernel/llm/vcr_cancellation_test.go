package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type canceledVCRProvider struct{}

func (canceledVCRProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, context.Canceled
}
func (canceledVCRProvider) StreamChat(context.Context, ChatRequest) (<-chan StreamEvent, error) {
	return nil, context.Canceled
}
func (canceledVCRProvider) ChatCompletion(context.Context, []Message) (string, error) {
	return "", context.Canceled
}

func TestVCRPreservesCallerCancellationWithoutInventingSuccess(t *testing.T) {
	for _, method := range []string{"chat", "stream", "completion"} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			session := "canceled-" + method
			ResetVCRSession(session)
			ctx, cancel := context.WithCancel(WithVCRSession(context.Background(), session))
			cancel()
			invoke := func(v *vcrProvider, ctx context.Context) error {
				switch method {
				case "chat":
					_, err := v.Chat(ctx, ChatRequest{})
					return err
				case "stream":
					_, err := v.StreamChat(ctx, ChatRequest{})
					return err
				default:
					_, err := v.ChatCompletion(ctx, nil)
					return err
				}
			}
			if err := invoke(&vcrProvider{inner: canceledVCRProvider{}, mode: "record", dir: dir}, ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, session, "0000.json"))
			if err != nil {
				t.Fatal(err)
			}
			var c cassette
			if err = json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if !c.CallerCanceled || c.Error != "" || c.Chat != nil || c.Completion != "" || len(c.Events) != 0 {
				t.Fatalf("cancellation misrepresented: %s", raw)
			}
			ResetVCRSession(session)
			// Replay preserves cancellation even when the faster playback has not yet
			// canceled its own context; no inner provider or successful body exists.
			if err = invoke(&vcrProvider{mode: "replay", dir: dir, offline: true}, WithVCRSession(context.Background(), session)); !errors.Is(err, context.Canceled) {
				t.Fatalf("replay lost cancellation: %v", err)
			}
		})
	}
}

func TestVCRDoesNotReclassifyUncanceledProviderFailures(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("temporary provider failure")} {
		c := failedCassette(context.Background(), "completion", err)
		if c.CallerCanceled || c.Error == "" {
			t.Fatalf("uncanceled provider error hidden: %+v", c)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := failedCassette(ctx, "completion", errors.New("authentication failed")); c.CallerCanceled || c.Error == "" {
		t.Fatal("coincident cancellation hid an unrelated provider failure")
	}
}

func TestVCRRejectsConflictingCancellationRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	for _, raw := range []string{
		`{"method":"completion","caller_canceled":true,"error":"provider failed"}`,
		`{"method":"completion","caller_canceled":true,"completion":"invented success"}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := (&vcrProvider{}).load(context.Background(), path, nil); err == nil {
			t.Fatal("conflicting cancellation record accepted")
		}
	}
}
