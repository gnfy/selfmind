package control

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrentAccountFirstUseResolvesOnePersonAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	const callers = 24
	type answer struct {
		identity *IdentityContext
		err      error
	}
	results := make(chan answer, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		store := first
		if i%2 != 0 {
			store = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "same-local-user", "Local")
			results <- answer{identity, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var personID, accountID string
	for result := range results {
		if result.err != nil || result.identity == nil {
			t.Fatalf("concurrent first use: identity=%+v err=%v", result.identity, result.err)
		}
		if personID == "" {
			personID, accountID = result.identity.PersonID, result.identity.AccountID
		}
		if result.identity.PersonID != personID || result.identity.AccountID != accountID {
			t.Fatalf("one platform identity split across people: %+v", result.identity)
		}
	}
	var people, accounts int
	if err := first.db.QueryRow(`SELECT COUNT(*) FROM persons WHERE tenant_id = ?`, "default").Scan(&people); err != nil {
		t.Fatal(err)
	}
	if err := first.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE tenant_id = ? AND platform = ? AND platform_user_id = ?`,
		"default", "cli", "same-local-user").Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if people != 1 || accounts != 1 {
		t.Fatalf("concurrent first use left %d persons and %d accounts", people, accounts)
	}
}
