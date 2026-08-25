package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

const skillDeliveryContractV1 = 1

// SkillPresentationDiagnostics is a redacted, durable truth check. It validates
// receipts from stored bytes and lifecycle rows rather than inferring health
// from recent logs or rebuilding a parallel Skill resolver in the CLI.
type SkillPresentationDiagnostics struct {
	SchemaVersion               int
	CurrentSchemaVersion        int
	Activations                 int
	LegacyActivations           int
	FullActivations             int
	PagedActivations            int
	InvalidDeliveryReceipts     int
	PackageResources            int
	InvalidResourceReceipts     int
	CandidateRefs               int
	TerminalCandidateRefLeaks   int
	TerminalCandidateRefOwners  []string
	CandidateRefsOverDriftLimit int
	Issues                      []SkillPresentationIssue
}

type SkillPresentationIssue struct {
	ID           string                         `json:"id"`
	Code         string                         `json:"code"`
	Severity     string                         `json:"severity"`
	Component    string                         `json:"component"`
	Location     string                         `json:"location"`
	Expected     string                         `json:"expected"`
	Observed     string                         `json:"observed"`
	Cause        string                         `json:"cause"`
	Owner        string                         `json:"owner"`
	Remediations []SkillPresentationRemediation `json:"remediations"`
	Verify       []string                       `json:"verify"`
	Ref          string                         `json:"ref,omitempty"`
}

type SkillPresentationRemediation struct {
	Description string   `json:"description"`
	Commands    []string `json:"commands,omitempty"`
}

func (d SkillPresentationDiagnostics) Healthy() bool {
	return d.InvalidDeliveryReceipts == 0 && d.InvalidResourceReceipts == 0 &&
		d.TerminalCandidateRefLeaks == 0 && d.CandidateRefsOverDriftLimit == 0
}

func (d SkillPresentationDiagnostics) Fatal() bool {
	return d.InvalidDeliveryReceipts > 0 || d.InvalidResourceReceipts > 0 || d.CandidateRefsOverDriftLimit > 0
}

