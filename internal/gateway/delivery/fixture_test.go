package delivery

import (
	"context"
	"testing"

	"selfmind/internal/control"
)

// Delivery fixtures have real active account bindings, like gateway-produced
// outbox rows. Ownership regressions construct and change their own bindings.
func openDeliveryFixture(t *testing.T) (*control.Store, error) {
	t.Helper()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		return nil, err
	}
	for _, peer := range []string{"wx", "wx-user", "wx-a", "wx-b"} {
		if _, err := store.BindAccount(context.Background(), "default", "p1", "weixin", peer, "fixture peer"); err != nil {
			store.Close()
			return nil, err
		}
	}
	if _, err := store.BindAccount(context.Background(), "default", "owner", "telegram", "user-1", "fixture peer"); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}
