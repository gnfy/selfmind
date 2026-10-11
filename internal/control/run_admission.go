package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// AdmissionSnapshotVersion is the snapshot contract this binary writes, and the
// only one it reads. Reads reject unknown fields, so any change to the field
// set, or to which fields a snapshot may omit, needs a new version and a
// decoder for each older version still stored; a golden wire-form test pins
// v1. Pair a bump with a control schema version when an older binary must
// refuse the database rather than quarantine the rows it cannot read.
const AdmissionSnapshotVersion = 1

// Kinds of durable record an admission input reference may name.
const (
	AdmissionInputEvent          = "event"
	AdmissionInputChannelMessage = "channel_message"
	AdmissionInputQueue          = "queue"
)

// admissionSummaryMaxRunes matches the Run input summary bound. It counts
// characters so CJK text keeps the same visible length as ASCII.
const admissionSummaryMaxRunes = 240

// admissionIdentifierMaxBytes bounds every other snapshot string.
const admissionIdentifierMaxBytes = 256

// The values a snapshot may record. They mirror the producers' names (kernel
// task classes, tool modes, and plan and web policies; tools approval modes
// and execution profiles; gateway run origins) because control may not import
// those packages. A new producer value must be added here before it can be
// recorded; TestAdmissionAcceptsEveryProducerValue checks today's producers.
var (
	admissionTaskClasses = map[string]bool{"simple_answer": true, "coding_example": true, "stable_advice": true,
		"external_lookup": true, "repo_task": true, "debug_task": true, "ci_cd_task": true, "general_task": true}
	admissionToolModes         = map[string]bool{"none": true, "web": true, "local_read": true, "local_write": true, "full": true}
	admissionPlanPolicies      = map[string]bool{"disabled": true, "optional": true, "required": true}
	admissionWebPolicies       = map[string]bool{"disabled": true, "explicit_only": true, "enabled": true}
	admissionApprovalModes     = map[string]bool{"on-request": true, "read-only": true, "auto-edit": true, "full-auto": true, "smart": true}
	admissionExecutionProfiles = map[string]bool{"": true, "watch-finalization": true}
	admissionRecoveryModes     = map[string]bool{"": true, RunRecoveryModeContinue: true, RunRecoveryModeVerifyOnly: true}
	admissionOriginKinds       = map[string]bool{"": true, "cron": true, "watch": true, "approval": true, "recovery": true,
		"resource": true, "provider_wait": true}
	admissionInputKinds = map[string]bool{"": true, AdmissionInputEvent: true, AdmissionInputChannelMessage: true, AdmissionInputQueue: true}
)

var (
	// ErrAdmissionSnapshotUnreadable reports a stored snapshot this binary
	// cannot interpret. It is never read as "no snapshot": falling back to
	// today's reconstruction could widen the authority the work was admitted
	// with.
	ErrAdmissionSnapshotUnreadable = errors.New("admission snapshot is unreadable")
	// ErrAdmissionRunNotFound reports a Run that does not exist for the given
	// tenant, or, for an exact parent, for the given person.
	ErrAdmissionRunNotFound = errors.New("run is not available for this admission read")
	// ErrResumeAdmissionMismatch reports a human-wait resume row supplied with
	// a snapshot other than the parked Run's own.
	ErrResumeAdmissionMismatch = errors.New("a resume row carries only the parked run's own admission snapshot")
	// ErrQueuedAdmissionMismatch reports a claimed queue row's Run supplied
	// with a snapshot other than the row's own.
	ErrQueuedAdmissionMismatch = errors.New("a claimed run carries only its queue row's admission snapshot")
)

// parkedRunAdmissionJSON is what a human-wait resume row stores: the parked
// Run's snapshot, copied verbatim inside the answer's transaction. A caller may
// restate it but never supply a different one, so answering a question cannot
// change the authority of the work it resumes. Both sides are compared in
// normalized form, so a restatement matches a snapshot stored before a
// normalization rule changed. An unreadable parked snapshot is copied as well;
// the resume row is then quarantined like any unreadable row.
func parkedRunAdmissionJSON(supplied AdmissionSnapshot, parkedRaw string) (string, error) {
	if supplied.IsZero() {
		return parkedRaw, nil
	}
	normalized, _, err := encodeAdmissionSnapshot(supplied)
	if err != nil {
		return "", err
	}
	parked, err := decodeAdmissionSnapshot(parkedRaw)
	if err != nil || parked.normalized() != normalized {
		return "", ErrResumeAdmissionMismatch
	}
	return parkedRaw, nil
}

