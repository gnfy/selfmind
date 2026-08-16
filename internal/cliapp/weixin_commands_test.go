package cliapp

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/platform/config"
)

func TestResolveWeixinOwnerUsesCurrentCLIIdentity(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, err := resolveWeixinOwner(context.Background(), store, control.DefaultTenantID, "local-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cliIdentity, err := store.ResolveAccount(context.Background(), control.DefaultTenantID, "cli", "local-user")
	if err != nil {
		t.Fatal(err)
	}
	if cliIdentity == nil || owner != cliIdentity.PersonID {
		t.Fatalf("owner = %q, CLI identity = %#v", owner, cliIdentity)
	}

	if _, err := store.BindAccount(context.Background(), control.DefaultTenantID, owner, "weixin", "wx-owner", "Weixin owner"); err != nil {
		t.Fatal(err)
	}
	wxIdentity, err := store.ResolveAccount(context.Background(), control.DefaultTenantID, "weixin", "wx-owner")
	if err != nil {
		t.Fatal(err)
	}
	if wxIdentity == nil || wxIdentity.PersonID != cliIdentity.PersonID {
		t.Fatalf("Weixin identity = %#v, want person %q", wxIdentity, cliIdentity.PersonID)
	}
}

func TestResolveWeixinOwnerPreservesConfiguredBinding(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, err := resolveWeixinOwner(context.Background(), store, control.DefaultTenantID, "different-local-user", "", "person-existing")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "person-existing" {
		t.Fatalf("owner = %q", owner)
	}
	identity, err := store.ResolveAccount(context.Background(), control.DefaultTenantID, "cli", "different-local-user")
	if err != nil {
		t.Fatal(err)
	}
	if identity != nil {
		t.Fatalf("configured owner should not create a new CLI identity: %#v", identity)
	}
}

func TestApplyWeixinCrossEndpointBindingIsSafeAndIdempotent(t *testing.T) {
	wx := config.WeixinConfig{
		DMPolicy:  "open",
		AllowFrom: []string{"existing-user", "WX-OWNER"},
	}
	applyWeixinCrossEndpointBinding(&wx, " person-owner ", "wx-owner")

	if wx.OwnerPersonID != "person-owner" {
		t.Fatalf("owner = %q", wx.OwnerPersonID)
	}
	if wx.DMPolicy != "allowlist" {
		t.Fatalf("dm policy = %q", wx.DMPolicy)
	}
	if len(wx.AllowFrom) != 2 {
		t.Fatalf("allow_from = %#v", wx.AllowFrom)
	}
}
