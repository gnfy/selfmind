CREATE TABLE tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'personal',
	created_at INTEGER NOT NULL
);
CREATE TABLE persons (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	display_name TEXT,
	default_workspace_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE accounts (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT NOT NULL,
	display_name TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, last_seen_at INTEGER,
	UNIQUE(tenant_id, platform, platform_user_id)
);
CREATE TABLE workspaces (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	owner_person_id TEXT NOT NULL,
	name TEXT NOT NULL,
	repo_url TEXT,
	local_path TEXT NOT NULL,
	default_branch TEXT,
	allowed_roots_json TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	trust_level TEXT NOT NULL DEFAULT 'untrusted',
	trust_source TEXT,
	trusted_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, owner_person_id, local_path)
);
CREATE TABLE current_workspace (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id)
);
CREATE TABLE workspace_knowledge_sections (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	file_path TEXT NOT NULL,
	file_name TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	file_mtime INTEGER NOT NULL DEFAULT 0,
	section_index INTEGER NOT NULL,
	title TEXT NOT NULL,
	excerpt TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, file_path, section_index)
);
CREATE TABLE execution_leases (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL UNIQUE,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	environment_profile TEXT NOT NULL,
	credential_refs_json TEXT NOT NULL DEFAULT '[]',
	principal_fingerprint TEXT,
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	environment_snapshot_id TEXT NOT NULL DEFAULT '',
	environment_generation INTEGER NOT NULL DEFAULT 0,
	environment_fingerprint TEXT NOT NULL DEFAULT '',
	credential_source_hash TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE execution_capability_grants (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	capability TEXT NOT NULL,
	resource_fingerprint TEXT NOT NULL DEFAULT '',
	granted_by TEXT,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, capability, resource_fingerprint)
);
CREATE TABLE tasks (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	title TEXT NOT NULL,
	status TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'work',
	visibility TEXT NOT NULL DEFAULT 'visible',
	pinned INTEGER NOT NULL DEFAULT 0,
	current_summary TEXT,
	next_steps_json TEXT,
	blocked_reason TEXT,
	active_run_id TEXT,
	last_channel TEXT,
	archived_at INTEGER,
	last_activity_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE current_task (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id)
);
CREATE TABLE task_runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	channel TEXT NOT NULL,
	input_summary TEXT,
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	heartbeat_at INTEGER,
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	last_error TEXT,
	resumed_by_run_id TEXT NOT NULL DEFAULT '',
	work_key TEXT NOT NULL DEFAULT ''