// queuedRunAdmissionJSON is what the Run a claimed queue row starts stores:
// the row's own snapshot, verbatim, so the claim cannot change the authority
// the work was accepted with. A caller may restate it. A row with no snapshot
// (accepted before schema v26, or deferred steering) takes the caller's
// derivation. An unreadable row never starts a Run.
func queuedRunAdmissionJSON(supplied AdmissionSnapshot, queuedRaw string) (string, error) {
	if queuedRaw == "" {
		_, raw, err := encodeAdmissionSnapshot(supplied)
		if err != nil {
			return "", fmt.Errorf("encode run admission snapshot: %w", err)
		}
		return raw, nil
	}
	queued, err := decodeAdmissionSnapshot(queuedRaw)
	if err != nil {
		return "", err
	}
	if supplied.IsZero() {
		return queuedRaw, nil
	}
	normalized, _, err := encodeAdmissionSnapshot(supplied)
	if err != nil || normalized != queued.normalized() {
		return "", ErrQueuedAdmissionMismatch
	}
	return queuedRaw, nil
}

// setStoredAdmission sets a row's snapshot fields from its stored form.
func (q *QueuedTask) setStoredAdmission(raw string) {
	admission, err := decodeAdmissionSnapshot(raw)
	if err != nil {
		q.Admission, q.AdmissionError = AdmissionSnapshot{}, &QueuedAdmissionError{QueueID: q.ID, Err: err}
		return
	}
	q.Admission, q.AdmissionError = admission, nil
}

// AdmissionSnapshot is the authority a Run or queued request was admitted
// with. A continuation of parked work rebuilds that authority from this durable
// fact instead of re-classifying daemon scheduling text. Fields are primitive:
// the gateway maps them to and from its own strategy and tool types.
//
// The zero value means "no snapshot". Rows admitted before schema v26 carry
// none; a continuation row without one continues its exact parent, whose
// snapshot ExactParentAdmissionSnapshot reads.
type AdmissionSnapshot struct {
	Version int `json:"version"`
	// Strategy is the tool-surface identity the task strategy is rebuilt from.
	// Prompt framing derived from the current environment, such as the
	// sandbox note and the configured action budget, is recomputed.
	Strategy AdmissionStrategy `json:"strategy,omitzero"`
	// RecoveryMode is set for a daemon-owned recovery child: "continue" or
	// "verify_only".
	RecoveryMode string `json:"recovery_mode,omitempty"`
	// ExecutionProfile is an internal execution contract for durable system
	// work, such as watcher finalization.
	ExecutionProfile string `json:"execution_profile,omitempty"`
	// WatchID is the durable watcher whose result this work continues.
	WatchID string `json:"watch_id,omitempty"`
	// ApprovalMode is the mode resolved once at admission. Later mode changes
	// apply only to newly admitted work.
	ApprovalMode string `json:"approval_mode,omitempty"`
	// Origin is who initiated the work. Empty is the person's own turn.
	Origin AdmissionOrigin `json:"origin,omitzero"`
	// StandingGrantsAuthorizedAt (unix seconds) bounds which remembered
	// approval classes scheduled work may use. Zero means none.
	StandingGrantsAuthorizedAt int64 `json:"standing_grants_authorized_at,omitempty"`
	// AdmittedAt (unix seconds) is when the work was first admitted: the
	// person's own turn or the schedule's fire. Only a new admission sets it,
	// and every continuation copies it verbatim, so it stays the moment a later
	// tightening of the person's approval mode is measured against. Zero means
	// unknown.
	AdmittedAt int64 `json:"admitted_at,omitempty"`
	// Input references the person's original request, never the daemon's
	// scheduling text.
	Input AdmissionInput `json:"input,omitzero"`
}

