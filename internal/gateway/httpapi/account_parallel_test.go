package httpapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

// Two terminals opening on a fresh installation resolve the same person before
// admission. Their requests must not race into a UNIQUE error or create an
// orphaned person; each session then owns only its own Run.
func TestTwoFreshCLISessionsResolveOnePersonAndAdmitSeparateRuns(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	if err := daemon.ConfigureWorkRunCapacity(2, 2); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		resp api.MessageResponse
		code int
	}
	results := make(chan answer, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, channel := range []string{"terminal-a", "terminal-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{
				Platform: "cli", PlatformUserID: "local", Channel: channel,
				Content: "explain this session", Async: true,
			})
			results <- answer{resp, code}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var owner string
	for result := range results {
		if result.code != 200 || !result.resp.Accepted || result.resp.Identity == nil {
			provider.releaseNow()
			t.Fatalf("fresh CLI session was rejected: status=%d response=%+v", result.code, result.resp)
		}
		if owner == "" {
			owner = result.resp.Identity.PersonID
		} else if result.resp.Identity.PersonID != owner {
			provider.releaseNow()
			t.Fatalf("two sessions split into people: %s != %s", result.resp.Identity.PersonID, owner)
		}
	}
	identity, err := store.ResolveAccount(context.Background(), "default", "cli", "local")
	if err != nil || identity == nil || identity.PersonID != owner {
		provider.releaseNow()
		t.Fatalf("persisted account mismatch: %+v, %v", identity, err)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.ActiveRunCount() == 0 }, "fresh runs did not settle")
}