, execution_roots_json TEXT NOT NULL DEFAULT '[]', parent_run_id TEXT NOT NULL DEFAULT '', recovery_contract_version INTEGER NOT NULL DEFAULT 0);
CREATE TABLE task_references (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	class TEXT NOT NULL,
	raw_value TEXT NOT NULL,
	normalized_value TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'shadow',
	user_confirmed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, normalized_value, task_id)
);
CREATE TABLE task_reference_evidence (
	id TEXT PRIMARY KEY,
	reference_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	provenance TEXT NOT NULL,
	source_ref TEXT NOT NULL DEFAULT '',
	evidence_hash TEXT NOT NULL,
	observed_at INTEGER NOT NULL,
	UNIQUE(reference_id, run_id, provenance, evidence_hash)
);
CREATE TABLE task_resolution_events (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	input_hash TEXT NOT NULL,
	matched_surface_forms_json TEXT NOT NULL DEFAULT '[]',
	unmatched_salient_tokens_json TEXT NOT NULL DEFAULT '[]',
	candidates_json TEXT NOT NULL DEFAULT '[]',
	selected_task_id TEXT NOT NULL DEFAULT '',
	final_task_id TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT 'unverified',
	attach_policy_json TEXT NOT NULL DEFAULT '{}',
	analyzer_evaluated INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE task_blockers (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	origin_run_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'open',
	detail_json TEXT NOT NULL DEFAULT '{}',
	resolved_by_run_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	resolved_at INTEGER,
	UNIQUE(origin_run_id, kind)
);
CREATE TABLE task_events (
	id TEXT PRIMARY KEY,
	cursor INTEGER,
	task_id TEXT NOT NULL,
	run_id TEXT,
	type TEXT NOT NULL,
	visibility TEXT NOT NULL DEFAULT 'task',
	channel TEXT,
	payload_json TEXT,
	idempotency_key TEXT,
	created_at INTEGER NOT NULL
);
CREATE TABLE event_sequence (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	next_cursor INTEGER NOT NULL
);
CREATE TABLE channel_messages (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT,
	channel TEXT NOT NULL,
	task_id TEXT,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE task_handoffs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	summary TEXT NOT NULL,
	done_items_json TEXT,
	next_steps_json TEXT,
	changed_files_json TEXT,
	test_status TEXT,
	risks_json TEXT,
	created_at INTEGER NOT NULL
, run_id TEXT NOT NULL DEFAULT '');
CREATE TABLE task_artifacts (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	run_id TEXT,
	kind TEXT NOT NULL,
	name TEXT,
	uri TEXT NOT NULL,
	mime_type TEXT,
	metadata_json TEXT,
	created_at INTEGER NOT NULL
);
CREATE TABLE approval_requests (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	action_type TEXT NOT NULL,
	payload_json TEXT,
	status TEXT NOT NULL,
	requested_channel TEXT,
	approved_channel TEXT,
	decision_scope TEXT,
	decision_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
, decision_grant_key TEXT, decision_note TEXT, decision_recorded_at INTEGER, waiter_state TEXT NOT NULL DEFAULT 'live', parked_at INTEGER, park_reason TEXT, decision_claimed_at INTEGER, claimed_by_run_id TEXT, resume_queue_id TEXT, authorization_fingerprint TEXT, authorization_state TEXT, authorization_expires_at INTEGER, notified_at INTEGER);
CREATE TABLE approval_grants (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	scope_kind TEXT NOT NULL,
	scope_id TEXT NOT NULL,
	pattern_key TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE(tenant_id, person_id, scope_kind, scope_id, pattern_key)
);
CREATE TABLE clarify_requests (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	question TEXT NOT NULL,
	options_json TEXT,
	status TEXT NOT NULL,
	answer TEXT,
	channel TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
, notified_at INTEGER);
CREATE TABLE notifications (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	task_id TEXT,
	event_id TEXT,
	status TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE outbound_messages (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT,
	channel TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	content TEXT NOT NULL,
	kind TEXT,
	approval_id TEXT,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 3,
	next_attempt_at INTEGER NOT NULL,
	last_error TEXT,
	part_index INTEGER NOT NULL DEFAULT 1,
	part_total INTEGER NOT NULL DEFAULT 1,
	idempotency_key TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	delivered_at INTEGER
, catchup_at INTEGER);
CREATE TABLE person_settings (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id, key)
);
CREATE TABLE task_queue (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT,
	content TEXT NOT NULL,
	approval_mode TEXT,
	workspace_id TEXT,
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT NOT NULL DEFAULT '',
	class TEXT NOT NULL DEFAULT 'foreground',
	priority INTEGER NOT NULL DEFAULT 100,
	not_before INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'queued',
	restarts INTEGER NOT NULL DEFAULT 0,
	claim_token TEXT NOT NULL DEFAULT '',
	lease_until INTEGER NOT NULL DEFAULT 0,
	attempt_generation INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
, execution_roots_json TEXT NOT NULL DEFAULT '[]', reply_to_run_id TEXT NOT NULL DEFAULT '', approval_id TEXT NOT NULL DEFAULT '', clarify_id TEXT NOT NULL DEFAULT '');
CREATE TABLE effect_receipts (
	effect_key TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	delivery_enqueued INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, effect_key)
);
CREATE TABLE inbound_dedup (
	platform TEXT NOT NULL,
	message_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (platform, message_id)
);
CREATE TABLE maintenance_jobs (
	run_id TEXT NOT NULL,
	analyzer_version INTEGER NOT NULL DEFAULT 1,
	tenant_id TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_retry_at INTEGER NOT NULL DEFAULT 0,
	result_hash TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '',
	proposal_json TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, blocked_route_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, analyzer_version)
);
CREATE TABLE maintenance_attempts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	analyzer_version INTEGER NOT NULL DEFAULT 1,
	tenant_id TEXT NOT NULL,
	attempt INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	route_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE provider_route_health (
	tenant_id TEXT NOT NULL,
	route_id TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'closed',
	failure_class TEXT NOT NULL DEFAULT '',
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	opened_at INTEGER NOT NULL DEFAULT 0,
	next_probe_at INTEGER NOT NULL DEFAULT 0,
	probe_lease_until INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	last_request_id TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, route_id)
);
CREATE TABLE maintenance_provider_calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	route_id TEXT NOT NULL DEFAULT '',
	candidate_index INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	trigger_class TEXT NOT NULL DEFAULT '',
	finish_reason TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_miss_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_usage_reported INTEGER NOT NULL DEFAULT 0,
	batch_size INTEGER NOT NULL DEFAULT 1,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE approval_triage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	tool_name TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	risk_level TEXT NOT NULL DEFAULT '',
	user_authorization TEXT NOT NULL DEFAULT '',
	grant_key TEXT NOT NULL DEFAULT '',
	provider_route TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	policy_version TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE gateway_runtime_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	instance_id TEXT NOT NULL,
	event_type TEXT NOT NULL,
	payload_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	UNIQUE(instance_id, event_type)
);
CREATE TABLE external_watches (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	task_id TEXT NOT NULL,
	run_id TEXT,
	channel TEXT,
	description TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL,
	command TEXT NOT NULL,
	success_pattern TEXT NOT NULL,
	failure_pattern TEXT NOT NULL DEFAULT '',
	spec_version INTEGER NOT NULL DEFAULT 1,
	target_pattern TEXT NOT NULL DEFAULT '',
	terminal_success_pattern TEXT NOT NULL DEFAULT '',
	terminal_failure_pattern TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	checker_status TEXT NOT NULL DEFAULT '',
	operation_status TEXT NOT NULL DEFAULT 'pending',
	verification_status TEXT NOT NULL DEFAULT 'not_required',
	interval_seconds INTEGER NOT NULL DEFAULT 30,
	current_interval_seconds INTEGER NOT NULL DEFAULT 0,
	command_timeout_seconds INTEGER NOT NULL DEFAULT 30,
	timeout_at INTEGER NOT NULL,
	next_check_at INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	extensions INTEGER NOT NULL DEFAULT 0,
	finalized INTEGER NOT NULL DEFAULT 0,
	verdict_revision INTEGER NOT NULL DEFAULT 1,
	notified INTEGER NOT NULL DEFAULT 0,
	last_output TEXT NOT NULL DEFAULT '',
	last_output_hash TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	execution_binding_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	finished_at INTEGER
, failure_class TEXT NOT NULL DEFAULT '', check_signature TEXT NOT NULL DEFAULT '', consecutive_failures INTEGER NOT NULL DEFAULT 0, environment_snapshot_id TEXT NOT NULL DEFAULT '', environment_generation INTEGER NOT NULL DEFAULT 0, principal_fingerprint TEXT NOT NULL DEFAULT '', environment_fingerprint TEXT NOT NULL DEFAULT '', credential_source_hash TEXT NOT NULL DEFAULT '', observation_adapter TEXT NOT NULL DEFAULT '', preflight_receipt_json TEXT NOT NULL DEFAULT '{}', wait_group_id TEXT NOT NULL DEFAULT '');
CREATE TABLE steering_mailbox (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	platform TEXT NOT NULL DEFAULT '',
	platform_user_id TEXT NOT NULL DEFAULT '',
	workspace_id TEXT NOT NULL DEFAULT '',
	approval_mode TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'accepted',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE loop_checkpoints (
	run_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	iteration INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	snapshot_json BLOB NOT NULL,
	updated_at INTEGER NOT NULL
, contract_version INTEGER NOT NULL DEFAULT 0, recovery_json TEXT NOT NULL DEFAULT '{}');
CREATE TABLE tool_ledger (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	tool_call_id TEXT NOT NULL,
	tool_name TEXT NOT NULL,
	args_hash TEXT NOT NULL DEFAULT '',
	retry_class TEXT NOT NULL DEFAULT 'side_effect',
	status TEXT NOT NULL DEFAULT 'planned',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, effect_id TEXT NOT NULL DEFAULT '', plan_version INTEGER NOT NULL DEFAULT 0, plan_step_id TEXT NOT NULL DEFAULT '', strategy TEXT NOT NULL DEFAULT '', effect_class TEXT NOT NULL DEFAULT '', environment_generation INTEGER NOT NULL DEFAULT 0, result_ref TEXT NOT NULL DEFAULT '', verification_state TEXT NOT NULL DEFAULT '',
	UNIQUE(run_id, tool_call_id)
);
CREATE TABLE workflow_profiles (
	run_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	skill_versions_json TEXT NOT NULL DEFAULT '{}',
	plan_hash TEXT NOT NULL DEFAULT '',
	tool_sequence_json TEXT NOT NULL DEFAULT '[]',
	tool_calls INTEGER NOT NULL DEFAULT 0,
	tool_failures INTEGER NOT NULL DEFAULT 0,
	provider_calls INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	billed_input_tokens INTEGER NOT NULL DEFAULT 0,
	outcome_status TEXT NOT NULL DEFAULT '',
	verification_state TEXT NOT NULL DEFAULT '',
	read_only INTEGER NOT NULL DEFAULT 0,
	applied_candidate_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE evolution_candidates (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	last_task_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	kind TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'candidate',
	contract_json TEXT NOT NULL DEFAULT '{}',
	repair_json TEXT NOT NULL DEFAULT '{}',
	observation_count INTEGER NOT NULL DEFAULT 0,
	shadow_runs INTEGER NOT NULL DEFAULT 0,
	shadow_matches INTEGER NOT NULL DEFAULT 0,
	fallback_count INTEGER NOT NULL DEFAULT 0,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_failure TEXT NOT NULL DEFAULT '',
	enabled_at INTEGER,
	last_applied_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, workflow_signature, kind)
);
CREATE TABLE skill_versions (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	parent_version_hash TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	content_ref TEXT NOT NULL DEFAULT '',
	content_body TEXT NOT NULL DEFAULT '',
	source_observation_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_set_hash TEXT NOT NULL DEFAULT '',
	evidence_json TEXT NOT NULL DEFAULT '{}',
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	promoted_at INTEGER, package_hash TEXT NOT NULL DEFAULT '', resource_manifest_json TEXT NOT NULL DEFAULT '[]', dependency_fingerprint TEXT NOT NULL DEFAULT '', verification_environment_fingerprint TEXT NOT NULL DEFAULT '', last_verified_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(control_tenant_id, skill_key, version_hash)
);
CREATE TABLE run_work_units (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	primary_task_id TEXT NOT NULL,
	related_task_id TEXT NOT NULL DEFAULT '',
	goal_digest TEXT NOT NULL DEFAULT '',
	plan_status TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	outcome_summary TEXT NOT NULL DEFAULT '',
		verification_state TEXT NOT NULL DEFAULT '',
		verification_refs_json TEXT NOT NULL DEFAULT '[]',
		started_at INTEGER,
		created_at INTEGER NOT NULL,
		finished_at INTEGER,
		started_cursor INTEGER NOT NULL DEFAULT 0,
		finished_cursor INTEGER NOT NULL DEFAULT 0,
		UNIQUE(run_id, sequence)
);
CREATE TABLE run_skill_activations (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	work_unit_id TEXT NOT NULL,
	execution_lane TEXT NOT NULL DEFAULT 'main',
	primary_task_id TEXT NOT NULL,
	related_task_id TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	activation_source TEXT NOT NULL,
	attachment_mode TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	fallback_reason TEXT NOT NULL DEFAULT '',
	selected_at INTEGER NOT NULL,
	finished_at INTEGER, package_hash TEXT NOT NULL DEFAULT '', delivery_contract_version INTEGER NOT NULL DEFAULT 0, delivery_mode TEXT NOT NULL DEFAULT '', delivered_main TEXT NOT NULL DEFAULT '', delivered_main_hash TEXT NOT NULL DEFAULT '', delivered_main_bytes INTEGER NOT NULL DEFAULT 0, resource_manifest_json TEXT NOT NULL DEFAULT '[]',
	UNIQUE(run_id, sequence)
);
CREATE TABLE task_skill_bindings (
	identity_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	state TEXT NOT NULL,
	binding_source TEXT NOT NULL,
	bound_from_run_id TEXT NOT NULL DEFAULT '',
	last_resolved_version_hash TEXT NOT NULL DEFAULT '',
	suspended_reason TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(identity_tenant_id, person_id, task_id)
);
CREATE TABLE skill_failure_guards (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	failure_signature TEXT NOT NULL,
	failed_step_id TEXT NOT NULL DEFAULT '',
	error_category TEXT NOT NULL DEFAULT '',
	normalized_input_shape TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'active',
	source_run_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	occurrence_count INTEGER NOT NULL DEFAULT 1,
	last_seen_at INTEGER NOT NULL DEFAULT 0, environment_fingerprint TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(control_tenant_id, skill_key, version_hash, failure_signature)
);
CREATE TABLE workflow_observations (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL UNIQUE,
	related_task_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	goal_digest TEXT NOT NULL DEFAULT '',
	environment_fingerprint TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL DEFAULT '',
	version_hash TEXT NOT NULL DEFAULT '',
	activation_state TEXT NOT NULL DEFAULT '',
	outcome_status TEXT NOT NULL,
	verification_state TEXT NOT NULL DEFAULT '',
	tool_sequence_json TEXT NOT NULL DEFAULT '[]',
	tool_failures INTEGER NOT NULL DEFAULT 0,
	provider_calls INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	user_corrected INTEGER NOT NULL DEFAULT 0,
	evidence_role TEXT NOT NULL DEFAULT 'audit',
	created_at INTEGER NOT NULL
);
CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	);
CREATE TABLE memory_governance_schedule (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	last_attempt_at INTEGER NOT NULL DEFAULT 0,
	last_success_at INTEGER NOT NULL DEFAULT 0,
	next_due_at INTEGER NOT NULL DEFAULT 0,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_outcome TEXT NOT NULL DEFAULT '',
	last_deferred_reason TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, person_id)
);
CREATE TABLE skill_candidate_evidence_snapshots (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	evidence_set_hash TEXT NOT NULL,
	observation_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	PRIMARY KEY(control_tenant_id, skill_key, version_hash, evidence_set_hash)
);
CREATE TABLE skill_candidate_refs (
	candidate_ref TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	package_hash TEXT NOT NULL,
	description_hash TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'issued',
	drift_count INTEGER NOT NULL DEFAULT 0,
	issued_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE(identity_tenant_id, run_id, work_unit_id, skill_key, package_hash, description_hash)
);
CREATE TABLE skill_package_resources (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	package_hash TEXT NOT NULL,
	resource_path TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	content_body TEXT NOT NULL,
	content_bytes INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(control_tenant_id, skill_key, package_hash, resource_path)
);
CREATE TABLE skill_attributions (
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	package_path TEXT NOT NULL,
	package_name TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL DEFAULT '',
	provenance TEXT NOT NULL DEFAULT '',
	tool_name TEXT NOT NULL DEFAULT '',
	observed_at INTEGER NOT NULL,
	PRIMARY KEY (control_tenant_id, run_id, work_unit_id, package_path)
);
CREATE TABLE pending_turn_choices (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	resolution_id TEXT NOT NULL DEFAULT '',
	request_json TEXT NOT NULL,
	options_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'pending',
	chosen_key TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	claimed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE turn_resolution_events (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	input_hash TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT '',
	decision TEXT NOT NULL DEFAULT '',
	certainty TEXT NOT NULL DEFAULT '',
	target_task_id TEXT NOT NULL DEFAULT '',
	target_run_id TEXT NOT NULL DEFAULT '',
	candidate_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_json TEXT NOT NULL DEFAULT '[]',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	correction_of TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE run_plan_versions (
	run_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	explanation TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, version)
);
CREATE TABLE run_plan_steps (
	run_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	plan_version INTEGER NOT NULL,
	step_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	step_text TEXT NOT NULL,
	status TEXT NOT NULL,
	success_criteria TEXT NOT NULL DEFAULT '',
	verification_required INTEGER NOT NULL DEFAULT 0,
	related_task_id TEXT NOT NULL DEFAULT '',
	work_unit_id TEXT NOT NULL DEFAULT '',
	work_unit_boundary INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, plan_version, step_id),
	UNIQUE (run_id, plan_version, sequence)
);
CREATE TABLE external_watch_groups (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	group_key TEXT NOT NULL,
	mode TEXT NOT NULL,
	expected_count INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	winner_watch_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE (tenant_id, run_id, group_key)
);
CREATE TABLE run_delivery_overrides (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	source_steering_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, run_id),
	UNIQUE (tenant_id, source_steering_id)
);
CREATE INDEX idx_workspace_knowledge_scope
	ON workspace_knowledge_sections(tenant_id, person_id, workspace_id, updated_at);