// AdmissionStrategy carries the strategy values the gateway sets from the
// request (text classification, web opt-in, intent, recovery mode), which a
// continuation would otherwise re-derive from its scheduling text. The hidden
// tool set follows from the plan and web policies.
type AdmissionStrategy struct {
	Class            string `json:"class,omitempty"`
	ToolMode         string `json:"tool_mode,omitempty"`
	PlanPolicy       string `json:"plan_policy,omitempty"`
	WebPolicy        string `json:"web_policy,omitempty"`
	ChannelMode      string `json:"channel_mode,omitempty"`
	VerificationOnly bool   `json:"verification_only,omitempty"`
}

// AdmissionOrigin names a daemon initiator and its source, for example the
// cron job whose schedule a person authorized.
type AdmissionOrigin struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

// AdmissionInput references the durable record holding the person's original
// words, plus a bounded summary for display and the work spine.
type AdmissionInput struct {
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// IsZero reports that no snapshot was recorded.
func (a AdmissionSnapshot) IsZero() bool { return a == AdmissionSnapshot{} }

// normalized trims every string and drops bytes that are not UTF-8: JSON
// would store each as a three-byte replacement character, so the bound
// checked on write would not be the value read back.
func (a AdmissionSnapshot) normalized() AdmissionSnapshot {
	for _, field := range []*string{
		&a.Strategy.Class, &a.Strategy.ToolMode, &a.Strategy.PlanPolicy, &a.Strategy.WebPolicy,
		&a.Strategy.ChannelMode, &a.RecoveryMode, &a.ExecutionProfile, &a.WatchID, &a.ApprovalMode,
		&a.Origin.Kind, &a.Origin.ID, &a.Input.Kind, &a.Input.ID, &a.Input.Summary,
	} {
		*field = strings.TrimSpace(strings.ToValidUTF8(*field, ""))
	}
	if utf8.RuneCountInString(a.Input.Summary) > admissionSummaryMaxRunes {
		// The cut can end on whitespace; trim again so normalizing is
		// idempotent and a restated snapshot equals the stored one.
		a.Input.Summary = strings.TrimSpace(string([]rune(a.Input.Summary)[:admissionSummaryMaxRunes]))
	}
	return a
}

// checkShape is the read rule: the v1 version and the minimum every recorded
// admission carries, its strategy identity and its frozen approval mode.
// Everything else a reader maps conservatively.
func (a AdmissionSnapshot) checkShape() error {
	if a.Version != AdmissionSnapshotVersion {
		return fmt.Errorf("unsupported admission snapshot version %d", a.Version)
	}
	if a.Strategy.Class == "" || a.Strategy.ToolMode == "" || a.Strategy.PlanPolicy == "" ||
		a.Strategy.WebPolicy == "" || a.Strategy.ChannelMode == "" || a.ApprovalMode == "" {
		return fmt.Errorf("admission snapshot lacks its strategy or approval mode")
	}
	return nil
}

// validate is the write rule: the shape, every value from its known set, the
// references complete, and every string bounded. It may tighten later without
// making a stored row unreadable, because reads check only the shape.
func (a AdmissionSnapshot) validate() error {
	if err := a.checkShape(); err != nil {
		return err
	}
	for _, field := range []struct {
		name, value string
		known       map[string]bool
	}{
		{"task class", a.Strategy.Class, admissionTaskClasses},
		{"tool mode", a.Strategy.ToolMode, admissionToolModes},
		{"plan policy", a.Strategy.PlanPolicy, admissionPlanPolicies},
		{"web policy", a.Strategy.WebPolicy, admissionWebPolicies},
		{"approval mode", a.ApprovalMode, admissionApprovalModes},
		{"execution profile", a.ExecutionProfile, admissionExecutionProfiles},
		{"recovery mode", a.RecoveryMode, admissionRecoveryModes},
		{"origin kind", a.Origin.Kind, admissionOriginKinds},
		{"input kind", a.Input.Kind, admissionInputKinds},
	} {
		if !field.known[field.value] {
			return fmt.Errorf("unsupported admission %s %q", field.name, field.value)
		}
	}
	if (a.Input.Kind == "") != (a.Input.ID == "") {
		return fmt.Errorf("admission input reference needs both its record kind and id")
	}
	if a.Origin.Kind == "" && a.Origin.ID != "" {
		return fmt.Errorf("admission origin source has no kind")
	}
	if a.StandingGrantsAuthorizedAt < 0 {
		return fmt.Errorf("admission standing-grant cutoff is negative")
	}
	if a.AdmittedAt < 0 {
		return fmt.Errorf("admission time is negative")
	}
	for _, field := range []string{a.Strategy.ChannelMode, a.WatchID, a.Origin.ID, a.Input.ID} {
		if len(field) > admissionIdentifierMaxBytes {
			return fmt.Errorf("admission snapshot value exceeds %d bytes", admissionIdentifierMaxBytes)
		}
	}
	return nil
}

// encodeAdmissionSnapshot returns the normalized snapshot and its stored form:
// empty for no snapshot, otherwise validated JSON that reads back exactly as
// written, so the store never keeps a row it cannot read.
func encodeAdmissionSnapshot(a AdmissionSnapshot) (AdmissionSnapshot, string, error) {
	if a.IsZero() {
		return a, "", nil
	}
	a = a.normalized()
	if err := a.validate(); err != nil {
		return AdmissionSnapshot{}, "", err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return AdmissionSnapshot{}, "", err
	}
	if read, err := decodeAdmissionSnapshot(string(data)); err != nil || read != a {
		return AdmissionSnapshot{}, "", fmt.Errorf("admission snapshot does not read back as written: %v", err)
	}
	return a, string(data), nil
}

// decodeAdmissionSnapshot reads the stored form strictly: one JSON object, no
// unknown field, and the v1 shape. Anything else is unreadable, never absent.
func decodeAdmissionSnapshot(raw string) (AdmissionSnapshot, error) {
	if raw == "" {
		return AdmissionSnapshot{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var snapshot AdmissionSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return AdmissionSnapshot{}, fmt.Errorf("%w: %v", ErrAdmissionSnapshotUnreadable, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return AdmissionSnapshot{}, fmt.Errorf("%w: trailing data", ErrAdmissionSnapshotUnreadable)
	}
	if err := snapshot.checkShape(); err != nil {
		return AdmissionSnapshot{}, fmt.Errorf("%w: %v", ErrAdmissionSnapshotUnreadable, err)
	}
	return snapshot, nil
}

// RunAdmissionSnapshot returns the snapshot frozen when runID was admitted. A
// zero snapshot means the Run carries none and keeps today's reconstruction.
func (s *Store) RunAdmissionSnapshot(ctx context.Context, tenantID, runID string) (AdmissionSnapshot, error) {
	return s.readRunAdmission(ctx, `SELECT admission_json FROM runs WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(runID))
}

// ExactParentAdmissionSnapshot returns the snapshot of the exact parent Run a
// continuation names through a structured edge (a queue row's reply edge, an
// approval or clarification's Run, or a resume edge). Only a Run of the same
// person answers: authority never crosses persons. The parent's snapshot is
// immutable, so reading it before the child's claim cannot race a change.
func (s *Store) ExactParentAdmissionSnapshot(ctx context.Context, tenantID, personID, parentRunID string) (AdmissionSnapshot, error) {
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return AdmissionSnapshot{}, ErrAdmissionRunNotFound
	}
	return s.readRunAdmission(ctx, `SELECT admission_json FROM runs WHERE tenant_id = ? AND id = ? AND person_id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(parentRunID), personID)
}

func (s *Store) readRunAdmission(ctx context.Context, query string, args ...any) (AdmissionSnapshot, error) {
	if s == nil || s.db == nil {
		return AdmissionSnapshot{}, fmt.Errorf("control store is unavailable")
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AdmissionSnapshot{}, ErrAdmissionRunNotFound
		}
		return AdmissionSnapshot{}, err
	}
	return decodeAdmissionSnapshot(raw)
}