func (s *Store) InspectSkillPresentation(ctx context.Context, tenantID, personID string) (SkillPresentationDiagnostics, error) {
	report := SkillPresentationDiagnostics{
		SchemaVersion: s.schemaVersion, CurrentSchemaVersion: CurrentControlSchemaVersion,
	}
	tenantID = normalizeTenant(tenantID)
	type activationReceipt struct {
		id              string
		controlTenantID string
		skillKey        string
		contractVersion int
		mode            string
		main            string
		hash            string
		deliveredBytes  int
		packageHash     string
		manifestJSON    string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, control_tenant_id, skill_key, delivery_contract_version, delivery_mode,
		delivered_main, delivered_main_hash, delivered_main_bytes, package_hash, resource_manifest_json
		FROM run_skill_activations WHERE identity_tenant_id=? AND person_id=? ORDER BY selected_at DESC`, tenantID, personID)
	if err != nil {
		return report, err
	}
	var receipts []activationReceipt
	for rows.Next() {
		var receipt activationReceipt
		if err := rows.Scan(&receipt.id, &receipt.controlTenantID, &receipt.skillKey, &receipt.contractVersion, &receipt.mode, &receipt.main, &receipt.hash,
			&receipt.deliveredBytes, &receipt.packageHash, &receipt.manifestJSON); err != nil {
			rows.Close()
			return report, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Close(); err != nil {
		return report, err
	}
	if err := rows.Err(); err != nil {
		return report, err
	}

	// Store uses a deliberately small connection pool. Finish consuming and
	// close the activation cursor before looking up package resources, otherwise
	// the diagnostic can wait on its own checked-out connection.
	for _, receipt := range receipts {
		report.Activations++
		if receipt.contractVersion == 0 {
			report.LegacyActivations++
			continue
		}
		valid := true
		location := "run_skill_activations/" + receipt.id
		if receipt.contractVersion != skillDeliveryContractV1 {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("unsupported_delivery_contract", receipt.id,
				location+"/delivery_contract_version", fmt.Sprintf("%d", skillDeliveryContractV1), fmt.Sprintf("%d", receipt.contractVersion)))
		}
		if strings.TrimSpace(receipt.packageHash) == "" {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("missing_package_hash", receipt.id,
				location+"/package_hash", "non-empty immutable package hash", "empty"))
		}
		if receipt.main == "" {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("missing_delivered_main", receipt.id,
				location+"/delivered_main", "non-empty exact delivered bytes", "empty"))
		}
		if receipt.deliveredBytes != len(receipt.main) {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("delivered_byte_count_mismatch", receipt.id,
				location+"/delivered_main_bytes", fmt.Sprintf("%d", len(receipt.main)), fmt.Sprintf("%d", receipt.deliveredBytes)))
		}
		switch receipt.mode {
		case "full":
			report.FullActivations++
		case "paged":
			report.PagedActivations++
		default:
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("invalid_delivery_mode", receipt.id,
				location+"/delivery_mode", "full or paged", diagnosticValue(receipt.mode)))
		}
		digest := sha256.Sum256([]byte(receipt.main))
		expectedHash := fmt.Sprintf("%x", digest[:])
		if receipt.hash != expectedHash {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("delivered_hash_mismatch", receipt.id,
				location+"/delivered_main_hash", shortDiagnosticValue(expectedHash), shortDiagnosticValue(receipt.hash)))
		}
		var manifest []skillPresentationManifestEntry
		if err := json.Unmarshal([]byte(receipt.manifestJSON), &manifest); err != nil {
			valid = false
			appendSkillPresentationIssue(&report, deliveryFinding("invalid_resource_manifest", receipt.id,
				location+"/resource_manifest_json", "valid resource manifest JSON", diagnosticValue(err.Error())))
		} else {
			for _, entry := range manifest {
				var storedHash string
				var storedBytes int
				err := s.db.QueryRowContext(ctx, `SELECT content_hash, content_bytes FROM skill_package_resources
					WHERE control_tenant_id=? AND skill_key=? AND package_hash=? AND resource_path=?`,
					normalizeTenant(receipt.controlTenantID), receipt.skillKey, receipt.packageHash, entry.Path).Scan(&storedHash, &storedBytes)
				if err != nil {
					valid = false
					appendSkillPresentationIssue(&report, resourceFinding("manifest_resource_missing", receipt.id,
						"skill_package_resources/"+receipt.packageHash+"/"+entry.Path,
						"resource row matching manifest", diagnosticValue(err.Error())))
					continue
				}
				if storedHash != entry.ContentHash || storedBytes != entry.Bytes {
					valid = false
					appendSkillPresentationIssue(&report, resourceFinding("manifest_resource_mismatch", receipt.id,
						"skill_package_resources/"+receipt.packageHash+"/"+entry.Path,
						fmt.Sprintf("hash=%s bytes=%d", shortDiagnosticValue(entry.ContentHash), entry.Bytes),
						fmt.Sprintf("hash=%s bytes=%d", shortDiagnosticValue(storedHash), storedBytes)))
				}
			}
		}
		if !valid {
			report.InvalidDeliveryReceipts++
		}
	}

	controlTenants := map[string]bool{tenantID: true}
	for _, receipt := range receipts {
		controlTenants[normalizeTenant(receipt.controlTenantID)] = true
	}
	for controlTenantID := range controlTenants {
		resourceRows, err := s.db.QueryContext(ctx, `SELECT package_hash, resource_path, content_hash, content_body, content_bytes
			FROM skill_package_resources WHERE control_tenant_id=?`, controlTenantID)
		if err != nil {
			return report, err
		}
		for resourceRows.Next() {
			var packageHash, path, hash, body string
			var size int
			if err := resourceRows.Scan(&packageHash, &path, &hash, &body, &size); err != nil {
				resourceRows.Close()
				return report, err
			}
			report.PackageResources++
			digest := sha256.Sum256([]byte(body))
			expectedHash := fmt.Sprintf("%x", digest[:])
			if size != len(body) || hash != expectedHash {
				report.InvalidResourceReceipts++
				ref := packageHash + ":" + path
				appendSkillPresentationIssue(&report, resourceFinding("invalid_resource_receipt", ref,
					"skill_package_resources/"+packageHash+"/"+path,
					fmt.Sprintf("hash=%s bytes=%d", shortDiagnosticValue(expectedHash), len(body)),
					fmt.Sprintf("hash=%s bytes=%d", shortDiagnosticValue(hash), size)))
			}
		}
		if err := resourceRows.Close(); err != nil {
			return report, err
		}
		if err := resourceRows.Err(); err != nil {
			return report, err
		}
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_candidate_refs
		WHERE identity_tenant_id=? AND person_id=?`, tenantID, personID).Scan(&report.CandidateRefs); err != nil {
		return report, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_candidate_refs r
		LEFT JOIN run_work_units w ON w.identity_tenant_id=r.identity_tenant_id AND w.run_id=r.run_id AND w.id=r.work_unit_id
		WHERE r.identity_tenant_id=? AND r.person_id=? AND
		(w.id IS NULL OR w.status IN ('completed','parked','fallback','failed','cancelled'))`, tenantID, personID).Scan(&report.TerminalCandidateRefLeaks); err != nil {
		return report, err
	}
	if report.TerminalCandidateRefLeaks > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT r.candidate_ref, r.run_id, r.work_unit_id,
			CASE WHEN w.id IS NULL THEN 'orphan' ELSE w.status END
			FROM skill_candidate_refs r
			LEFT JOIN run_work_units w ON w.identity_tenant_id=r.identity_tenant_id AND w.run_id=r.run_id AND w.id=r.work_unit_id
			WHERE r.identity_tenant_id=? AND r.person_id=? AND
				(w.id IS NULL OR w.status IN ('completed','parked','fallback','failed','cancelled'))
			ORDER BY r.issued_at, r.candidate_ref LIMIT 5`, tenantID, personID)
		if err != nil {
			return report, err
		}
		for rows.Next() {
			var ref, runID, workUnitID, state string
			if err := rows.Scan(&ref, &runID, &workUnitID, &state); err != nil {
				rows.Close()
				return report, err
			}
			report.TerminalCandidateRefOwners = append(report.TerminalCandidateRefOwners,
				fmt.Sprintf("ref=%s run=%s work_unit=%s owner_state=%s", ref, runID, workUnitID, state))
		}
		if err := rows.Close(); err != nil {
			return report, err
		}
		if err := rows.Err(); err != nil {
			return report, err
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_candidate_refs
		WHERE identity_tenant_id=? AND person_id=? AND drift_count>1`, tenantID, personID).Scan(&report.CandidateRefsOverDriftLimit); err != nil {
		return report, err
	}
	if report.TerminalCandidateRefLeaks > 0 {
		ref := strings.Join(report.TerminalCandidateRefOwners, "; ")
		appendSkillPresentationIssue(&report, candidateFinding("terminal_candidate_ref_leak", ref,
			"skill_candidate_refs", "0 refs owned by terminal work units", fmt.Sprintf("%d", report.TerminalCandidateRefLeaks), "warning"))
	}
	if report.CandidateRefsOverDriftLimit > 0 {
		appendSkillPresentationIssue(&report, candidateFinding("candidate_ref_drift_limit_exceeded", "drift-limit",
			"skill_candidate_refs/drift_count", "drift_count <= 1", fmt.Sprintf("%d refs exceed limit", report.CandidateRefsOverDriftLimit), "fatal"))
	}
	return report, nil
}

type skillPresentationManifestEntry struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Bytes       int    `json:"bytes"`
}

func appendSkillPresentationIssue(report *SkillPresentationDiagnostics, issue SkillPresentationIssue) {
	if report == nil || len(report.Issues) >= 20 {
		return
	}
	report.Issues = append(report.Issues, issue)
}

func deliveryFinding(code, ref, location, expected, observed string) SkillPresentationIssue {
	return SkillPresentationIssue{
		ID: "skill_presentation." + code, Code: code, Severity: "fatal", Component: "delivery_receipt",
		Location: location, Expected: expected, Observed: observed, Ref: ref,
		Cause: "stored activation receipt no longer matches the immutable model-visible main delivery contract",
		Owner: "kernel skill delivery and control activation persistence",
		Remediations: []SkillPresentationRemediation{
			{Description: "Stop the gateway and preserve the affected control.db before repair.", Commands: []string{"selfmind gateway stop", "selfmind doctor --verbose --out <path>"}},
			{Description: "Restore control.db from a verified pre-corruption backup; do not rewrite receipt fields by hand.", Commands: []string{"selfmind maintenance restore-control --backup <path> --yes", "selfmind gateway start"}},
		},
		Verify: []string{"selfmind doctor --verbose", "selfmind doctor --probe-models --verbose"},
	}
}

func resourceFinding(code, ref, location, expected, observed string) SkillPresentationIssue {
	issue := deliveryFinding(code, ref, location, expected, observed)
	issue.Component = "package_resource"
	issue.Cause = "stored package resource bytes or manifest identity no longer match the immutable package receipt"
	issue.Owner = "control Skill package resource persistence"
	return issue
}

func candidateFinding(code, ref, location, expected, observed, severity string) SkillPresentationIssue {
	issue := SkillPresentationIssue{
		ID: "skill_presentation." + code, Code: code, Severity: severity, Component: "candidate_ref",
		Location: location, Expected: expected, Observed: observed, Ref: ref,
		Cause: "candidate-ref lifecycle did not remain within its work-unit contract",
		Owner: "control work-unit lifecycle and gateway Skill selection",
		Remediations: []SkillPresentationRemediation{
			{Description: "Finish or stop the affected work unit so the authoritative terminal transaction can clean its refs.", Commands: []string{"selfmind status", "selfmind stop"}},
			{Description: "Restart on the current binary and rerun diagnostics; do not delete candidate refs directly from SQLite.", Commands: []string{"selfmind gateway restart", "selfmind doctor --verbose"}},
		},
		Verify: []string{"selfmind doctor --verbose"},
	}
	if code == "terminal_candidate_ref_leak" {
		issue.Remediations = []SkillPresentationRemediation{
			{Description: "Preview the exact terminal/orphan candidate refs selected by the transactional cleanup query.", Commands: []string{"selfmind maintenance prune-skill-candidate-refs"}},
			{Description: "Apply only the reviewed terminal/orphan cleanup; live work-unit refs are excluded.", Commands: []string{"selfmind maintenance prune-skill-candidate-refs --apply"}},
		}
		issue.Verify = []string{"selfmind maintenance prune-skill-candidate-refs", "selfmind doctor --verbose"}
	}
	return issue
}

func diagnosticValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "empty"
	}
	return shortDiagnosticValue(value)
}

func shortDiagnosticValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 24 {
		return value
	}
	return value[:24] + "..."
}
