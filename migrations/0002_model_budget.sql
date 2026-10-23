-- One atomic MySQL 8 ALTER keeps the ledger extension together.
-- Nullable call_key preserves possible legacy rows without inventing call IDs.
ALTER TABLE model_calls
 ADD COLUMN job_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 ADD COLUMN step_id CHAR(36) CHARACTER SET ascii NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 ADD COLUMN call_key VARCHAR(128) NULL,
 ADD COLUMN request_hash CHAR(64) CHARACTER SET ascii NOT NULL DEFAULT '',
 ADD COLUMN usage_known BOOLEAN NOT NULL DEFAULT FALSE,
 ADD COLUMN budget_limit INT NOT NULL DEFAULT 0,
 ADD UNIQUE KEY model_physical_call(step_id,call_key),
 ADD INDEX model_job_budget(job_id);
