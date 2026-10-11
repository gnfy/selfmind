-- Released v0.1.0-beta.27, commit d90f4ff2c4f6, schema v17.
-- Created by that release's OpenStore and seeded through its store API with
-- synthetic rows (sqlite3 .dump). No operator data.
PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'personal',
	created_at INTEGER NOT NULL
);
INSERT INTO tenants VALUES('default','default','personal',1791248252);
CREATE TABLE persons (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	display_name TEXT,
	default_workspace_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO persons VALUES('person_8cc9b727-d853-4a6c-a078-69816854c28c','default','Fixture Owner',NULL,1791248252,1791248252);
INSERT INTO persons VALUES('person_51b31149-dc18-4d87-8165-048a504d8346','default','Fixture Other',NULL,1791248252,1791248252);
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
INSERT INTO accounts VALUES('acct_279c8858-d203-459e-aed5-780b4f676d1c','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','fixture-owner','Fixture Owner','active',1791248252,1791248252,NULL);
INSERT INTO accounts VALUES('acct_7db039ed-f37a-44b1-9da9-4fe7dd173645','default','person_51b31149-dc18-4d87-8165-048a504d8346','telegram','1000001','Fixture Other','active',1791248252,1791248252,NULL);
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
, execution_roots_json TEXT NOT NULL DEFAULT '[]', resumes_run_id TEXT NOT NULL DEFAULT '', recovery_contract_version INTEGER NOT NULL DEFAULT 0, attention_dismissed_at INTEGER NOT NULL DEFAULT 0, attention_dismissed_by TEXT NOT NULL DEFAULT '');
INSERT INTO runs VALUES('run_82695c57-56d5-4fd0-aae7-47aad5382ad3','task_07fa7e81-8dd5-4a27-b3a6-edffe598a299','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','summarize the release notes','done',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
INSERT INTO runs VALUES('run_99f0ac31-fbd7-4602-869d-a9da24fa081d','task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','migrate the build scripts','interrupted',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
INSERT INTO runs VALUES('run_4c947d54-ffba-4b8e-8e3d-c409a5664d28','task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','deploy the preview build','waiting_user',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
INSERT INTO runs VALUES('run_ff75f96d-bd92-479b-93fb-2e0c7376469e','task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','continue','done',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_4c947d54-ffba-4b8e-8e3d-c409a5664d28',1,0,'');
INSERT INTO runs VALUES('run_8ed2eb72-949e-4012-abac-483950f8f8e5','task_8ccc6f25-98e1-408d-a99d-1893b8e91937','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','roll out the canary','waiting_external',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
INSERT INTO runs VALUES('run_474fead4-a8bf-4fc1-a248-8ba2bfc5150a','task_43a87ed8-0652-4939-9449-7ec703c031e0','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','telegram','roll out the stable build','waiting_external',1791248252,1791248252,1791248252,0,NULL,'','','null','',1,0,'');
INSERT INTO runs VALUES('run_04c6ff64-2339-4c36-98c9-94b9ba991a10','task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','deploy the preview build to staging','waiting_user',1791248252,1791248252,1791248252,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
INSERT INTO runs VALUES('run_a151096d-65e2-4e56-9309-31847965a035','task_789c7666-1b71-46e2-b535-7e20441e2365','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','rename the release branch','waiting_user',1791248252,1791248252,1791248252,0,NULL,'','','null','',1,0,'');
INSERT INTO runs VALUES('run_13f6d535-0856-430a-a7ac-eed7d1688795','task_c3a4b902-c774-4490-998b-9a66e0ce4b0b','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','ship the release; skip the live check this time','waiting_user',1791248252,1791248252,1791248252,0,NULL,'','','null','',1,0,'');
INSERT INTO runs VALUES('run_6034195e-ed33-4154-bb2d-ae9ca89a0e85','task_2d5c40c0-ce17-4105-94e0-50e0068eaede','default','person_51b31149-dc18-4d87-8165-048a504d8346','','telegram','check the invoice export','blocked',1791248252,1791248252,1791248252,0,NULL,'','','null','',1,0,'');
INSERT INTO runs VALUES('run_222aff42-8ba9-4213-9ac9-fd35b563b8fb','task_87b28f85-f423-45a5-9d45-160217737999','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','cli','rebuild the docs index','running',1791248252,NULL,1791248252,0,NULL,'','','[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','',1,0,'');
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
INSERT INTO task_events VALUES('event_a09d4563-94b1-4e8f-a520-75d0e2a4638a',1,'task_07fa7e81-8dd5-4a27-b3a6-edffe598a299','run_82695c57-56d5-4fd0-aae7-47aad5382ad3','run.started','task','cli','{"input":"summarize the release notes","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_e3b49081-2539-497c-a9cd-c326134a1625',2,'task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','run_99f0ac31-fbd7-4602-869d-a9da24fa081d','run.started','task','cli','{"input":"migrate the build scripts","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_3b9744dd-d09b-48fa-8434-1757c45fd3eb',3,'task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','run_4c947d54-ffba-4b8e-8e3d-c409a5664d28','run.started','task','cli','{"input":"deploy the preview build","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_01113377-7a37-4fdb-b7c8-cdba3d136fcd',4,'task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','run_ff75f96d-bd92-479b-93fb-2e0c7376469e','run.started','task','cli','{"input":"continue","resumes_run_id":"run_4c947d54-ffba-4b8e-8e3d-c409a5664d28"}',NULL,1791248252);
INSERT INTO task_events VALUES('event_4393601d-ac45-4e3d-8bb8-8640ce5497bd',5,'task_8ccc6f25-98e1-408d-a99d-1893b8e91937','run_8ed2eb72-949e-4012-abac-483950f8f8e5','run.started','task','cli','{"input":"roll out the canary","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_06f3d7fb-90db-4c83-acd9-6a2ea177f0e7',6,'task_43a87ed8-0652-4939-9449-7ec703c031e0','run_474fead4-a8bf-4fc1-a248-8ba2bfc5150a','run.started','task','telegram','{"input":"roll out the stable build","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_e734a262-a3dc-42be-b2f4-41f263321379',7,'task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','run_04c6ff64-2339-4c36-98c9-94b9ba991a10','run.started','task','cli','{"input":"deploy the preview build to staging","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_46455045-8a43-4fd6-a26d-1f71630d4eda',8,'task_789c7666-1b71-46e2-b535-7e20441e2365','run_a151096d-65e2-4e56-9309-31847965a035','run.started','task','cli','{"input":"rename the release branch","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_12495f16-d20e-47a7-b99e-92c721855024',9,'task_c3a4b902-c774-4490-998b-9a66e0ce4b0b','run_13f6d535-0856-430a-a7ac-eed7d1688795','run.started','task','cli','{"input":"ship the release; skip the live check this time","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_1a8d1380-5971-4a52-8a04-4a0386fb8b22',10,'task_2d5c40c0-ce17-4105-94e0-50e0068eaede','run_6034195e-ed33-4154-bb2d-ae9ca89a0e85','run.started','task','telegram','{"input":"check the invoice export","resumes_run_id":""}',NULL,1791248252);
INSERT INTO task_events VALUES('event_c4900a42-0de5-4a27-b736-738688064df9',11,'task_87b28f85-f423-45a5-9d45-160217737999','run_222aff42-8ba9-4213-9ac9-fd35b563b8fb','run.started','task','cli','{"input":"rebuild the docs index","resumes_run_id":""}',NULL,1791248252);
CREATE TABLE event_sequence (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	next_cursor INTEGER NOT NULL
);
INSERT INTO event_sequence VALUES(1,11);
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
INSERT INTO approval_requests VALUES('apr_77a94784-1cc8-44e0-ac18-b90226c0b7e3','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','run_04c6ff64-2339-4c36-98c9-94b9ba991a10','tool_call','{"tool":"terminal","command":"./deploy.sh staging"}','pending','cli','',NULL,NULL,1791248252,1791248252,NULL,NULL,NULL,'parked',1791248252,'approval wait budget elapsed',NULL,NULL,NULL,'resume:v1:fixture-deploy',NULL,NULL,NULL);
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
INSERT INTO clarify_requests VALUES('clr_0d269c0b-ade6-4055-89e5-de1545b8ff0d','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','task_789c7666-1b71-46e2-b535-7e20441e2365','run_a151096d-65e2-4e56-9309-31847965a035','Which branch should be renamed?','[]','pending','','cli',1791248252,1791248252,NULL);
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
INSERT INTO task_queue VALUES('queue_351c81d9-193d-4d0b-978b-402daca7287a','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','fixture-owner','look up today''s release notes','smart','','','','','foreground',100,0,'queued',0,'',0,0,1791248252,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','','','','[{"kind":"image","path":"/srv/fixture/attachments/shot.png","mime_type":"image/png","name":"shot.png","size":42}]');
INSERT INTO task_queue VALUES('queue_c2ce9932-b6ec-4cb7-98b0-48b58ddf7844','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','Resume the original task after a durable external observation.','','','task_43a87ed8-0652-4939-9449-7ec703c031e0','','external-watch:watch_44b47ca4-6676-4a71-929b-e8db198e7993:r1','finalization',80,0,'queued',0,'',0,0,1791248252,'null','run_474fead4-a8bf-4fc1-a248-8ba2bfc5150a','','','[]');
INSERT INTO task_queue VALUES('queue_47b8ff48-d513-4882-99cf-13896d75379c','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','Recover the interrupted run in verification-only mode.','','','task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','','run-recovery:run_99f0ac31-fbd7-4602-869d-a9da24fa081d:verify_only','recovery',60,0,'queued',0,'',0,0,1791248252,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_99f0ac31-fbd7-4602-869d-a9da24fa081d','','','[]');
INSERT INTO task_queue VALUES('queue_00ce3741-cbef-4f37-b69e-903fc027ec55','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','Continue this exact work after the provider became available.','','','task_8ccc6f25-98e1-408d-a99d-1893b8e91937','','provider-wait:run_8ed2eb72-949e-4012-abac-483950f8f8e5','finalization',80,1791251852,'queued',0,'',0,0,1791248252,'null','run_8ed2eb72-949e-4012-abac-483950f8f8e5','','','[]');
INSERT INTO task_queue VALUES('queue_39580095-d3eb-4305-9d88-86050e572a47','default','person_51b31149-dc18-4d87-8165-048a504d8346','telegram','telegram','1000001','summarize the overnight alerts','','','','','','cron',20,0,'queued',0,'',0,0,1791248252,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_6ebff8b0-77fe-4189-bf50-9fd159d31e45','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','rebuild the docs index','full-auto','','','run_222aff42-8ba9-4213-9ac9-fd35b563b8fb','','foreground',100,0,'started',0,'claim_9c09ed55-f333-4264-ba7a-142fbf265ac4',1791248312,1,1791248252,'[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','','','','[]');
INSERT INTO task_queue VALUES('queue_3e18659f-c6e4-4887-98ef-8b11f57ae73f','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','rotate the preview credentials','','','','','','foreground',100,0,'done',0,'',0,0,1791248252,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_bda99d7c-a7ff-4033-bf02-268bb5051ccd','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','cli','cli','','/qwer','','','','','','foreground',100,0,'cancelled',0,'',0,0,1791248252,'null','','','','[]');
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
	PRIMARY KEY (platform, message_id)
);
INSERT INTO inbound_dedup VALUES('telegram','fixture-tg-update-1',1791262905);
INSERT INTO inbound_dedup VALUES('feishu','fixture-fs-msg-1',1791262905);
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
INSERT INTO external_watches VALUES('watch_ffae8c35-8239-43c8-a0c2-11303d533834','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','task_8ccc6f25-98e1-408d-a99d-1893b8e91937','run_8ed2eb72-949e-4012-abac-483950f8f8e5','cli','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED','^SUCCEEDED$','',1,'','','','pending','','pending','not_required',30,30,30,1791255452,1791248252,0,0,0,1,0,'','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248252,1791248252,NULL,'','',0,'',0,'','','','','{"version":2,"command_hash":"95e776a4a498058eba1c4cbe16f7f37a1c1f88f50cda924026103cf597a2ab3a","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
INSERT INTO external_watches VALUES('watch_44b47ca4-6676-4a71-929b-e8db198e7993','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','task_43a87ed8-0652-4939-9449-7ec703c031e0','run_474fead4-a8bf-4fc1-a248-8ba2bfc5150a','telegram','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED; true','^SUCCEEDED$','',1,'','','','succeeded','','pending','not_required',30,30,30,1791255452,1791248252,0,0,1,1,0,'SUCCEEDED','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248252,1791248252,1791248252,'','',0,'',0,'','','','','{"version":2,"command_hash":"b9056c85234d9017146f98ccf5ec1f0787214eade918913063d185171db9b80f","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
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
, attachments_json TEXT);
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
INSERT INTO loop_checkpoints VALUES('run_04c6ff64-2339-4c36-98c9-94b9ba991a10','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','task_16dddb1c-d90a-4cc4-b13a-14120e168bf1',3,'execute_tools','awaiting approval',x'5b7b22726f6c65223a2275736572222c22636f6e74656e74223a226465706c6f79207468652070726576696577206275696c6420746f2073746167696e67227d5d',1791248252,1,x'7b7d');
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
INSERT INTO run_work_units VALUES('wu_2ab8d886-dd49-4af0-9152-a1fd25c06cb6','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_82695c57-56d5-4fd0-aae7-47aad5382ad3',1,'task_07fa7e81-8dd5-4a27-b3a6-edffe598a299','task_07fa7e81-8dd5-4a27-b3a6-edffe598a299','summarize the release notes','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_e5b472c4-893d-4768-a430-f67b768e6510','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_99f0ac31-fbd7-4602-869d-a9da24fa081d',1,'task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','migrate the build scripts','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_a2b6af73-e061-4f43-945c-d3eb57abd19b','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_4c947d54-ffba-4b8e-8e3d-c409a5664d28',1,'task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','deploy the preview build','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_211a2325-192d-4b04-99a0-598dc710ae13','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_ff75f96d-bd92-479b-93fb-2e0c7376469e',1,'task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','continue','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_93ce7e25-4fc0-4057-95cb-1d5428de63f6','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_8ed2eb72-949e-4012-abac-483950f8f8e5',1,'task_8ccc6f25-98e1-408d-a99d-1893b8e91937','task_8ccc6f25-98e1-408d-a99d-1893b8e91937','roll out the canary','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_d6376dcb-c800-405f-9bec-f72a46abd062','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_474fead4-a8bf-4fc1-a248-8ba2bfc5150a',1,'task_43a87ed8-0652-4939-9449-7ec703c031e0','task_43a87ed8-0652-4939-9449-7ec703c031e0','roll out the stable build','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_21cdb5f2-bd87-403b-9141-0d0b19a6a566','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_04c6ff64-2339-4c36-98c9-94b9ba991a10',1,'task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','deploy the preview build to staging','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_69e9e28d-ee25-4352-bedb-5483f69a2b25','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_a151096d-65e2-4e56-9309-31847965a035',1,'task_789c7666-1b71-46e2-b535-7e20441e2365','task_789c7666-1b71-46e2-b535-7e20441e2365','rename the release branch','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_a2554b7d-681a-4b68-b754-7c5e5ed9ef26','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_13f6d535-0856-430a-a7ac-eed7d1688795',1,'task_c3a4b902-c774-4490-998b-9a66e0ce4b0b','task_c3a4b902-c774-4490-998b-9a66e0ce4b0b','Build the release','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_830f2e45-770d-40a4-b230-d6cea94140f2','default','person_51b31149-dc18-4d87-8165-048a504d8346','','run_6034195e-ed33-4154-bb2d-ae9ca89a0e85',1,'task_2d5c40c0-ce17-4105-94e0-50e0068eaede','task_2d5c40c0-ce17-4105-94e0-50e0068eaede','check the invoice export','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_5bdef56d-6ab7-4cde-9393-cefc21487679','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','run_222aff42-8ba9-4213-9ac9-fd35b563b8fb',1,'task_87b28f85-f423-45a5-9d45-160217737999','task_87b28f85-f423-45a5-9d45-160217737999','rebuild the docs index','in_progress','active','','','[]',1791248252,1791248252,NULL,0,0);
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
INSERT INTO schema_migrations VALUES(1,'legacy-baseline',1791248252);
INSERT INTO schema_migrations VALUES(2,'memory-governance-schedule',1791248252);
INSERT INTO schema_migrations VALUES(3,'run-execution-roots',1791248252);
INSERT INTO schema_migrations VALUES(4,'skill-presentation-contract',1791248252);
INSERT INTO schema_migrations VALUES(5,'skill-repair-health',1791248252);
INSERT INTO schema_migrations VALUES(6,'skill-attribution',1791248252);
INSERT INTO schema_migrations VALUES(7,'run-parent-edge',1791248252);
INSERT INTO schema_migrations VALUES(8,'turn-continuity-resolution',1791248252);
INSERT INTO schema_migrations VALUES(9,'run-recovery-contract',1791248252);
INSERT INTO schema_migrations VALUES(10,'run-delivery-override',1791248252);
INSERT INTO schema_migrations VALUES(11,'threaded-work-history',1791248252);
INSERT INTO schema_migrations VALUES(12,'resume-edge-naming',1791248252);
INSERT INTO schema_migrations VALUES(13,'attention-dismissal-only',1791248252);
INSERT INTO schema_migrations VALUES(14,'durable-attachments',1791248252);
INSERT INTO schema_migrations VALUES(15,'drop-task-references',1791248252);
INSERT INTO schema_migrations VALUES(16,'drop-thread-pin',1791248252);
INSERT INTO schema_migrations VALUES(17,'run-plan-step-source',1791248252);
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
INSERT INTO run_plan_versions VALUES('run_13f6d535-0856-430a-a7ac-eed7d1688795','default',1,'fixture plan','760423cf69aec4e212e7b79c9008a4ac',1791248252);
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
	created_at INTEGER NOT NULL, source_step_id TEXT NOT NULL DEFAULT '', source_plan_version INTEGER NOT NULL DEFAULT 0, prior_verification_reused INTEGER NOT NULL DEFAULT 0, reuse_reason TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, plan_version, step_id),
	UNIQUE (run_id, plan_version, sequence)
);
INSERT INTO run_plan_steps VALUES('run_13f6d535-0856-430a-a7ac-eed7d1688795','default',1,'step_c1ab8b58-5cc9-4809-9b33-22d653b92c1a',1,'Build the release','completed','the build passes',0,'','wu_a2554b7d-681a-4b68-b754-7c5e5ed9ef26',1,1791248252,'',0,0,'');
INSERT INTO run_plan_steps VALUES('run_13f6d535-0856-430a-a7ac-eed7d1688795','default',1,'step_9c8f1bb4-bf18-4535-bf80-8ec2c4448c68',2,'Run the live check','in_progress','the live check passes',0,'','wu_a2554b7d-681a-4b68-b754-7c5e5ed9ef26',0,1791248252,'',0,0,'');
INSERT INTO run_plan_steps VALUES('run_13f6d535-0856-430a-a7ac-eed7d1688795','default',1,'step_6acd8034-9a7a-480a-9875-590c19bcdbc6',3,'Publish notes','pending','',0,'','wu_a2554b7d-681a-4b68-b754-7c5e5ed9ef26',0,1791248252,'',0,0,'');
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
INSERT INTO threads VALUES('task_07fa7e81-8dd5-4a27-b3a6-edffe598a299','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture finished work','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_88eeae1e-0cc6-4f25-863d-7ee4eb75a1e2','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture interrupted work','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_1be99ac9-81a8-44ea-a3e8-8f79de74055e','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture parked work','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_8ccc6f25-98e1-408d-a99d-1893b8e91937','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture watched rollout','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_43a87ed8-0652-4939-9449-7ec703c031e0','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture finished rollout','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_16dddb1c-d90a-4cc4-b13a-14120e168bf1','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture parked approval','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_789c7666-1b71-46e2-b535-7e20441e2365','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture pending clarification','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_c3a4b902-c774-4490-998b-9a66e0ce4b0b','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture planned work','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_2d5c40c0-ce17-4105-94e0-50e0068eaede','default','person_51b31149-dc18-4d87-8165-048a504d8346','','work','listed','Fixture other person work','',1791248252,1791248252,1791248252);
INSERT INTO threads VALUES('task_87b28f85-f423-45a5-9d45-160217737999','default','person_8cc9b727-d853-4a6c-a078-69816854c28c','','work','listed','Fixture drained work','',1791248252,1791248252,1791248252);
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
COMMIT;
