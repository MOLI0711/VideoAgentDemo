CREATE TABLE IF NOT EXISTS users (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, username VARCHAR(100) NOT NULL UNIQUE,
 password_hash VARCHAR(255) NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);
CREATE TABLE IF NOT EXISTS auth_sessions (
 token_hash CHAR(64) CHARACTER SET ascii PRIMARY KEY, user_id CHAR(36) CHARACTER SET ascii NOT NULL,
 expires_at DATETIME(6) NOT NULL, revoked_at DATETIME(6) NULL, INDEX auth_expiry(expires_at)
);
CREATE TABLE IF NOT EXISTS quota_guards (
 owner_id CHAR(36) CHARACTER SET ascii PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS api_idempotency (
 owner_id CHAR(36) CHARACTER SET ascii NOT NULL, scope VARCHAR(100) NOT NULL,
 idempotency_key VARCHAR(128) CHARACTER SET ascii NOT NULL, request_hash CHAR(64) CHARACTER SET ascii NOT NULL,
 resource_id CHAR(36) CHARACTER SET ascii NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(owner_id,scope,idempotency_key)
);
CREATE TABLE IF NOT EXISTS videos (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 title VARCHAR(255) NOT NULL, status VARCHAR(32) NOT NULL,
 source_asset_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 active_version_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 desired_generation BIGINT NOT NULL DEFAULT 0, duration_ms BIGINT NOT NULL DEFAULT 0,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 deleted_at DATETIME(6) NULL, INDEX video_owner(owner_id,created_at,id)
);
CREATE TABLE IF NOT EXISTS video_versions (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, video_id CHAR(36) CHARACTER SET ascii NOT NULL,
 generation BIGINT NOT NULL, status VARCHAR(32) NOT NULL, config JSON NOT NULL,
 index_name VARCHAR(200) NOT NULL DEFAULT '', index_build_id VARCHAR(100) NOT NULL DEFAULT '', metadata JSON NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), UNIQUE KEY version_generation(video_id,generation)
);
CREATE TABLE IF NOT EXISTS upload_sessions (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 video_id CHAR(36) CHARACTER SET ascii NOT NULL, multipart_id VARCHAR(1024) NOT NULL,
 object_key VARCHAR(700) NOT NULL, filename VARCHAR(255) NOT NULL, content_type VARCHAR(255) NOT NULL,
 size BIGINT NOT NULL, status VARCHAR(32) NOT NULL,
 job_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 expires_at DATETIME(6) NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 INDEX upload_expiry(status,expires_at), INDEX upload_owner(owner_id,id)
);
CREATE TABLE IF NOT EXISTS jobs (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 video_id CHAR(36) CHARACTER SET ascii NOT NULL, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 type VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, config JSON NOT NULL,
 error_code VARCHAR(100) NOT NULL DEFAULT '', error_message VARCHAR(1000) NOT NULL DEFAULT '',
 deadline_at DATETIME(6) NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), INDEX job_owner(owner_id,created_at,id), INDEX job_status(status,updated_at)
);
CREATE TABLE IF NOT EXISTS job_steps (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, job_id CHAR(36) CHARACTER SET ascii NOT NULL,
 step_key VARCHAR(100) NOT NULL, step_type VARCHAR(64) NOT NULL, worker_kind VARCHAR(32) NOT NULL,
 status VARCHAR(32) NOT NULL, dispatch_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
 attempt_no BIGINT UNSIGNED NOT NULL DEFAULT 0, retry_count INT NOT NULL DEFAULT 0,
 lease_owner VARCHAR(128) NOT NULL DEFAULT '', lease_until DATETIME(6) NULL,
 next_run_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), next_poll_at DATETIME(6) NULL,
 current_external_job_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 result JSON NOT NULL, error_code VARCHAR(100) NOT NULL DEFAULT '', created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 UNIQUE KEY step_key_unique(job_id,step_key), INDEX step_ready(worker_kind,status,next_run_at,id),
 INDEX step_expired(status,lease_until,id), INDEX step_poll(status,next_poll_at,id)
);
CREATE TABLE IF NOT EXISTS job_attempts (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, step_id CHAR(36) CHARACTER SET ascii NOT NULL,
 attempt_no BIGINT UNSIGNED NOT NULL, worker_id VARCHAR(128) NOT NULL, status VARCHAR(32) NOT NULL,
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), ended_at DATETIME(6) NULL,
 error_code VARCHAR(100) NOT NULL DEFAULT '', UNIQUE KEY attempt_unique(step_id,attempt_no)
);
CREATE TABLE IF NOT EXISTS outbox_events (
 event_id CHAR(36) CHARACTER SET ascii PRIMARY KEY, step_id CHAR(36) CHARACTER SET ascii NOT NULL,
 dispatch_seq BIGINT UNSIGNED NOT NULL, payload JSON NOT NULL, status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
 publish_attempt BIGINT UNSIGNED NOT NULL DEFAULT 0, publish_lease_until DATETIME(6) NULL,
 next_publish_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 UNIQUE KEY outbox_dispatch(step_id,dispatch_seq), INDEX outbox_due(status,next_publish_at,event_id)
);
CREATE TABLE IF NOT EXISTS inbox_events (
 consumer_name VARCHAR(100) NOT NULL, event_id CHAR(36) CHARACTER SET ascii NOT NULL,
 received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY(consumer_name,event_id)
);
CREATE TABLE IF NOT EXISTS quarantined_events (
 topic VARCHAR(200) NOT NULL, partition_id INT NOT NULL, offset_value BIGINT NOT NULL,
 error_code VARCHAR(100) NOT NULL, received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(topic,partition_id,offset_value)
);
CREATE TABLE IF NOT EXISTS external_jobs (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, step_id CHAR(36) CHARACTER SET ascii NOT NULL,
 submit_generation BIGINT UNSIGNED NOT NULL, provider_ref VARCHAR(100) NOT NULL,
 client_request_id VARCHAR(100) NOT NULL, provider_job_id VARCHAR(255) NOT NULL DEFAULT '',
 status VARCHAR(32) NOT NULL, request JSON NOT NULL, deadline_at DATETIME(6) NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 UNIQUE KEY external_generation(step_id,submit_generation), UNIQUE KEY external_request(provider_ref,client_request_id)
);
CREATE TABLE IF NOT EXISTS media_assets (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 video_id CHAR(36) CHARACTER SET ascii NOT NULL, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 step_id CHAR(36) CHARACTER SET ascii NOT NULL, attempt_no BIGINT UNSIGNED NOT NULL,
 kind VARCHAR(64) NOT NULL, storage_backend VARCHAR(32) NOT NULL, object_key VARCHAR(700) NOT NULL,
 content_type VARCHAR(255) NOT NULL DEFAULT '', size BIGINT NOT NULL DEFAULT 0, checksum VARCHAR(255) NOT NULL DEFAULT '',
 start_ms BIGINT NOT NULL DEFAULT 0, end_ms BIGINT NOT NULL DEFAULT 0, metadata JSON NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 UNIQUE KEY asset_object(storage_backend,object_key), INDEX asset_version(video_version_id,kind,start_ms), INDEX asset_video(video_id,kind)
);
CREATE TABLE IF NOT EXISTS transcript_sentences (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 start_ms BIGINT NOT NULL, end_ms BIGINT NOT NULL, text TEXT NOT NULL, INDEX sentence_time(video_version_id,start_ms,id)
);
CREATE TABLE IF NOT EXISTS video_frames (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 asset_id CHAR(36) CHARACTER SET ascii NOT NULL, timestamp_ms BIGINT NOT NULL,
 ocr_text TEXT NOT NULL, ocr_status VARCHAR(32) NOT NULL, INDEX frame_time(video_version_id,timestamp_ms,id)
);
CREATE TABLE IF NOT EXISTS video_segments (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 ordinal INT NOT NULL, start_ms BIGINT NOT NULL, end_ms BIGINT NOT NULL, text TEXT NOT NULL, ocr_text TEXT NOT NULL,
 sentence_ids JSON NOT NULL, frame_ids JSON NOT NULL, UNIQUE KEY segment_order(video_version_id,ordinal)
);
CREATE TABLE IF NOT EXISTS agents (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 name VARCHAR(200) NOT NULL, description TEXT NOT NULL, status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
 active_version_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), INDEX agent_owner(owner_id,created_at,id)
);
CREATE TABLE IF NOT EXISTS agent_versions (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, agent_id CHAR(36) CHARACTER SET ascii NOT NULL,
 version_no INT NOT NULL, status VARCHAR(32) NOT NULL, config JSON NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), UNIQUE KEY agent_version(agent_id,version_no)
);
CREATE TABLE IF NOT EXISTS sessions (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 video_id CHAR(36) CHARACTER SET ascii NOT NULL, video_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 agent_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 active_run_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), INDEX session_owner(owner_id,created_at,id)
);
CREATE TABLE IF NOT EXISTS messages (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, session_id CHAR(36) CHARACTER SET ascii NOT NULL,
 run_id CHAR(36) CHARACTER SET ascii NOT NULL, seq BIGINT NOT NULL, role VARCHAR(32) NOT NULL, content MEDIUMTEXT NOT NULL,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), UNIQUE KEY message_seq(session_id,seq)
);
CREATE TABLE IF NOT EXISTS agent_runs (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, job_id CHAR(36) CHARACTER SET ascii NOT NULL UNIQUE,
 session_id CHAR(36) CHARACTER SET ascii NOT NULL, owner_id CHAR(36) CHARACTER SET ascii NOT NULL,
 video_version_id CHAR(36) CHARACTER SET ascii NOT NULL, agent_version_id CHAR(36) CHARACTER SET ascii NOT NULL,
 index_name VARCHAR(200) NOT NULL, index_build_id VARCHAR(100) NOT NULL,
 status VARCHAR(32) NOT NULL, question TEXT NOT NULL, answer JSON NOT NULL,
 next_event_seq BIGINT NOT NULL DEFAULT 0, checkpoint_seq BIGINT NOT NULL DEFAULT 0,
 budget_used INT NOT NULL DEFAULT 0, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 INDEX run_session(session_id,created_at,id)
);
CREATE TABLE IF NOT EXISTS run_events (
 run_id CHAR(36) CHARACTER SET ascii NOT NULL, seq BIGINT NOT NULL, attempt_no BIGINT UNSIGNED NOT NULL,
 type VARCHAR(100) NOT NULL, payload JSON NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(run_id,seq), INDEX event_retention(created_at)
);
CREATE TABLE IF NOT EXISTS run_checkpoints (
 run_id CHAR(36) CHARACTER SET ascii NOT NULL, seq BIGINT NOT NULL, attempt_no BIGINT UNSIGNED NOT NULL,
 state JSON NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY(run_id,seq)
);
CREATE TABLE IF NOT EXISTS tool_invocations (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, run_id CHAR(36) CHARACTER SET ascii NOT NULL,
 logical_call_id VARCHAR(200) NOT NULL, tool_name VARCHAR(100) NOT NULL, args JSON NOT NULL,
 status VARCHAR(32) NOT NULL, result JSON NOT NULL, attempt_no BIGINT UNSIGNED NOT NULL,
 UNIQUE KEY tool_call_unique(run_id,logical_call_id)
);
CREATE TABLE IF NOT EXISTS model_calls (
 id CHAR(36) CHARACTER SET ascii PRIMARY KEY, run_id CHAR(36) CHARACTER SET ascii NOT NULL,
 attempt_no BIGINT UNSIGNED NOT NULL, provider_ref VARCHAR(100) NOT NULL, model_id VARCHAR(200) NOT NULL,
 status VARCHAR(32) NOT NULL, reserved_tokens INT NOT NULL, actual_tokens INT NOT NULL DEFAULT 0,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), INDEX model_run(run_id,created_at,id)
);
