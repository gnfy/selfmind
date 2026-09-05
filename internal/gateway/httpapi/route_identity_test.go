package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control/controltest"
)

func TestRouteIdentityForPersonNeverDriftsToCLILocal(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)

	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "weixin", "wxid_owner", "Owner WeChat"); err != nil {
		t.Fatal(err)
	}
	local, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if local.PersonID == owner.PersonID {
		t.Fatal("fixture must contain a separate cli:local person")
	}

	daemon := &Server{Control: store}
	got := daemon.routeIdentityForPerson(ctx, owner.TenantID, owner.PersonID, "cli-session-id", "cli", nil)
	if got.PersonID != owner.PersonID || got.Platform != "cli" || got.PlatformUserID != "gnfy" {
		t.Fatalf("CLI route drifted: %+v", got)
	}

	got = daemon.routeIdentityForPerson(ctx, owner.TenantID, owner.PersonID, "wxid_owner", "cli", nil)
	if got.PersonID != owner.PersonID || got.Platform != "weixin" || got.PlatformUserID != "wxid_owner" {
		t.Fatalf("exact IM channel was not preserved: %+v", got)
	}

	got = daemon.routeIdentityForPerson(ctx, owner.TenantID, owner.PersonID, "", "cli", local)
	if got.PersonID != owner.PersonID || got.PlatformUserID == "local" {
		t.Fatalf("foreign-person fallback was accepted: %+v", got)
	}
}