CREATE INDEX idx_execution_leases_person
	ON execution_leases(tenant_id, person_id, created_at);
CREATE INDEX idx_execution_capability_active
	ON execution_capability_grants(tenant_id, person_id, workspace_id, capability, expires_at);
CREATE INDEX idx_tasks_owner ON tasks(tenant_id, person_id, updated_at);
CREATE INDEX idx_task_runs_task_started ON task_runs(tenant_id, task_id, started_at);
CREATE INDEX idx_task_runs_person_status ON task_runs(tenant_id, person_id, status, started_at);
CREATE INDEX idx_task_references_owner_value
	ON task_references(tenant_id, person_id, normalized_value, status);
CREATE INDEX idx_task_references_task
	ON task_references(tenant_id, task_id, status, updated_at);
CREATE INDEX idx_task_reference_evidence_ref
	ON task_reference_evidence(reference_id, provenance, observed_at);
CREATE INDEX idx_task_resolution_events_owner
	ON task_resolution_events(tenant_id, person_id, created_at);
CREATE INDEX idx_task_resolution_events_run
	ON task_resolution_events(tenant_id, run_id, created_at);
CREATE UNIQUE INDEX idx_task_resolution_events_run_unique
	ON task_resolution_events(tenant_id, run_id) WHERE run_id != '';
