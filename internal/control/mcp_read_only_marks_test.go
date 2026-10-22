package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMCPReadOnlyMarkIsPersonalIdentityBoundAndAudited(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "777", "Owner phone")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "telegram", "888", "Other")
	if err != nil {
		t.Fatal(err)
	}
	request := MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, ServerName: "github",
		ServerIdentity: "mcpsrv_a", ToolName: "get_issue", ActorAccountID: owner.AccountID, ActorChannel: "session-1"}
	mark, created, err := store.MarkMCPToolReadOnly(ctx, request)
	if err != nil || !created || mark.ID == "" || mark.MarkedByAccountID != owner.AccountID ||
		mark.MarkedChannel != "session-1" || mark.MarkedAt.IsZero() || !mark.RevokedAt.IsZero() {
		t.Fatalf("mark = %+v created=%t err=%v", mark, created, err)
	}
	again, created, err := store.MarkMCPToolReadOnly(ctx, request)
	if err != nil || created || again.ID != mark.ID {
		t.Fatalf("re-marking duplicated the marking: %+v created=%t err=%v", again, created, err)
	}
	marked := func(person, identity, tool string) bool {
		t.Helper()
		ok, err := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, person, "github", identity, tool, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !marked(owner.PersonID, "mcpsrv_a", "get_issue") {
		t.Fatal("the marking does not apply to its own tool and identity")
	}
	if marked(owner.PersonID, "mcpsrv_b", "get_issue") || marked(owner.PersonID, "mcpsrv_a", "get_issues") ||
		marked(owner.PersonID, "mcpsrv_a", "GET_ISSUE") || marked(other.PersonID, "mcpsrv_a", "get_issue") {
		t.Fatal("the marking reached another identity, tool spelling, or person")
	}
	if marks, err := store.ListMCPReadOnlyMarks(ctx, other.TenantID, other.PersonID); err != nil || len(marks) != 0 {
		t.Fatalf("another person sees the marking: %+v err=%v", marks, err)
	}
	// A schedule authorised before the marking does not carry it.
	for cutoff, want := range map[time.Time]bool{mark.MarkedAt: true, mark.MarkedAt.Add(-time.Second): false} {
		if got, err := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_a", "get_issue", cutoff); err != nil || got != want {
			t.Fatalf("marking with cutoff %s = %t err=%v, want %t", cutoff, got, err, want)
		}
	}

	// The same name under a new server identity is a separate assertion.
	if _, created, err := store.MarkMCPToolReadOnly(ctx, MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ServerName: "github", ServerIdentity: "mcpsrv_b", ToolName: "get_issue",
		ActorAccountID: phone.AccountID, ActorChannel: "777"}); err != nil || !created {
		t.Fatalf("mark under a new identity: created=%t err=%v", created, err)
	}
	marks, err := store.ListMCPReadOnlyMarks(ctx, owner.TenantID, owner.PersonID)
	if err != nil || len(marks) != 2 {
		t.Fatalf("listed marks = %+v err=%v", marks, err)
	}

	// Unmarking withdraws every identity's marking and keeps the audit.
	revoked, err := store.UnmarkMCPToolReadOnly(ctx, MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ServerName: "github", ToolName: "get_issue", ActorAccountID: phone.AccountID, ActorChannel: "777"})
	if err != nil || revoked != 2 {
		t.Fatalf("revoked=%d err=%v", revoked, err)
	}
	if marked(owner.PersonID, "mcpsrv_a", "get_issue") || marked(owner.PersonID, "mcpsrv_b", "get_issue") {
		t.Fatal("an unmarked tool is still marked")
	}
	var revokedBy, revokedChannel string
	var revokedAt int64
	if err := store.db.QueryRowContext(ctx, `SELECT revoked_at, revoked_by_account_id, revoked_channel
		FROM mcp_read_only_marks WHERE id = ?`, mark.ID).Scan(&revokedAt, &revokedBy, &revokedChannel); err != nil ||
		revokedAt == 0 || revokedBy != phone.AccountID || revokedChannel != "777" {
		t.Fatalf("revocation audit = %d %q %q err=%v", revokedAt, revokedBy, revokedChannel, err)
	}
	remarked, created, err := store.MarkMCPToolReadOnly(ctx, request)
	if err != nil || !created || remarked.ID == mark.ID {
		t.Fatalf("marking again after a revocation reused the old audit row: %+v created=%t err=%v", remarked, created, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_read_only_marks WHERE person_id = ?`, owner.PersonID).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("audit rows = %d err=%v", rows, err)
	}
}

// Only the person's own active endpoint can mark or unmark, and the names are
// exact: a pattern or a control character cannot name a tool.
func TestMCPReadOnlyMarkRejectsForeignActorsAndInexactNames(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "telegram", "888", "Other")
	if err != nil {
		t.Fatal(err)
	}
	base := MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, ServerName: "github",
		ServerIdentity: "mcpsrv_a", ToolName: "get_issue", ActorAccountID: owner.AccountID, ActorChannel: "session-1"}
	for name, mutate := range map[string]func(*MCPReadOnlyMarkRequest){
		"another person's account":   func(r *MCPReadOnlyMarkRequest) { r.ActorAccountID = other.AccountID },
		"another account's IM chat":  func(r *MCPReadOnlyMarkRequest) { r.ActorChannel = "888" },
		"no channel":                 func(r *MCPReadOnlyMarkRequest) { r.ActorChannel = "" },
		"unknown account":            func(r *MCPReadOnlyMarkRequest) { r.ActorAccountID = "acct_missing" },
		"marking for another person": func(r *MCPReadOnlyMarkRequest) { r.PersonID = other.PersonID },
	} {
		request := base
		mutate(&request)
		if _, _, err := store.MarkMCPToolReadOnly(ctx, request); !errors.Is(err, ErrMCPReadOnlyMarkActor) {
			t.Errorf("%s: mark err = %v", name, err)
		}
		if _, err := store.UnmarkMCPToolReadOnly(ctx, request); !errors.Is(err, ErrMCPReadOnlyMarkActor) {
			t.Errorf("%s: unmark err = %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*MCPReadOnlyMarkRequest){
		"empty tool":        func(r *MCPReadOnlyMarkRequest) { r.ToolName = "" },
		"control character": func(r *MCPReadOnlyMarkRequest) { r.ToolName = "get\nissue" },
		"empty server":      func(r *MCPReadOnlyMarkRequest) { r.ServerName = "" },
		"no identity":       func(r *MCPReadOnlyMarkRequest) { r.ServerIdentity = "" },
	} {
		request := base
		mutate(&request)
		if _, _, err := store.MarkMCPToolReadOnly(ctx, request); !errors.Is(err, ErrMCPReadOnlyMarkInvalid) {
			t.Errorf("%s: mark err = %v", name, err)
		}
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_read_only_marks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused requests wrote %d row(s) err=%v", rows, err)
	}
}

// A database an earlier development build stamped v26 lacks the marking
// table. Opening it reports the gap in schema health instead of claiming a
// healthy v26, and every marking call returns one typed, explainable error
// rather than a raw SQL failure.
func TestMissingMCPReadOnlyMarkTableIsReportedAndTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if missing := store.SchemaStatus().MissingTables; len(missing) != 0 {
		t.Fatalf("a complete v26 store reports missing tables %v", missing)
	}
	owner, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE mcp_read_only_marks`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatalf("an incomplete v26 store did not open: %v", err)
	}
	defer store.Close()
	status := store.SchemaStatus()
	if status.Version != CurrentControlSchemaVersion || len(status.MissingTables) != 1 || status.MissingTables[0] != "mcp_read_only_marks" {
		t.Fatalf("schema status = %+v, want the missing marking table reported", status)
	}
	request := MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, ServerName: "github",
		ServerIdentity: "mcpsrv_a", ToolName: "get_issue", ActorAccountID: owner.AccountID, ActorChannel: "session-1"}
	if _, _, err := store.MarkMCPToolReadOnly(ctx, request); !errors.Is(err, ErrMCPReadOnlyMarksUnavailable) {
		t.Fatalf("mark err = %v", err)
	}
	if _, err := store.UnmarkMCPToolReadOnly(ctx, request); !errors.Is(err, ErrMCPReadOnlyMarksUnavailable) {
		t.Fatalf("unmark err = %v", err)
	}
	if _, err := store.ListMCPReadOnlyMarks(ctx, owner.TenantID, owner.PersonID); !errors.Is(err, ErrMCPReadOnlyMarksUnavailable) {
		t.Fatalf("list err = %v", err)
	}
	if marked, err := store.MCPToolMarkedReadOnly(ctx, owner.TenantID, owner.PersonID, "github", "mcpsrv_a", "get_issue", time.Time{}); marked ||
		!errors.Is(err, ErrMCPReadOnlyMarksUnavailable) {
		t.Fatalf("lookup = %t err=%v", marked, err)
	}
}

// Two endpoints marking the same tool at once record one marking.
func TestConcurrentMCPReadOnlyMarksRecordOneMarking(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	request := MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, ServerName: "notion",
		ServerIdentity: "mcpsrv_n", ToolName: "search", ActorAccountID: owner.AccountID, ActorChannel: "session-1"}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mark, _, err := store.MarkMCPToolReadOnly(ctx, request)
			ids[i], errs[i] = mark.ID, err
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("concurrent marking %d = %q err=%v, want %q", i, ids[i], errs[i], ids[0])
		}
	}
}
