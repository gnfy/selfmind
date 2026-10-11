-- Released v0.1.0-beta.28, commit 5e4be677d30b, schema v24.
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
INSERT INTO tenants VALUES('default','default','personal',1791248257);
CREATE TABLE persons (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	display_name TEXT,
	default_workspace_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO persons VALUES('person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','default','Fixture Owner',NULL,1791248257,1791248257);
INSERT INTO persons VALUES('person_855dd362-7d6a-41b4-8a79-a01c12276c3e','default','Fixture Other',NULL,1791248257,1791248257);
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
INSERT INTO accounts VALUES('acct_aac18855-8f2a-44c7-b418-80ccb5e11cd8','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','fixture-owner','Fixture Owner','active',1791248257,1791248257,NULL);
INSERT INTO accounts VALUES('acct_b5191db2-e8c2-4848-8e1c-e56c157dee25','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','telegram','1000001','Fixture Other','active',1791248257,1791248257,NULL);
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
INSERT INTO runs VALUES('run_151a51c0-9948-491f-83a4-4b92e6073fa4','task_658b5d0e-65c9-44b2-99fc-0d808629b857','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','summarize the release notes','done',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
INSERT INTO runs VALUES('run_b6445199-ef5d-48b8-a3f5-bc28692b7911','task_c7bfcc53-8670-4843-bd35-784b4003c1d0','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','migrate the build scripts','interrupted',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
INSERT INTO runs VALUES('run_b6821f4e-ba34-4e8b-9c93-89799f2b024b','task_52870343-a2b0-4ff6-aec3-2b112c176f57','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','deploy the preview build','waiting_user',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
INSERT INTO runs VALUES('run_5ee7c377-cfcd-4ef9-9620-d01fd40b7b3d','task_52870343-a2b0-4ff6-aec3-2b112c176f57','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','continue','done',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_b6821f4e-ba34-4e8b-9c93-89799f2b024b',2,0,'','work');
INSERT INTO runs VALUES('run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9','task_3907b332-bccb-43d6-99d5-5412d662c77e','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','roll out the canary','waiting_external',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
INSERT INTO runs VALUES('run_ef1ce369-6f68-47ec-8f32-af00ca6462f5','task_f2edda5b-cc7d-4647-b938-3433ea241604','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','telegram','roll out the stable build','waiting_external',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_177c4360-1f0c-4cc4-bc77-9a15f8369064','task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','deploy the preview build to staging','waiting_user',1791248257,1791248257,1791248257,0,NULL,'','','[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
INSERT INTO runs VALUES('run_0e47d612-b65e-4457-925d-f1713302ed82','task_45f1e2cd-291a-42a9-9292-4a025dd18b85','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','rename the release branch','waiting_user',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','task_679cc50e-f20b-4eaf-ad7f-f698daf4dfab','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','ship the release; skip the live check this time','waiting_user',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_33122f43-dade-469a-895c-939d64b9f2a6','task_e4f26535-3e20-4def-8fc5-9c775b318372','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','','telegram','check the invoice export','blocked',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_b51b78a8-da1c-492a-86b1-e9d4a6c0f070','task_2d76929f-958d-487c-9551-4783e095f08e','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','publish the package','interrupted',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_2e00d913-b24b-4f0c-8733-9180146c9c9c','task_c15a822f-2945-4ede-9cd4-8802844fa434','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','publish the docs site','waiting_external',1791248257,1791248257,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_8a4a7d89-f2a8-4ca0-84fb-d801b68c1b11','task_ee7e8763-d9a5-4421-9fb2-6305f8fbf1d7','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','sync two clusters','running',1791248257,NULL,1791248257,0,NULL,'','','null','',2,0,'','work');
INSERT INTO runs VALUES('run_1889c130-cfa2-4aae-a209-904376ed9da8','task_000a5724-893e-450d-922b-34a40b277bf9','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','cli','rebuild the docs index','running',1791248257,NULL,1791248257,0,NULL,'','','[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','',2,0,'','work');
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
INSERT INTO task_events VALUES('event_3c9078b9-c3a0-477c-a562-b1d3c747243b',1,'task_658b5d0e-65c9-44b2-99fc-0d808629b857','run_151a51c0-9948-491f-83a4-4b92e6073fa4','run.started','task','cli','{"input":"summarize the release notes","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_a00cf744-9e8c-47e6-95c3-3861d7a9427b',2,'task_c7bfcc53-8670-4843-bd35-784b4003c1d0','run_b6445199-ef5d-48b8-a3f5-bc28692b7911','run.started','task','cli','{"input":"migrate the build scripts","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_d55e87c0-29cd-4ada-aa50-d8b4f1eff7dc',3,'task_52870343-a2b0-4ff6-aec3-2b112c176f57','run_b6821f4e-ba34-4e8b-9c93-89799f2b024b','run.started','task','cli','{"input":"deploy the preview build","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_e3b2df7a-6a9a-4762-83b6-98581ddb1edf',4,'task_52870343-a2b0-4ff6-aec3-2b112c176f57','run_5ee7c377-cfcd-4ef9-9620-d01fd40b7b3d','run.started','task','cli','{"input":"continue","resumes_run_id":"run_b6821f4e-ba34-4e8b-9c93-89799f2b024b"}',NULL,1791248257);
INSERT INTO task_events VALUES('event_bb9d6e0c-42ac-4a87-9463-dc27ad8d58b6',5,'task_3907b332-bccb-43d6-99d5-5412d662c77e','run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9','run.started','task','cli','{"input":"roll out the canary","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_4cff399e-1e6d-4ad3-8805-6281740db69e',6,'task_f2edda5b-cc7d-4647-b938-3433ea241604','run_ef1ce369-6f68-47ec-8f32-af00ca6462f5','run.started','task','telegram','{"input":"roll out the stable build","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_b141adf4-44e0-4280-833c-ae9fdb830193',7,'task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','run_177c4360-1f0c-4cc4-bc77-9a15f8369064','run.started','task','cli','{"input":"deploy the preview build to staging","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_a01a493d-5f0b-44bd-bf67-9071e97b5b26',8,'task_45f1e2cd-291a-42a9-9292-4a025dd18b85','run_0e47d612-b65e-4457-925d-f1713302ed82','run.started','task','cli','{"input":"rename the release branch","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_b2196366-8e34-4f64-ba25-5d3b062bb4a4',9,'task_679cc50e-f20b-4eaf-ad7f-f698daf4dfab','run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','run.started','task','cli','{"input":"ship the release; skip the live check this time","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_1b6660d0-6cad-47e9-90f2-671822c0bf2e',10,'task_e4f26535-3e20-4def-8fc5-9c775b318372','run_33122f43-dade-469a-895c-939d64b9f2a6','run.started','task','telegram','{"input":"check the invoice export","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_bd1ce825-97a5-4255-b734-fda760a18d81',11,'task_2d76929f-958d-487c-9551-4783e095f08e','run_b51b78a8-da1c-492a-86b1-e9d4a6c0f070','run.started','task','cli','{"input":"publish the package","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_15092518-de68-4554-ac27-99f230f13f82',12,'task_c15a822f-2945-4ede-9cd4-8802844fa434','run_2e00d913-b24b-4f0c-8733-9180146c9c9c','run.started','task','cli','{"input":"publish the docs site","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_366f959c-d398-40f5-899d-683ca7f877c3',13,'task_ee7e8763-d9a5-4421-9fb2-6305f8fbf1d7','run_8a4a7d89-f2a8-4ca0-84fb-d801b68c1b11','run.started','task','cli','{"input":"sync two clusters","resumes_run_id":""}',NULL,1791248257);
INSERT INTO task_events VALUES('event_747637ac-e949-4844-aa5e-ca9f94c7d208',14,'task_000a5724-893e-450d-922b-34a40b277bf9','run_1889c130-cfa2-4aae-a209-904376ed9da8','run.started','task','cli','{"input":"rebuild the docs index","resumes_run_id":""}',NULL,1791248257);
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
INSERT INTO approval_requests VALUES('apr_27fa0e6f-ee3d-4e50-b1de-6a0f2728cd98','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','run_177c4360-1f0c-4cc4-bc77-9a15f8369064','tool_call','{"tool":"terminal","command":"./deploy.sh staging"}','pending','cli','',NULL,NULL,1791248257,1791248257,NULL,NULL,NULL,'parked',1791248257,'approval wait budget elapsed',NULL,NULL,NULL,'resume:v1:fixture-deploy',NULL,NULL,NULL);
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
INSERT INTO clarify_requests VALUES('clr_7294739d-2cd0-4f1a-9143-72479dd265f0','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','task_45f1e2cd-291a-42a9-9292-4a025dd18b85','run_0e47d612-b65e-4457-925d-f1713302ed82','Which branch should be renamed?','[]','pending','','cli',1791248257,1791248257,NULL);
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
INSERT INTO task_queue VALUES('queue_b2bc620d-0012-4d78-8ea5-12e80d3a7295','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','fixture-owner','look up today''s release notes','smart','','','','','foreground',100,0,'queued',0,'',0,0,1791248257,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','','','','[{"kind":"image","path":"/srv/fixture/attachments/shot.png","mime_type":"image/png","name":"shot.png","size":42}]');
INSERT INTO task_queue VALUES('queue_386804f4-d84f-479e-8f56-089ca78b21c2','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','Resume the original task after a durable external observation.','','','task_f2edda5b-cc7d-4647-b938-3433ea241604','','external-watch:watch_9bc6408c-1fe3-4533-8315-ad2650ad4ed8:r1','finalization',80,0,'queued',0,'',0,0,1791248257,'null','run_ef1ce369-6f68-47ec-8f32-af00ca6462f5','','','[]');
INSERT INTO task_queue VALUES('queue_10859482-8dcc-4465-b137-0c962f144868','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','Recover the interrupted run in verification-only mode.','','','task_c7bfcc53-8670-4843-bd35-784b4003c1d0','','run-recovery:run_b6445199-ef5d-48b8-a3f5-bc28692b7911:verify_only','recovery',60,0,'queued',0,'',0,0,1791248257,'[{"path":"/srv/fixture/repo","role":"primary","access_cap":"write","source":"workspace"}]','run_b6445199-ef5d-48b8-a3f5-bc28692b7911','','','[]');
INSERT INTO task_queue VALUES('queue_3b8fc0fc-9f2e-401c-8494-41759c318ffb','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','Continue this exact work after the provider became available.','','','task_3907b332-bccb-43d6-99d5-5412d662c77e','','provider-wait:run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9','finalization',80,1791251858,'queued',0,'',0,0,1791248257,'null','run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9','','','[]');
INSERT INTO task_queue VALUES('queue_5220dc4b-ca1d-4dd5-975d-e8958c5a13ea','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','telegram','telegram','1000001','summarize the overnight alerts','','','','','','cron',20,0,'queued',0,'',0,0,1791248257,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_7e3f7473-f5af-4d22-9919-e9552fa6688e','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','rebuild the docs index','full-auto','','','run_1889c130-cfa2-4aae-a209-904376ed9da8','','foreground',100,0,'started',0,'claim_bbc4826e-a906-4023-bf49-a1b54aa660a0',1791248317,1,1791248257,'[{"path":"/srv/fixture/docs","role":"primary","access_cap":"write","source":"workspace"}]','','','','[]');
INSERT INTO task_queue VALUES('queue_2b976dfd-7b5a-4153-abea-5a209575c7e5','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','rotate the preview credentials','','','','','','foreground',100,0,'done',0,'',0,0,1791248257,'null','','','','[]');
INSERT INTO task_queue VALUES('queue_d040c9e6-9a73-4710-be50-b44dc30ab0ec','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','cli','cli','','/qwer','','','','','','foreground',100,0,'cancelled',0,'',0,0,1791248257,'null','','','','[]');
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
INSERT INTO inbound_dedup VALUES('weixin','fixture-wx-pending',1791262906,'pending',x'7b22636f6e74656e74223a22636865636b207468652073746167696e67206c6f6773227d',1791262906,'','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','check the staging logs');
INSERT INTO inbound_dedup VALUES('weixin','fixture-wx-uncertain',1791262906,'dispatching',x'7b22636f6e74656e74223a22726573746172742074686520707265766965772073657276696365227d',1791262906,'gateway unavailable: connection reset','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','restart the preview service');
INSERT INTO inbound_dedup VALUES('feishu','fixture-fs-accepted',1791262906,'accepted',x'7b22636f6e74656e74223a2273756d6d6172697a65207468652072656c65617365206e6f746573227d',1791262906,'','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','summarize the release notes');
INSERT INTO inbound_dedup VALUES('telegram','fixture-tg-other',1791262906,'dispatching',x'7b22636f6e74656e74223a22636865636b2074686520696e766f696365206578706f7274227d',1791262906,'','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','check the invoice export');
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
INSERT INTO external_watches VALUES('watch_270460dd-9512-4d98-a8d7-728b75d5e009','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','task_3907b332-bccb-43d6-99d5-5412d662c77e','run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9','cli','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED','^SUCCEEDED$','',1,'','','','pending','','pending','not_required',30,30,30,1791255457,1791248257,0,0,0,1,0,'','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248257,1791248257,NULL,'','',0,'',0,'','','','','{"version":2,"command_hash":"95e776a4a498058eba1c4cbe16f7f37a1c1f88f50cda924026103cf597a2ab3a","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
INSERT INTO external_watches VALUES('watch_9bc6408c-1fe3-4533-8315-ad2650ad4ed8','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','task_f2edda5b-cc7d-4647-b938-3433ea241604','run_ef1ce369-6f68-47ec-8f32-af00ca6462f5','telegram','fixture deployment check','/srv/fixture/repo','printf SUCCEEDED; true','^SUCCEEDED$','',1,'','','','succeeded','','pending','not_required',30,30,30,1791255457,1791248257,0,0,1,1,0,'SUCCEEDED','','','{"version":0,"id":"","tenant_id":"","person_id":""}',1791248257,1791248257,1791248257,'','',0,'',0,'','','','','{"version":2,"command_hash":"b9056c85234d9017146f98ccf5ec1f0787214eade918913063d185171db9b80f","environment_generation":0,"adapter":"","target":"","deadline_unix":0}','');
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
INSERT INTO loop_checkpoints VALUES('run_177c4360-1f0c-4cc4-bc77-9a15f8369064','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','task_b6e55a2d-a977-4c58-b99b-c6769a888bf5',3,'execute_tools','awaiting approval',x'5b7b22726f6c65223a2275736572222c22636f6e74656e74223a226465706c6f79207468652070726576696577206275696c6420746f2073746167696e67227d5d',1791248257,2,x'7b7d');
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
INSERT INTO run_work_units VALUES('wu_7d30ddd5-c654-46da-8cd3-eb0bf7f0e5fe','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_151a51c0-9948-491f-83a4-4b92e6073fa4',1,'task_658b5d0e-65c9-44b2-99fc-0d808629b857','task_658b5d0e-65c9-44b2-99fc-0d808629b857','summarize the release notes','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_a5d41ec1-b4af-4c7e-b201-4f2e84f6cc7c','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_b6445199-ef5d-48b8-a3f5-bc28692b7911',1,'task_c7bfcc53-8670-4843-bd35-784b4003c1d0','task_c7bfcc53-8670-4843-bd35-784b4003c1d0','migrate the build scripts','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_3dc4e5b8-f76b-4f94-928c-0f2e3fcc8212','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_b6821f4e-ba34-4e8b-9c93-89799f2b024b',1,'task_52870343-a2b0-4ff6-aec3-2b112c176f57','task_52870343-a2b0-4ff6-aec3-2b112c176f57','deploy the preview build','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_83d914c2-e363-488e-a078-8ad76c2eb1b5','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_5ee7c377-cfcd-4ef9-9620-d01fd40b7b3d',1,'task_52870343-a2b0-4ff6-aec3-2b112c176f57','task_52870343-a2b0-4ff6-aec3-2b112c176f57','continue','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_7e6e75af-0f95-433c-b2b7-5459df8bd496','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_5d364e6f-0e3e-4949-81f9-b0e1c3d5e4c9',1,'task_3907b332-bccb-43d6-99d5-5412d662c77e','task_3907b332-bccb-43d6-99d5-5412d662c77e','roll out the canary','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_82ba8880-a13c-4157-8dd8-c7a6d448d4d6','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_ef1ce369-6f68-47ec-8f32-af00ca6462f5',1,'task_f2edda5b-cc7d-4647-b938-3433ea241604','task_f2edda5b-cc7d-4647-b938-3433ea241604','roll out the stable build','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_ff80786a-f5e7-492c-a745-69562214c1a6','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_177c4360-1f0c-4cc4-bc77-9a15f8369064',1,'task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','deploy the preview build to staging','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_6b0c2529-d12b-4dc5-bb9a-990d01eb8553','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_0e47d612-b65e-4457-925d-f1713302ed82',1,'task_45f1e2cd-291a-42a9-9292-4a025dd18b85','task_45f1e2cd-291a-42a9-9292-4a025dd18b85','rename the release branch','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb',1,'task_679cc50e-f20b-4eaf-ad7f-f698daf4dfab','task_679cc50e-f20b-4eaf-ad7f-f698daf4dfab','Build the release','completed','completed','Build the release','not_applicable','[]',1791248257,1791248257,1791248257,0,9);
INSERT INTO run_work_units VALUES('wu_b0c74604-7bdb-442c-b058-daabeac8e335','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','','run_33122f43-dade-469a-895c-939d64b9f2a6',1,'task_e4f26535-3e20-4def-8fc5-9c775b318372','task_e4f26535-3e20-4def-8fc5-9c775b318372','check the invoice export','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_3f4b7a5b-edf6-474c-b1a9-f6c427a83ba5','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_b51b78a8-da1c-492a-86b1-e9d4a6c0f070',1,'task_2d76929f-958d-487c-9551-4783e095f08e','task_2d76929f-958d-487c-9551-4783e095f08e','publish the package','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_77c2e71d-e803-492c-8924-68ec81ebce7b','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_2e00d913-b24b-4f0c-8733-9180146c9c9c',1,'task_c15a822f-2945-4ede-9cd4-8802844fa434','task_c15a822f-2945-4ede-9cd4-8802844fa434','publish the docs site','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_df810e08-ed24-4297-86f0-85ef996d196a','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_8a4a7d89-f2a8-4ca0-84fb-d801b68c1b11',1,'task_ee7e8763-d9a5-4421-9fb2-6305f8fbf1d7','task_ee7e8763-d9a5-4421-9fb2-6305f8fbf1d7','sync two clusters','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
INSERT INTO run_work_units VALUES('wu_2607ff74-c369-42e5-8f9a-8aa165aa3268','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','run_1889c130-cfa2-4aae-a209-904376ed9da8',1,'task_000a5724-893e-450d-922b-34a40b277bf9','task_000a5724-893e-450d-922b-34a40b277bf9','rebuild the docs index','in_progress','active','','','[]',1791248257,1791248257,NULL,0,0);
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
INSERT INTO schema_migrations VALUES(1,'legacy-baseline',1791248257);
INSERT INTO schema_migrations VALUES(2,'memory-governance-schedule',1791248257);
INSERT INTO schema_migrations VALUES(3,'run-execution-roots',1791248257);
INSERT INTO schema_migrations VALUES(4,'skill-presentation-contract',1791248257);
INSERT INTO schema_migrations VALUES(5,'skill-repair-health',1791248257);
INSERT INTO schema_migrations VALUES(6,'skill-attribution',1791248257);
INSERT INTO schema_migrations VALUES(7,'run-parent-edge',1791248257);
INSERT INTO schema_migrations VALUES(8,'turn-continuity-resolution',1791248257);
INSERT INTO schema_migrations VALUES(9,'run-recovery-contract',1791248257);
INSERT INTO schema_migrations VALUES(10,'run-delivery-override',1791248257);
INSERT INTO schema_migrations VALUES(11,'threaded-work-history',1791248257);
INSERT INTO schema_migrations VALUES(12,'resume-edge-naming',1791248257);
INSERT INTO schema_migrations VALUES(13,'attention-dismissal-only',1791248257);
INSERT INTO schema_migrations VALUES(14,'durable-attachments',1791248257);
INSERT INTO schema_migrations VALUES(15,'drop-task-references',1791248257);
INSERT INTO schema_migrations VALUES(16,'drop-thread-pin',1791248257);
INSERT INTO schema_migrations VALUES(17,'run-plan-step-source',1791248257);
INSERT INTO schema_migrations VALUES(18,'steering-execution-roots',1791248257);
INSERT INTO schema_migrations VALUES(19,'native-im-reply-edges',1791248257);
INSERT INTO schema_migrations VALUES(20,'run-execution-class',1791248257);
INSERT INTO schema_migrations VALUES(21,'turn-choice-observation-receipt',1791248257);
INSERT INTO schema_migrations VALUES(22,'inbound-processing-receipt',1791248257);
INSERT INTO schema_migrations VALUES(23,'external-effect-claims',1791248257);
INSERT INTO schema_migrations VALUES(24,'assessed-plan-cancellations',1791248257);
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
INSERT INTO run_plan_versions VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',1,'fixture plan','ac7e45a80d513be33db3ff2ee7037578',1791248257);
INSERT INTO run_plan_versions VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',2,'the person takes the live check','bb45f78f61ac592116c5a93abdc1427e',1791248257);
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
	created_at INTEGER NOT NULL, source_step_id TEXT NOT NULL DEFAULT '', source_plan_version INTEGER NOT NULL DEFAULT 0, prior_verification_reused INTEGER NOT NULL DEFAULT 0, reuse_reason TEXT NOT NULL DEFAULT '', cancellation_disposition TEXT NOT NULL DEFAULT '', cancellation_reason TEXT NOT NULL DEFAULT '', user_takeover_quote TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, plan_version, step_id),
	UNIQUE (run_id, plan_version, sequence)
);
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',1,'step_6048cd66-305f-4a21-9525-4b8cb62692b7',1,'Build the release','completed','the build passes',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',1,1791248257,'',0,0,'','','','');
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',1,'step_f3f2ad12-6259-45e0-b46f-5393c8528572',2,'Run the live check','pending','the live check passes',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',0,1791248257,'',0,0,'','','','');
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',1,'step_0d3f8911-0cdf-4c91-a5d9-04e61548fcee',3,'Publish notes','pending','',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',0,1791248257,'',0,0,'','','','');
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',2,'step_6048cd66-305f-4a21-9525-4b8cb62692b7',1,'Build the release','completed','the build passes',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',1,1791248257,'',0,0,'','','','');
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',2,'step_f3f2ad12-6259-45e0-b46f-5393c8528572',2,'Run the live check','cancelled','the live check passes',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',0,1791248257,'',0,0,'','user_takeover','the person runs it','skip the live check');
INSERT INTO run_plan_steps VALUES('run_a13dcb21-cd41-4a1f-ba94-38c3102e53fb','default',2,'step_0d3f8911-0cdf-4c91-a5d9-04e61548fcee',3,'Publish notes','cancelled','',0,'','wu_dd8f8d42-b291-4a12-a112-60cc8b75d2a0',0,1791248257,'',0,0,'','not_required','the notes ship with the tag','');
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
INSERT INTO threads VALUES('task_658b5d0e-65c9-44b2-99fc-0d808629b857','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture finished work','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_c7bfcc53-8670-4843-bd35-784b4003c1d0','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture interrupted work','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_52870343-a2b0-4ff6-aec3-2b112c176f57','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture parked work','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_3907b332-bccb-43d6-99d5-5412d662c77e','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture watched rollout','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_f2edda5b-cc7d-4647-b938-3433ea241604','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture finished rollout','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_b6e55a2d-a977-4c58-b99b-c6769a888bf5','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture parked approval','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_45f1e2cd-291a-42a9-9292-4a025dd18b85','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture pending clarification','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_679cc50e-f20b-4eaf-ad7f-f698daf4dfab','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture planned work','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_e4f26535-3e20-4def-8fc5-9c775b318372','default','person_855dd362-7d6a-41b4-8a79-a01c12276c3e','','work','listed','Fixture other person work','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_2d76929f-958d-487c-9551-4783e095f08e','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture effect holder','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_c15a822f-2945-4ede-9cd4-8802844fa434','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture effect waiter','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_ee7e8763-d9a5-4421-9fb2-6305f8fbf1d7','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture effect reserver','',1791248257,1791248257,1791248257);
INSERT INTO threads VALUES('task_000a5724-893e-450d-922b-34a40b277bf9','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','','work','listed','Fixture drained work','',1791248257,1791248257,1791248257);
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
INSERT INTO external_effect_claims VALUES('effectclaim_b749f851-55bd-44c1-bb86-5ddcb98b0559','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','run_b51b78a8-da1c-492a-86b1-e9d4a6c0f070','effect-publish','registry:npm','uncertain','',1791248257,1791248257);
INSERT INTO external_effect_claims VALUES('effectclaim_dd593bed-5546-43f3-9fb0-b165491b1498','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','run_b51b78a8-da1c-492a-86b1-e9d4a6c0f070','effect-shell','git:origin','observed','watch:fixture:r1',1791248257,1791248257);
INSERT INTO external_effect_claims VALUES('effectclaim_a8db93dc-13c9-41a9-9c81-9e0317f48ccf','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','run_8a4a7d89-f2a8-4ca0-84fb-d801b68c1b11','effect-sync','cluster:east','reserved','',1791248257,1791248257);
INSERT INTO external_effect_claims VALUES('effectclaim_e52d7c6b-056c-4f47-84c9-ca1b854eee59','default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','run_8a4a7d89-f2a8-4ca0-84fb-d801b68c1b11','effect-sync','cluster:west','reserved','',1791248257,1791248257);
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
INSERT INTO external_resource_waits VALUES('default','person_190db8f2-2b5e-4e3b-a3e3-1eeed3071929','run_2e00d913-b24b-4f0c-8733-9180146c9c9c','effect-docs','["registry:npm"]','pending',1791248257,1791248257);
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
