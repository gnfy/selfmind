package tools

// Deterministic governance coverage for the skill curator, complementing
// TestCuratorDryRunArchiveAndRestore / TestCuratorSkipsCatalogInstalledSkills
// in skill_runtime_test.go: pin protection, manual-source protection, and a
// real (non-dry) archive run with its learning-audit record and restore.
// Staleness is crafted the way the curator measures it: it reads the sidecar
// usage record's LastUsed (parseSkillActivityTime), so tests backdate that.

import (
	"strings"
	"testing"
	"time"
)

func backdateSkillUsage(t *testing.T, tenantID, name, source string, days int) {
	t.Helper()
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	old := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	if err := updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		rec.Source = source
		rec.LastUsed = old
	}); err != nil {
		t.Fatalf("usage update: %v", err)
	}
}

func TestCuratorLeavesPinnedAgentCreatedSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "pinned-flow", "Pinned reusable flow.")
	if err := MarkSkillCreated("default", "pinned-flow", SkillSourceAgentCreated, "test"); err != nil {
		t.Fatalf("mark created: %v", err)
	}
	backdateSkillUsage(t, "default", "pinned-flow", SkillSourceAgentCreated, 120)
	if err := SetSkillPinned("default", "pinned-flow", true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	resp, err := RunCuratorForTenantWithOptions("default", CuratorOptions{})
	if err != nil {
		t.Fatalf("run curator: %v", err)
	}
	if strings.Contains(resp, "archived pinned-flow") || strings.Contains(resp, "marked stale pinned-flow") {
		t.Fatalf("curator must not touch a pinned skill, got:\n%s", resp)
	}
	info, err := findSkill("default", "pinned-flow")
	if err != nil {
		t.Fatalf("pinned skill should remain active: %v", err)
	}
	if info.State != SkillStateActive || !info.Pinned {
		t.Fatalf("unexpected pinned skill state after curator: %+v", info)
	}
}

func TestCuratorLeavesManualStaleSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "manual-flow", "Hand-authored flow.")
	backdateSkillUsage(t, "default", "manual-flow", SkillSourceManual, 120)

	resp, err := RunCuratorForTenantWithOptions("default", CuratorOptions{})
	if err != nil {
		t.Fatalf("run curator: %v", err)
	}
	if strings.Contains(resp, "archived manual-flow") || strings.Contains(resp, "marked stale manual-flow") {
		t.Fatalf("curator governance: only agent-created skills may be archived, got:\n%s", resp)
	}
	info, err := findSkill("default", "manual-flow")
	if err != nil {
		t.Fatalf("manual skill should remain active: %v", err)
	}
	if info.State != SkillStateActive || info.Source != SkillSourceManual {
		t.Fatalf("unexpected manual skill state after curator: %+v", info)
	}
}

func TestCuratorRunArchivesStaleAgentSkillWithAuditAndRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	createTestSkill(t, "default", "expired-flow", "Once useful flow.")
	if err := MarkSkillCreated("default", "expired-flow", SkillSourceAgentCreated, "test"); err != nil {
		t.Fatalf("mark created: %v", err)
	}
	backdateSkillUsage(t, "default", "expired-flow", SkillSourceAgentCreated, 120)

	resp, err := RunCuratorForTenantWithOptions("default", CuratorOptions{})
	if err != nil {
		t.Fatalf("run curator: %v", err)
	}
	if !strings.Contains(resp, "archived expired-flow") {
		t.Fatalf("curator should archive the stale agent-created skill, got:\n%s", resp)
	}
	if _, err := findSkill("default", "expired-flow"); err == nil {
		t.Fatal("archived skill must not stay active")
	}

	// The archive must leave a learning-audit record.
	changes, err := ListSkillLearningChanges("default", "expired-flow", 20)
	if err != nil {
		t.Fatalf("list learning changes: %v", err)
	}
	foundArchive := false
	for _, c := range changes {
		if c.Action == "archive" && c.Kind == "skill" {
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("no archive learning-audit record for expired-flow: %+v", changes)
	}

	// Restore brings the skill back to active.
	if _, err := RestoreSkillForTenant("default", "expired-flow"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	info, err := findSkill("default", "expired-flow")
	if err != nil {
		t.Fatalf("restored skill should be active: %v", err)
	}
	if info.State != SkillStateActive {
		t.Fatalf("unexpected restored skill state: %+v", info)
	}
}
