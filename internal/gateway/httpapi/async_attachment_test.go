package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type attachmentCaptureProvider struct {
	*slowLLMProvider
	requests chan llm.ChatRequest
}

func (p *attachmentCaptureProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests <- req
	return p.slowLLMProvider.StreamChat(ctx, req)
}

func TestAsyncAttachmentReachesProvider(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "async_local"
		if queued {
			name = "queue_drain"
		}
		t.Run(name, func(t *testing.T) {
			slow := newSlowLLMProvider("done")
			slow.releaseNow()
			daemon, store, _ := newDetachedRunServer(t, slow)
			cap := &attachmentCaptureProvider{slow, make(chan llm.ChatRequest, 10)}
			daemon.Gateway = router.NewGateway(kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, cap, "test", 1, 1, nil), nil)
			daemon.AttachmentsDir = t.TempDir()
			src := filepath.Join(t.TempDir(), "unique-review-picture.png")
			if err := os.WriteFile(src, []byte("image bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if queued {
				service, _ := testModelChangeService(t)
				daemon.ModelChanges = service
			}
			resp, code := daemon.ProcessMessage(withLocalFilesystemAuthority(context.Background()), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "inspect my image", Async: true, Attachments: []api.MessageAttachment{{Name: "unique-review-picture.png", Path: src, Kind: "image"}}})
			if code != 200 || !resp.Accepted {
				t.Fatalf("not accepted: %d %+v", code, resp)
			}
			if err := os.Remove(src); err != nil {
				t.Fatal(err)
			}
			if queued {
				q, err := store.GetQueued(context.Background(), resp.Identity.TenantID, resp.Turn.QueueID)
				if err != nil {
					t.Fatal(err)
				}
				if len(q.Attachments) != 1 || q.Attachments[0].Path == "" {
					t.Fatal("not saved")
				}
				daemon.ModelChanges = nil
				daemon.DrainQueuedAtBoot(context.Background())
			}
			var request llm.ChatRequest
			select {
			case request = <-cap.requests:
			case <-time.After(3 * time.Second):
				t.Fatal("provider not called")
			}
			waitUntil(t, 3*time.Second, func() bool { return daemon.coordinator().currentActive(resp.Identity.PersonID) == nil }, "run did not finish")
			var managedPath string
			for _, message := range request.Messages {
				for _, line := range strings.Split(message.Content, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "path: ") {
						managedPath = strings.TrimPrefix(strings.TrimSpace(line), "path: ")
					}
				}
			}
			ownedRoot := filepath.Join(daemon.AttachmentsDir, resp.Identity.PersonID) + string(filepath.Separator)
			if !strings.HasPrefix(managedPath, ownedRoot) {
				t.Fatalf("model did not receive a person-owned attachment path: %q", managedPath)
			}
			data, err := os.ReadFile(managedPath)
			if err != nil || string(data) != "image bytes" {
				t.Fatalf("model attachment path lost the submitted bytes: %q, %v", data, err)
			}
		})
	}
}
