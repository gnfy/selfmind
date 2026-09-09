package control

import (
	"context"
	"testing"
)

// A person's reply is often only meaningful against what they were just
// offered: "2" answering a numbered list of next steps carries a complete
// authorization and reads as nothing alone. Approval triage needs the offer,
// and it needs the LATEST one from the RIGHT channel and person.
func TestPrecedingAssistantOfferReturnsTheOfferBeingAnswered(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	alice, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "alice", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}
	bob, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "bob", "Bob")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}

	// Nothing said yet is not an error; the judge simply gets no context.
	offer, err := store.PrecedingAssistantOffer(ctx, alice.TenantID, alice.PersonID, "cli")
	if err != nil || offer != "" {
		t.Fatalf("empty channel = %q, %v", offer, err)
	}

	for _, message := range []struct{ role, content string }{
		{"assistant", "1. approve in the console  2. authorize me  3. wait"},
		{"user", "2"},
	} {
		if err := store.RecordChannelMessage(ctx, *alice, "cli", "task-1", message.role, message.content); err != nil {
			t.Fatalf("RecordChannelMessage: %v", err)
		}
	}
	// Another person and another channel must not leak in.
	if err := store.RecordChannelMessage(ctx, *bob, "cli", "task-2", "assistant", "bob's offer"); err != nil {
		t.Fatalf("RecordChannelMessage: %v", err)
	}
	if err := store.RecordChannelMessage(ctx, *alice, "wechat", "task-3", "assistant", "wechat offer"); err != nil {
		t.Fatalf("RecordChannelMessage: %v", err)
	}

	offer, err = store.PrecedingAssistantOffer(ctx, alice.TenantID, alice.PersonID, "cli")
	if err != nil {
		t.Fatalf("PrecedingAssistantOffer: %v", err)
	}
	if offer != "1. approve in the console  2. authorize me  3. wait" {
		t.Fatalf("offer = %q", offer)
	}

	// The person's own reply must never come back as the offer: it is the thing
	// being interpreted, not the interpretation.
	if err := store.RecordChannelMessage(ctx, *alice, "cli", "task-1", "assistant", "done, three builds approved"); err != nil {
		t.Fatalf("RecordChannelMessage: %v", err)
	}
	offer, err = store.PrecedingAssistantOffer(ctx, alice.TenantID, alice.PersonID, "cli")
	if err != nil {
		t.Fatalf("PrecedingAssistantOffer: %v", err)
	}
	if offer != "done, three builds approved" {
		t.Fatalf("stale offer returned: %q", offer)
	}
}

// The newest assistant message is not automatically an offer being answered.
// Once the person has opened an unrelated topic it is just the previous turn's
// closing remark, and handing that to the judge as the referent of their new
// words invents a connection that was never made.
func TestPrecedingAssistantOfferIsWithheldOnceTheTopicMovedOn(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	record := func(role, content string) {
		if err := store.RecordChannelMessage(ctx, *identity, "cli", "task-1", role, content); err != nil {
			t.Fatalf("RecordChannelMessage: %v", err)
		}
	}

	record("assistant", "1. approve in the console  2. authorize me")
	// The person's current turn is recorded before the approval path reads the
	// offer, so exactly one of their turns may sit in between.
	record("user", "2")
	offer, err := store.PrecedingAssistantOffer(ctx, identity.TenantID, identity.PersonID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if offer == "" {
		t.Fatal("a direct reply must carry the offer it answers")
	}

	// A second, unrelated turn means the offer is no longer what is being
	// answered.
	record("user", "now look at the other repository")
	offer, err = store.PrecedingAssistantOffer(ctx, identity.TenantID, identity.PersonID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if offer != "" {
		t.Fatalf("a stale offer was presented as the referent of new words: %q", offer)
	}

	// SelfMind answering again makes its reply the current offer.
	record("assistant", "the other repository has no protection; 1. add it  2. skip")
	record("user", "1")
	offer, err = store.PrecedingAssistantOffer(ctx, identity.TenantID, identity.PersonID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if offer != "the other repository has no protection; 1. add it  2. skip" {
		t.Fatalf("offer = %q", offer)
	}
}
