-- Unreleased integration branch hardening/concurrency, commit 33b5d601d352,
-- schema v25. Created by that tree's OpenStore and seeded through its store
-- API with synthetic rows (sqlite3 .dump). No operator data.
PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'personal',
	created_at INTEGER NOT NULL
);
INSERT INTO tenants VALUES('default','default','personal',1791248258);
CREATE TABLE persons (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	display_name TEXT,
	default_workspace_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO persons VALUES('person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','default','Fixture Owner',NULL,1791248258,1791248258);
INSERT INTO persons VALUES('person_4ee800ce-809b-40e6-b436-572622e885fe','default','Fixture Other',NULL,1791248258,1791248258);
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
INSERT INTO accounts VALUES('acct_24876408-7438-4dcb-bd00-aff2066e8958','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','fixture-owner','Fixture Owner','active',1791248258,1791248258,NULL);
INSERT INTO accounts VALUES('acct_56cce20e-7e55-4dcb-bbed-be23caf8c8f9','default','person_4ee800ce-809b-40e6-b436-572622e885fe','telegram','1000001','Fixture Other','active',1791248258,1791248258,NULL);
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
CREATE TABLE IF NOT EXISTS "runs" (
	id TEXT PRIMARY KEY,
	thread_id TEXT NOT NULL,
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
, execution_roots_json TEXT NOT NULL DEFAULT '[]', resumes_run_id TEXT NOT NULL DEFAULT '', recovery_contract_version INTEGER NOT NULL DEFAULT 0, attention_dismissed_at INTEGER NOT NULL DEFAULT 0, attention_dismissed_by TEXT NOT NULL DEFAULT '', execution_class TEXT NOT NULL DEFAULT 'work');
INSERT INTO runs VALUES('run_91f492de-fbcd-4c9d-8de8-abfaf8ea097a','task_d5d3b75d-e9ff-4827-bb71-082db8b77e04','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','summarize the release notes','done',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
INSERT INTO runs VALUES('run_953f977c-0986-4505-aa90-783105dccd80','task_9e0e03c8-941b-4310-8a91-427b38da492d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','migrate the build scripts','interrupted',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
INSERT INTO runs VALUES('run_5372de9a-5f7c-436e-b6a7-c77be6ebf231','task_e863fe02-aeef-442c-b555-568f490498b5','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','deploy the preview build','waiting_user',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
INSERT INTO runs VALUES('run_377602f7-56c1-4b07-8de9-bb34264d55d9','task_e863fe02-aeef-442c-b555-568f490498b5','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','continue','done',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_5372de9a-5f7c-436e-b6a7-c77be6ebf231',3,0,'','work');
INSERT INTO runs VALUES('run_5a30d858-ee03-4bf7-9633-6c089bc5acb2','task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','roll out the canary','waiting_external',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
INSERT INTO runs VALUES('run_614f5acb-d303-4f6f-a399-2d79b6313f5a','task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','telegram','roll out the stable build','waiting_external',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_4dcb9ff9-4700-4c55-a84d-868729942c2e','task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','deploy the preview build to staging','waiting_user',1791248258,1791248258,1791248258,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
INSERT INTO runs VALUES('run_cac9832e-ef27-4522-b009-1c9e716c0a42','task_63b608a1-7476-4e46-a7e3-21777424126d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','rename the release branch','waiting_user',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','task_a34a929d-0c19-4523-94c3-cd6ebb841324','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','ship the release; skip the live check this time','waiting_user',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_a4416bc0-7b6d-47f2-a5aa-8958d916754e','task_66429d3f-36df-4bf4-a02c-8cead0b91596','default','person_4ee800ce-809b-40e6-b436-572622e885fe','','telegram','check the invoice export','blocked',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_433b7209-b7bd-49bf-bf22-71606142ba47','task_5a9a0d07-adfe-42ce-8d29-df617d9f6641','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','publish the package','interrupted',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_3f703ad3-d77b-4d73-bafe-a75ed7de5595','task_d6d90d57-2d3d-4ca5-93df-edd685a8485e','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','publish the docs site','waiting_external',1791248258,1791248258,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_1061220a-6b38-44cf-bab5-a8f3617f3268','task_8e20ac5a-8057-4ed0-ad17-4cea47cafb7f','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','sync two clusters','running',1791248258,NULL,1791248258,0,NULL,'','','null','',3,0,'','work');
INSERT INTO runs VALUES('run_17daba33-888a-4f98-ae34-0e8f3fb4be11','task_d86bf591-ba30-4471-a584-6dd77e998aff','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','cli','rebuild the docs index','running',1791248258,NULL,1791248258,0,NULL,'','','[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','',3,0,'','work');
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
	thread_id TEXT NOT NULL,
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
	thread_id TEXT NOT NULL,
	run_id TEXT,
	type TEXT NOT NULL,
	visibility TEXT NOT NULL DEFAULT 'task',
	channel TEXT,
	payload_json TEXT,
	idempotency_key TEXT,
	created_at INTEGER NOT NULL
);
INSERT INTO task_events VALUES('event_ba5f2bc7-8ac9-4513-8e4f-8f9833505df3',1,'task_d5d3b75d-e9ff-4827-bb71-082db8b77e04','run_91f492de-fbcd-4c9d-8de8-abfaf8ea097a','run.started','task','cli','{"input":"summarize the release notes","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_1099d1cf-708a-408d-9578-d3616561e962',2,'task_9e0e03c8-941b-4310-8a91-427b38da492d','run_953f977c-0986-4505-aa90-783105dccd80','run.started','task','cli','{"input":"migrate the build scripts","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_8803f1d7-ed26-449a-b484-3d2f74e7657d',3,'task_e863fe02-aeef-442c-b555-568f490498b5','run_5372de9a-5f7c-436e-b6a7-c77be6ebf231','run.started','task','cli','{"input":"deploy the preview build","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_5ee121e3-181c-4310-8a38-65cfc28b2bb3',4,'task_e863fe02-aeef-442c-b555-568f490498b5','run_377602f7-56c1-4b07-8de9-bb34264d55d9','run.started','task','cli','{"input":"continue","resumes_run_id":"run_5372de9a-5f7c-436e-b6a7-c77be6ebf231"}',NULL,1791248258);
INSERT INTO task_events VALUES('event_64e83b98-15e3-4b5a-a41f-5d77e476d57a',5,'task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','run_5a30d858-ee03-4bf7-9633-6c089bc5acb2','run.started','task','cli','{"input":"roll out the canary","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_cc1ccd62-0b5e-4527-9c9d-cf0a8c5db42d',6,'task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','run_614f5acb-d303-4f6f-a399-2d79b6313f5a','run.started','task','telegram','{"input":"roll out the stable build","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_1e38ee6e-57f6-4ce2-b4cf-c7ca99886dd5',7,'task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','run_4dcb9ff9-4700-4c55-a84d-868729942c2e','run.started','task','cli','{"input":"deploy the preview build to staging","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_d30c109e-ad05-4cc0-a62d-6029d5ec0f2c',8,'task_63b608a1-7476-4e46-a7e3-21777424126d','run_cac9832e-ef27-4522-b009-1c9e716c0a42','run.started','task','cli','{"input":"rename the release branch","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_5109a679-a7b2-4dc8-a04e-8b1feb9f3399',9,'task_a34a929d-0c19-4523-94c3-cd6ebb841324','run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','run.started','task','cli','{"input":"ship the release; skip the live check this time","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_8921689b-d22b-4ec0-b2e9-7652735cc6d4',10,'task_66429d3f-36df-4bf4-a02c-8cead0b91596','run_a4416bc0-7b6d-47f2-a5aa-8958d916754e','run.started','task','telegram','{"input":"check the invoice export","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_fe2f83a7-14ce-41bc-a53d-fc5fc8b4b3a6',11,'task_5a9a0d07-adfe-42ce-8d29-df617d9f6641','run_433b7209-b7bd-49bf-bf22-71606142ba47','run.started','task','cli','{"input":"publish the package","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_b8d154f9-e229-4fe9-b6bf-d0052b906c6f',12,'task_d6d90d57-2d3d-4ca5-93df-edd685a8485e','run_3f703ad3-d77b-4d73-bafe-a75ed7de5595','run.started','task','cli','{"input":"publish the docs site","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_b3957b39-f60c-4599-af23-095d3a684293',13,'task_8e20ac5a-8057-4ed0-ad17-4cea47cafb7f','run_1061220a-6b38-44cf-bab5-a8f3617f3268','run.started','task','cli','{"input":"sync two clusters","resumes_run_id":""}',NULL,1791248258);
INSERT INTO task_events VALUES('event_04f1786f-dec0-460a-8016-9f1a88de74ef',14,'task_d86bf591-ba30-4471-a584-6dd77e998aff','run_17daba33-888a-4f98-ae34-0e8f3fb4be11','run.started','task','cli','{"input":"rebuild the docs index","resumes_run_id":""}',NULL,1791248258);
CREATE TABLE event_sequence (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	next_cursor INTEGER NOT NULL
);
INSERT INTO event_sequence VALUES(1,14);
CREATE TABLE channel_messages (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT,
	channel TEXT NOT NULL,
	thread_id TEXT,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE task_handoffs (
	id TEXT PRIMARY KEY,
	thread_id TEXT NOT NULL,
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
	thread_id TEXT NOT NULL,
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
	thread_id TEXT,
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
INSERT INTO approval_requests VALUES('apr_3b4c280c-23f2-4d7e-b1d1-a0dcad3b9ed7','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','run_4dcb9ff9-4700-4c55-a84d-868729942c2e','tool_call','{"tool":"terminal","command":"./deploy.sh staging"}','pending','cli','',NULL,NULL,1791248258,1791248258,NULL,NULL,NULL,'parked',1791248258,'approval wait budget elapsed',NULL,NULL,NULL,'resume:v1:fixture-deploy',NULL,NULL,NULL);
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
	thread_id TEXT,
	run_id TEXT,
	question TEXT NOT NULL,
	options_json TEXT,
	status TEXT NOT NULL,
	answer TEXT,
	channel TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
, notified_at INTEGER);
INSERT INTO clarify_requests VALUES('clr_433312ce-512d-4bbb-a4b3-e8a401ebdce8','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','task_63b608a1-7476-4e46-a7e3-21777424126d','run_cac9832e-ef27-4522-b009-1c9e716c0a42','Which branch should be renamed?','[]','pending','','cli',1791248258,1791248258,NULL);
CREATE TABLE notifications (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	thread_id TEXT,
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
	thread_id TEXT,
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
, catchup_at INTEGER, clarify_id TEXT NOT NULL DEFAULT '');
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
	thread_id TEXT NOT NULL DEFAULT '',
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
, execution_roots_json TEXT NOT NULL DEFAULT '[]', reply_to_run_id TEXT NOT NULL DEFAULT '', approval_id TEXT NOT NULL DEFAULT '', clarify_id TEXT NOT NULL DEFAULT '', attachments_json TEXT);
INSERT INTO task_queue VALUES('queue_47590fc7-f63d-4687-a21c-07a7068a2e00','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','fixture-owner','look up today''s release notes','smart','','','','','foreground',100,0,'queued',0,'',0,0,1791248258,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','','','','[{"kind":"image","path":"/srv/fixture/attachments/shot.png","mime_type":"image/png","name":"shot.png","size":42}]');
INSERT INTO task_queue VALUES('queue_dfc7b443-22fe-49dc-87e7-75be0b54ee1d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','Resume the original task after a durable external observation.','','','task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','','external-watch:watch_6e7ec0c3-7add-4467-9175-fd54de87041c:r1','finalization',80,0,'queued',0,'',0,0,1791248258,'null','run_614f5acb-d303-4f6f-a399-2d79b6313f5a','','','[]');
INSERT INTO task_queue VALUES('queue_1c00c403-cfb0-42e6-b80f-9405a3308a08','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','Recover the interrupted run in verification-only mode.','','','task_9e0e03c8-941b-4310-8a91-427b38da492d','','run-recovery:run_953f977c-0986-4505-aa90-783105dccd80:verify_only','recovery',60,0,'queued',0,'',0,0,1791248258,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_953f977c-0986-4505-aa90-783105dccd80','','','[]');
INSERT INTO task_queue VALUES('queue_dd40138b-953b-4567-8c29-e454abee8073','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','Continue this exact work after the provider became available.','','','task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','','provider-wait:run_5a30d858-ee03-4bf7-9633-6c089bc5acb2','finalization',80,1791251859,'queued',0,'',0,0,1791248258,'null','run_5a30d858-ee03-4bf7-9633-6c089bc5acb2','','','[]');
INSERT INTO task_queue VALUES('queue_81bc1eea-9918-4452-840d-fcbacfde1e88','default','person_4ee800ce-809b-40e6-b436-572622e885fe','telegram','telegram','1000001','summarize the overnight alerts','','','','','','cron',20,0,'queued',0,'',0,0,1791248258,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_526452ba-e1fe-43ca-be66-9b8c5c1ce796','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','rebuild the docs index','full-auto','','','run_17daba33-888a-4f98-ae34-0e8f3fb4be11','','foreground',100,0,'started',0,'claim_5f33ed64-ff68-4886-bade-abb267115f8d',1791248318,1,1791248258,'[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','','','','[]');
INSERT INTO task_queue VALUES('queue_9444fd6e-28eb-4252-88ce-fbbcf9887c3c','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','rotate the preview credentials','','','','','','foreground',100,0,'done',0,'',0,0,1791248258,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_0edf834e-1da1-4ac8-9577-ef6ff5b1da19','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','cli','cli','','/qwer','','','','','','foreground',100,0,'cancelled',0,'',0,0,1791248258,'null','','','','[]');
CREATE TABLE effect_receipts (
	effect_key TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
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
	state TEXT NOT NULL DEFAULT 'accepted',
	payload BLOB NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	tenant_id TEXT NOT NULL DEFAULT '',
	person_id TEXT NOT NULL DEFAULT '',
	preview TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (platform, message_id)
);
INSERT INTO inbound_dedup VALUES('weixin','fixture-wx-pending',1791262907,'pending',x'7b22636f6e74656e74223a22636865636b207468652073746167696e67206c6f6773227d',1791262907,'','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','check the staging logs');
INSERT INTO inbound_dedup VALUES('weixin','fixture-wx-uncertain',1791262907,'dispatching',x'7b22636f6e74656e74223a22726573746172742074686520707265766965772073657276696365227d',1791262907,'gateway unavailable: connection reset','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','restart the preview service');
INSERT INTO inbound_dedup VALUES('feishu','fixture-fs-accepted',1791262907,'accepted',x'7b22636f6e74656e74223a2273756d6d6172697a65207468652072656c65617365206e6f746573227d',1791262907,'','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','summarize the release notes');
INSERT INTO inbound_dedup VALUES('telegram','fixture-tg-other',1791262907,'dispatching',x'7b22636f6e74656e74223a22636865636b2074686520696e766f696365206578706f7274227d',1791262907,'','default','person_4ee800ce-809b-40e6-b436-572622e885fe','check the invoice export');
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
	thread_id TEXT NOT NULL DEFAULT '',
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
	thread_id TEXT NOT NULL DEFAULT '',
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
	thread_id TEXT NOT NULL,
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
INSERT INTO external_watches VALUES('watch_e6671180-bda5-4d97-8eba-eb4bcb1762c0','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','run_5a30d858-ee03-4bf7-9633-6c089bc5acb2','cli','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED','^SUCCEEDED$','',1,'','','','pending','','pending','not_required',30,30,30,1791255458,1791248258,0,0,0,1,0,'','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248258,1791248258,NULL,'','',0,'',0,'','','','','{"version":2,"command_hash":"95e776a4a498058eba1c4cbe16f7f37a1c1f88f50cda924026103cf597a2ab3a","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
INSERT INTO external_watches VALUES('watch_6e7ec0c3-7add-4467-9175-fd54de87041c','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','run_614f5acb-d303-4f6f-a399-2d79b6313f5a','telegram','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED; true','^SUCCEEDED$','',1,'','','','succeeded','','pending','not_required',30,30,30,1791255458,1791248258,0,0,1,1,0,'SUCCEEDED','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248258,1791248258,1791248258,'','',0,'',0,'','','','','{"version":2,"command_hash":"b9056c85234d9017146f98ccf5ec1f0787214eade918913063d185171db9b80f","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
CREATE TABLE steering_mailbox (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	thread_id TEXT NOT NULL DEFAULT '',
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
, attachments_json TEXT, execution_roots_json TEXT);
CREATE TABLE loop_checkpoints (
	run_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	iteration INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	snapshot_json BLOB NOT NULL,
	updated_at INTEGER NOT NULL
, contract_version INTEGER NOT NULL DEFAULT 0, recovery_json TEXT NOT NULL DEFAULT '{}');
INSERT INTO loop_checkpoints VALUES('run_4dcb9ff9-4700-4c55-a84d-868729942c2e','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f',3,'execute_tools','awaiting approval',x'5b7b22726f6c65223a2275736572222c22636f6e74656e74223a226465706c6f79207468652070726576696577206275696c6420746f2073746167696e67227d5d',1791248258,3,x'7b7d');
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
	thread_id TEXT NOT NULL,
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
	package_hash TEXT NOT NULL DEFAULT '',
	resource_manifest_json TEXT NOT NULL DEFAULT '[]',
	source_observation_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_set_hash TEXT NOT NULL DEFAULT '',
	evidence_json TEXT NOT NULL DEFAULT '{}',
	dependency_fingerprint TEXT NOT NULL DEFAULT '',
	verification_environment_fingerprint TEXT NOT NULL DEFAULT '',
	last_verified_at INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	promoted_at INTEGER,
	PRIMARY KEY(control_tenant_id, skill_key, version_hash)
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
INSERT INTO run_work_units VALUES('wu_fcc43af7-1b35-4c28-b569-33857c7de230','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_91f492de-fbcd-4c9d-8de8-abfaf8ea097a',1,'task_d5d3b75d-e9ff-4827-bb71-082db8b77e04','task_d5d3b75d-e9ff-4827-bb71-082db8b77e04','summarize the release notes','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_e1ffc497-d285-4a9c-8cb9-f8d5dd12ec19','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_953f977c-0986-4505-aa90-783105dccd80',1,'task_9e0e03c8-941b-4310-8a91-427b38da492d','task_9e0e03c8-941b-4310-8a91-427b38da492d','migrate the build scripts','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_f26ea2ff-7853-4d80-a0f1-39b28444da2f','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_5372de9a-5f7c-436e-b6a7-c77be6ebf231',1,'task_e863fe02-aeef-442c-b555-568f490498b5','task_e863fe02-aeef-442c-b555-568f490498b5','deploy the preview build','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_9351148d-5ee9-490a-82a1-e8a0205105ad','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_377602f7-56c1-4b07-8de9-bb34264d55d9',1,'task_e863fe02-aeef-442c-b555-568f490498b5','task_e863fe02-aeef-442c-b555-568f490498b5','continue','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_ae85b1bf-3e12-41c0-be37-c60b3548541b','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_5a30d858-ee03-4bf7-9633-6c089bc5acb2',1,'task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','roll out the canary','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_bb57471b-16ae-44a4-8354-37e77fc737b1','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_614f5acb-d303-4f6f-a399-2d79b6313f5a',1,'task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','roll out the stable build','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_d9b5ad14-de00-400f-b486-8e61c674fede','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_4dcb9ff9-4700-4c55-a84d-868729942c2e',1,'task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','deploy the preview build to staging','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_602ee56e-d7d7-464f-8bd7-8db35a8c0acf','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_cac9832e-ef27-4522-b009-1c9e716c0a42',1,'task_63b608a1-7476-4e46-a7e3-21777424126d','task_63b608a1-7476-4e46-a7e3-21777424126d','rename the release branch','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_1e9d49c7-f468-4335-9326-5f2636fe130c','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62',1,'task_a34a929d-0c19-4523-94c3-cd6ebb841324','task_a34a929d-0c19-4523-94c3-cd6ebb841324','Build the release','completed','completed','Build the release','not_applicable','[]',1791248258,1791248258,1791248258,0,9);
INSERT INTO run_work_units VALUES('wu_e524939c-beeb-4404-ae57-bffa0d6f2431','default','person_4ee800ce-809b-40e6-b436-572622e885fe','','run_a4416bc0-7b6d-47f2-a5aa-8958d916754e',1,'task_66429d3f-36df-4bf4-a02c-8cead0b91596','task_66429d3f-36df-4bf4-a02c-8cead0b91596','check the invoice export','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_d3c7f7e7-f102-42a0-b5b6-303fdfd0010e','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_433b7209-b7bd-49bf-bf22-71606142ba47',1,'task_5a9a0d07-adfe-42ce-8d29-df617d9f6641','task_5a9a0d07-adfe-42ce-8d29-df617d9f6641','publish the package','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_ca248a44-8902-43b8-b9fd-bd2175df2796','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_3f703ad3-d77b-4d73-bafe-a75ed7de5595',1,'task_d6d90d57-2d3d-4ca5-93df-edd685a8485e','task_d6d90d57-2d3d-4ca5-93df-edd685a8485e','publish the docs site','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_b0856100-dff3-4cd1-a8b7-75cf68119a3e','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_1061220a-6b38-44cf-bab5-a8f3617f3268',1,'task_8e20ac5a-8057-4ed0-ad17-4cea47cafb7f','task_8e20ac5a-8057-4ed0-ad17-4cea47cafb7f','sync two clusters','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_547c1662-8a93-45a6-ab86-b26c35ed3357','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','run_17daba33-888a-4f98-ae34-0e8f3fb4be11',1,'task_d86bf591-ba30-4471-a584-6dd77e998aff','task_d86bf591-ba30-4471-a584-6dd77e998aff','rebuild the docs index','in_progress','active','','','[]',1791248258,1791248258,NULL,0,0);
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
	package_hash TEXT NOT NULL DEFAULT '',
	activation_source TEXT NOT NULL,
	attachment_mode TEXT NOT NULL DEFAULT '',
	delivery_contract_version INTEGER NOT NULL DEFAULT 0,
	delivery_mode TEXT NOT NULL DEFAULT '',
	delivered_main TEXT NOT NULL DEFAULT '',
	delivered_main_hash TEXT NOT NULL DEFAULT '',
	delivered_main_bytes INTEGER NOT NULL DEFAULT 0,
	resource_manifest_json TEXT NOT NULL DEFAULT '[]',
	state TEXT NOT NULL,
	fallback_reason TEXT NOT NULL DEFAULT '',
	selected_at INTEGER NOT NULL,
	finished_at INTEGER,
	UNIQUE(run_id, sequence)
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
CREATE TABLE task_skill_bindings (
	identity_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
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
	PRIMARY KEY(identity_tenant_id, person_id, thread_id)
);
CREATE TABLE skill_failure_guards (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	failure_signature TEXT NOT NULL,
	failed_step_id TEXT NOT NULL DEFAULT '',
	error_category TEXT NOT NULL DEFAULT '',
	normalized_input_shape TEXT NOT NULL DEFAULT '',
	environment_fingerprint TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'active',
	source_run_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	occurrence_count INTEGER NOT NULL DEFAULT 1,
	last_seen_at INTEGER NOT NULL DEFAULT 0,
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
INSERT INTO schema_migrations VALUES(1,'legacy-baseline',1791248258);
INSERT INTO schema_migrations VALUES(2,'memory-governance-schedule',1791248258);
INSERT INTO schema_migrations VALUES(3,'run-execution-roots',1791248258);
INSERT INTO schema_migrations VALUES(4,'skill-presentation-contract',1791248258);
INSERT INTO schema_migrations VALUES(5,'skill-repair-health',1791248258);
INSERT INTO schema_migrations VALUES(6,'skill-attribution',1791248258);
INSERT INTO schema_migrations VALUES(7,'run-parent-edge',1791248258);
INSERT INTO schema_migrations VALUES(8,'turn-continuity-resolution',1791248258);
INSERT INTO schema_migrations VALUES(9,'run-recovery-contract',1791248258);
INSERT INTO schema_migrations VALUES(10,'run-delivery-override',1791248258);
INSERT INTO schema_migrations VALUES(11,'threaded-work-history',1791248258);
INSERT INTO schema_migrations VALUES(12,'resume-edge-naming',1791248258);
INSERT INTO schema_migrations VALUES(13,'attention-dismissal-only',1791248258);
INSERT INTO schema_migrations VALUES(14,'durable-attachments',1791248258);
INSERT INTO schema_migrations VALUES(15,'drop-task-references',1791248258);
INSERT INTO schema_migrations VALUES(16,'drop-thread-pin',1791248258);
INSERT INTO schema_migrations VALUES(17,'run-plan-step-source',1791248258);
INSERT INTO schema_migrations VALUES(18,'steering-execution-roots',1791248258);
INSERT INTO schema_migrations VALUES(19,'native-im-reply-edges',1791248258);
INSERT INTO schema_migrations VALUES(20,'run-execution-class',1791248258);
INSERT INTO schema_migrations VALUES(21,'turn-choice-observation-receipt',1791248258);
INSERT INTO schema_migrations VALUES(22,'inbound-processing-receipt',1791248258);
INSERT INTO schema_migrations VALUES(23,'external-effect-claims',1791248258);
INSERT INTO schema_migrations VALUES(24,'assessed-plan-cancellations',1791248258);
INSERT INTO schema_migrations VALUES(25,'plan-cancellation-scope-evidence',1791248258);
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
, resolution_kind TEXT NOT NULL DEFAULT '', response_text TEXT NOT NULL DEFAULT '');
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
INSERT INTO run_plan_versions VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',1,'fixture plan','2f829d9a567caf3eaff3f776e65a86f9',1791248258);
INSERT INTO run_plan_versions VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',2,'the person changed the scope','14dc033eb28f8f7ac31b6473055be6be',1791248258);
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
	created_at INTEGER NOT NULL, source_step_id TEXT NOT NULL DEFAULT '', source_plan_version INTEGER NOT NULL DEFAULT 0, prior_verification_reused INTEGER NOT NULL DEFAULT 0, reuse_reason TEXT NOT NULL DEFAULT '', cancellation_disposition TEXT NOT NULL DEFAULT '', cancellation_reason TEXT NOT NULL DEFAULT '', user_takeover_quote TEXT NOT NULL DEFAULT '', replacement_step_id TEXT NOT NULL DEFAULT '', scope_change_quote TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, plan_version, step_id),
	UNIQUE (run_id, plan_version, sequence)
);
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',1,'step_6a6bd073-3dba-430b-bd06-30524437a6ee',1,'Build the release','completed','the build passes',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',1,1791248258,'',0,0,'','','','','','');
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',1,'step_5260600a-45d7-4aa2-8228-08748f14cdd5',2,'Run the live check','pending','the live check passes',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',0,1791248258,'',0,0,'','','','','','');
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',1,'step_7c98a5d9-7c82-4b5d-a86d-0df292a034db',3,'Publish notes','pending','',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',0,1791248258,'',0,0,'','','','','','');
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',2,'step_6a6bd073-3dba-430b-bd06-30524437a6ee',1,'Build the release','completed','the build passes',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',1,1791248258,'',0,0,'','','','','','');
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',2,'step_5260600a-45d7-4aa2-8228-08748f14cdd5',2,'Run the live check','cancelled','the live check passes',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',0,1791248258,'',0,0,'','not_required','the person removed the live check','','','skip the live check');
INSERT INTO run_plan_steps VALUES('run_ddc44065-c2bf-4896-9eac-a5e1ccb6fb62','default',2,'step_7c98a5d9-7c82-4b5d-a86d-0df292a034db',3,'Publish notes','cancelled','',0,'','wu_1e9d49c7-f468-4335-9326-5f2636fe130c',0,1791248258,'',0,0,'','user_takeover','the person publishes','ship the release','','');
CREATE TABLE external_watch_groups (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
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
CREATE TABLE IF NOT EXISTS "threads" (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			person_id TEXT NOT NULL,
			workspace_id TEXT,
			kind TEXT NOT NULL,
			visibility TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			last_activity_at INTEGER NOT NULL
		);
INSERT INTO threads VALUES('task_d5d3b75d-e9ff-4827-bb71-082db8b77e04','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture finished work','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_9e0e03c8-941b-4310-8a91-427b38da492d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture interrupted work','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_e863fe02-aeef-442c-b555-568f490498b5','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture parked work','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_95b7acd0-e0dc-48d6-a5e6-c3ca140fcec6','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture watched rollout','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_a0237ce2-7575-4c63-8314-1aafd80fbdc4','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture finished rollout','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_5a33a765-7cab-42f5-85ed-0fd3c0c16d5f','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture parked approval','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_63b608a1-7476-4e46-a7e3-21777424126d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture pending clarification','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_a34a929d-0c19-4523-94c3-cd6ebb841324','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture planned work','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_66429d3f-36df-4bf4-a02c-8cead0b91596','default','person_4ee800ce-809b-40e6-b436-572622e885fe','','work','listed','Fixture other person work','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_5a9a0d07-adfe-42ce-8d29-df617d9f6641','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture effect holder','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_d6d90d57-2d3d-4ca5-93df-edd685a8485e','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture effect waiter','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_8e20ac5a-8057-4ed0-ad17-4cea47cafb7f','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture effect reserver','',1791248258,1791248258,1791248258);
INSERT INTO threads VALUES('task_d86bf591-ba30-4471-a584-6dd77e998aff','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','','work','listed','Fixture drained work','',1791248258,1791248258,1791248258);
CREATE TABLE native_im_reply_edges (
				platform TEXT NOT NULL,
				channel TEXT NOT NULL,
				message_id TEXT NOT NULL,
				outbound_id TEXT NOT NULL,
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL DEFAULT '',
				approval_id TEXT NOT NULL DEFAULT '',
				clarify_id TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				PRIMARY KEY (platform, channel, message_id)
			);
CREATE TABLE external_effect_claims (
				id TEXT PRIMARY KEY,
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				effect_id TEXT NOT NULL,
				target_key TEXT NOT NULL,
				state TEXT NOT NULL CHECK(state IN ('reserved', 'uncertain', 'observed')),
				observation_ref TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				UNIQUE(tenant_id, run_id, effect_id, target_key)
			);
INSERT INTO external_effect_claims VALUES('effectclaim_3046815f-0985-4038-b14a-140064d8981d','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','run_433b7209-b7bd-49bf-bf22-71606142ba47','effect-publish','registry:npm','uncertain','',1791248258,1791248258);
INSERT INTO external_effect_claims VALUES('effectclaim_82ef5ed5-134f-4a12-b4d5-981ead7fc971','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','run_433b7209-b7bd-49bf-bf22-71606142ba47','effect-shell','git:origin','observed','watch:fixture:r1',1791248258,1791248258);
INSERT INTO external_effect_claims VALUES('effectclaim_ad3efc45-9443-406b-bd3e-edfefef09ffe','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','run_1061220a-6b38-44cf-bab5-a8f3617f3268','effect-sync','cluster:east','reserved','',1791248258,1791248258);
INSERT INTO external_effect_claims VALUES('effectclaim_531befcd-1726-4c57-a054-571c7e253e1c','default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','run_1061220a-6b38-44cf-bab5-a8f3617f3268','effect-sync','cluster:west','reserved','',1791248258,1791248258);
CREATE TABLE external_resource_waits (
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				effect_id TEXT NOT NULL,
				targets_json TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status IN ('pending', 'queued')),
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (tenant_id, run_id, effect_id)
			);
INSERT INTO external_resource_waits VALUES('default','person_4c5dfbfd-ce19-4f31-be5f-66340fa0f9f1','run_3f703ad3-d77b-4d73-bafe-a75ed7de5595','effect-docs','["registry:npm"]','pending',1791248258,1791248258);
CREATE INDEX idx_workspace_knowledge_scope
	ON workspace_knowledge_sections(tenant_id, person_id, workspace_id, updated_at);
CREATE INDEX idx_execution_leases_person
	ON execution_leases(tenant_id, person_id, created_at);
CREATE INDEX idx_execution_capability_active
	ON execution_capability_grants(tenant_id, person_id, workspace_id, capability, expires_at);
CREATE INDEX idx_task_runs_task_started ON "runs"(tenant_id, thread_id, started_at);
CREATE INDEX idx_task_runs_person_status ON "runs"(tenant_id, person_id, status, started_at);
CREATE INDEX idx_task_resolution_events_owner
	ON task_resolution_events(tenant_id, person_id, created_at);
CREATE INDEX idx_task_resolution_events_run
	ON task_resolution_events(tenant_id, run_id, created_at);
CREATE UNIQUE INDEX idx_task_resolution_events_run_unique
	ON task_resolution_events(tenant_id, run_id) WHERE run_id != '';
CREATE INDEX idx_task_blockers_open
	ON task_blockers(tenant_id, thread_id, status, created_at);
CREATE INDEX idx_task_events_task ON task_events(thread_id, created_at);
CREATE INDEX idx_channel_messages_person_channel ON channel_messages(tenant_id, person_id, channel, created_at);
CREATE INDEX idx_task_handoffs_task_created ON task_handoffs(thread_id, created_at);
CREATE INDEX idx_task_artifacts_task ON task_artifacts(thread_id, created_at);
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
	ON loop_checkpoints(tenant_id, thread_id, updated_at);
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
CREATE INDEX idx_skill_candidate_evidence_set
	ON skill_candidate_evidence_snapshots(control_tenant_id, evidence_set_hash);
CREATE INDEX idx_run_work_units_run
	ON run_work_units(identity_tenant_id, run_id, sequence);
CREATE UNIQUE INDEX idx_run_skill_activations_live_lane
	ON run_skill_activations(run_id, work_unit_id, execution_lane)
	WHERE state IN ('selected', 'active');
CREATE INDEX idx_run_skill_activations_skill
	ON run_skill_activations(control_tenant_id, skill_key, version_hash, selected_at);
CREATE INDEX idx_skill_candidate_refs_work_unit
	ON skill_candidate_refs(identity_tenant_id, run_id, work_unit_id, issued_at);
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
		ON "runs"(tenant_id, thread_id, work_key, status, started_at);
CREATE UNIQUE INDEX idx_task_events_cursor ON task_events(cursor);
CREATE UNIQUE INDEX idx_task_events_idempotency
			ON task_events(idempotency_key)
			WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
CREATE UNIQUE INDEX idx_task_queue_idempotency ON task_queue(tenant_id, idempotency_key)
			WHERE idempotency_key != '';
CREATE INDEX idx_task_queue_schedule
			ON task_queue(status, not_before, priority DESC, created_at);
CREATE INDEX idx_task_queue_claims
			ON task_queue(status, lease_until);
CREATE INDEX idx_approval_resume_authorization
			ON approval_requests(tenant_id, person_id, thread_id, authorization_fingerprint, authorization_state);
CREATE INDEX idx_maintenance_provider_calls_person
			ON maintenance_provider_calls(tenant_id, person_id, created_at);
CREATE INDEX idx_memory_governance_due
	ON memory_governance_schedule(next_due_at, tenant_id, person_id);
CREATE INDEX idx_skill_versions_review_due
				ON skill_versions(control_tenant_id, state, last_verified_at);
CREATE INDEX idx_skill_attributions_skill
	ON skill_attributions(control_tenant_id, skill_key, observed_at);
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
CREATE INDEX idx_run_delivery_overrides_person
	ON run_delivery_overrides(tenant_id, person_id, updated_at);
CREATE INDEX idx_threads_owner
			ON threads(tenant_id, person_id, visibility, last_activity_at);
CREATE INDEX idx_runs_thread_started
			ON runs(tenant_id, thread_id, started_at);
CREATE INDEX idx_runs_person_status
			ON runs(tenant_id, person_id, status, started_at);
CREATE INDEX idx_runs_attention
			ON runs(tenant_id, person_id, status, attention_dismissed_at, started_at);
CREATE UNIQUE INDEX idx_runs_resumes_once
	ON runs(tenant_id, resumes_run_id) WHERE resumes_run_id <> '';
CREATE INDEX idx_native_im_reply_edges_outbound
				ON native_im_reply_edges(outbound_id);
CREATE INDEX idx_inbound_processing_owner
				ON inbound_dedup(tenant_id, person_id, state, updated_at) WHERE state <> 'accepted';
CREATE INDEX idx_external_effect_claims_active
				ON external_effect_claims(tenant_id, person_id, state, target_key)
				WHERE state <> 'observed';
CREATE INDEX idx_external_resource_waits_pending
				ON external_resource_waits(status, created_at) WHERE status = 'pending';
COMMIT;