CREATE INDEX idx_task_blockers_open
	ON task_blockers(tenant_id, task_id, status, created_at);
CREATE INDEX idx_task_events_task ON task_events(task_id, created_at);
CREATE INDEX idx_channel_messages_person_channel ON channel_messages(tenant_id, person_id, channel, created_at);
CREATE INDEX idx_task_handoffs_task_created ON task_handoffs(task_id, created_at);
CREATE INDEX idx_task_artifacts_task ON task_artifacts(task_id, created_at);
CREATE INDEX idx_approval_pending ON approval_requests(tenant_id, person_id, status, created_at);
CREATE INDEX idx_approval_grants_lookup ON approval_grants(tenant_id, person_id, pattern_key);
CREATE INDEX idx_clarify_pending ON clarify_requests(tenant_id, person_id, status, created_at);
CREATE INDEX idx_outbound_due ON outbound_messages(status, next_attempt_at);
CREATE UNIQUE INDEX idx_outbound_idempotency ON outbound_messages(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
CREATE INDEX idx_task_queue_person ON task_queue(tenant_id, person_id, status, created_at);
CREATE INDEX idx_effect_receipts_run ON effect_receipts(tenant_id, run_id);
CREATE INDEX idx_inbound_dedup_created ON inbound_dedup(created_at);
CREATE INDEX idx_maintenance_jobs_status ON maintenance_jobs(tenant_id, status, next_retry_at);
CREATE INDEX idx_maintenance_attempts_job
	ON maintenance_attempts(tenant_id, run_id, analyzer_version, created_at);
CREATE INDEX idx_maintenance_attempts_recent
	ON maintenance_attempts(tenant_id, created_at);
CREATE INDEX idx_provider_route_probe
	ON provider_route_health(tenant_id, state, next_probe_at);
CREATE INDEX idx_maintenance_provider_calls_recent
	ON maintenance_provider_calls(tenant_id, created_at);
CREATE INDEX idx_maintenance_provider_calls_route
	ON maintenance_provider_calls(tenant_id, route_id, created_at);
CREATE INDEX idx_approval_triage_events_recent
	ON approval_triage_events(tenant_id, person_id, created_at);
CREATE INDEX idx_gateway_runtime_events_recent
	ON gateway_runtime_events(created_at);
CREATE INDEX idx_steering_mailbox_live
	ON steering_mailbox(tenant_id, status, created_at);
CREATE INDEX idx_steering_mailbox_run
	ON steering_mailbox(run_id, status);
CREATE INDEX idx_loop_checkpoints_task
	ON loop_checkpoints(tenant_id, task_id, updated_at);
CREATE INDEX idx_tool_ledger_uncertain
	ON tool_ledger(tenant_id, run_id, status);
CREATE INDEX idx_workflow_profiles_signature
	ON workflow_profiles(tenant_id, person_id, workspace_id, workflow_signature, created_at);
CREATE INDEX idx_evolution_candidates_active
	ON evolution_candidates(tenant_id, person_id, workspace_id, status, updated_at);
CREATE UNIQUE INDEX idx_skill_versions_one_active
	ON skill_versions(control_tenant_id, skill_key) WHERE state = 'active';
CREATE UNIQUE INDEX idx_skill_versions_candidate_evidence
	ON skill_versions(control_tenant_id, evidence_set_hash)
	WHERE state = 'candidate' AND evidence_set_hash != '';
CREATE INDEX idx_run_work_units_run
	ON run_work_units(identity_tenant_id, run_id, sequence);
CREATE UNIQUE INDEX idx_run_skill_activations_live_lane
	ON run_skill_activations(run_id, work_unit_id, execution_lane)
	WHERE state IN ('selected', 'active');
CREATE INDEX idx_run_skill_activations_skill
	ON run_skill_activations(control_tenant_id, skill_key, version_hash, selected_at);
CREATE INDEX idx_task_skill_bindings_skill
	ON task_skill_bindings(control_tenant_id, skill_key, state, updated_at);
CREATE INDEX idx_workflow_observations_cohort
	ON workflow_observations(identity_tenant_id, person_id, workspace_id, workflow_signature, created_at);
CREATE INDEX idx_task_events_run_created
	ON task_events(run_id, created_at);
CREATE INDEX idx_external_watches_due
	ON external_watches(status, next_check_at);
CREATE INDEX idx_external_watches_owner
	ON external_watches(tenant_id, person_id, status, updated_at);
CREATE INDEX idx_task_runs_work_key
		ON task_runs(tenant_id, task_id, work_key, status, started_at);
CREATE UNIQUE INDEX idx_task_events_cursor ON task_events(cursor);
CREATE UNIQUE INDEX idx_task_events_idempotency
			ON task_events(idempotency_key)
			WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
CREATE INDEX idx_tasks_governance
			ON tasks(tenant_id, person_id, visibility, status, updated_at);
CREATE INDEX idx_tasks_retention
			ON tasks(visibility, pinned, status, last_activity_at);
CREATE UNIQUE INDEX idx_tasks_inbox_unique
			ON tasks(tenant_id, person_id, COALESCE(workspace_id, ''))
			WHERE kind = 'inbox';
CREATE INDEX idx_task_queue_schedule
			ON task_queue(status, not_before, priority DESC, created_at);
CREATE INDEX idx_task_queue_claims
			ON task_queue(status, lease_until);
CREATE INDEX idx_approval_resume_authorization
			ON approval_requests(tenant_id, person_id, task_id, authorization_fingerprint, authorization_state);
CREATE INDEX idx_maintenance_provider_calls_person
			ON maintenance_provider_calls(tenant_id, person_id, created_at);
CREATE INDEX idx_memory_governance_due
	ON memory_governance_schedule(next_due_at, tenant_id, person_id);
CREATE INDEX idx_skill_candidate_evidence_set
	ON skill_candidate_evidence_snapshots(control_tenant_id, evidence_set_hash);
CREATE INDEX idx_skill_candidate_refs_work_unit
	ON skill_candidate_refs(identity_tenant_id, run_id, work_unit_id, issued_at);
CREATE INDEX idx_skill_versions_review_due
				ON skill_versions(control_tenant_id, state, last_verified_at);
CREATE INDEX idx_skill_attributions_skill
	ON skill_attributions(control_tenant_id, skill_key, observed_at);
CREATE UNIQUE INDEX idx_task_runs_parent_once
	ON task_runs(tenant_id, parent_run_id) WHERE parent_run_id <> '';
CREATE INDEX idx_task_handoffs_run
	ON task_handoffs(run_id) WHERE run_id <> '';
CREATE INDEX idx_pending_turn_choices_person
	ON pending_turn_choices(tenant_id, person_id, status, expires_at, created_at);
CREATE INDEX idx_turn_resolution_events_person
	ON turn_resolution_events(tenant_id, person_id, created_at);
CREATE INDEX idx_turn_resolution_events_target
	ON turn_resolution_events(tenant_id, target_run_id, created_at)
	WHERE target_run_id <> '';
CREATE INDEX idx_run_plan_versions_latest
	ON run_plan_versions(tenant_id, run_id, version DESC);
CREATE INDEX idx_run_plan_steps_latest
	ON run_plan_steps(tenant_id, run_id, plan_version DESC, sequence);
CREATE INDEX idx_tool_ledger_effect
	ON tool_ledger(tenant_id, effect_id) WHERE effect_id <> '';
CREATE INDEX idx_tool_ledger_plan_step
	ON tool_ledger(tenant_id, run_id, plan_step_id) WHERE plan_step_id <> '';
CREATE INDEX idx_external_watch_groups_pending
	ON external_watch_groups(tenant_id, person_id, status, created_at);
CREATE UNIQUE INDEX idx_task_queue_idempotency ON task_queue(tenant_id, idempotency_key)
			WHERE idempotency_key != '';
CREATE INDEX idx_run_delivery_overrides_person
	ON run_delivery_overrides(tenant_id, person_id, updated_at);
