-- Typed portable delegation. Additive DDL only; no tasks or credentials seeded.
CREATE TABLE IF NOT EXISTS connector_tasks (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  owner_id BIGINT NOT NULL,
  idempotency_hash VARCHAR(64) NOT NULL,
  request_digest VARCHAR(64) NOT NULL,
  agent_id VARCHAR(64) NOT NULL,
  provider VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL,
  attempt BIGINT NOT NULL DEFAULT 0,
  lease_hash VARCHAR(64) NULL,
  lease_until DATETIME(3) NULL,
  deadline_at DATETIME(3) NULL,
  cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
  recovery_reason VARCHAR(32) NOT NULL DEFAULT '',
  `input` JSON NOT NULL,
  `output` JSON NULL,
  output_digest VARCHAR(64) NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY idx_connector_task_request (owner_id, idempotency_hash),
  UNIQUE KEY idx_connector_tasks_lease_hash (lease_hash),
  INDEX idx_connector_tasks_owner_id (owner_id),
  INDEX idx_connector_task_queue (agent_id, status, created_at)
);
CREATE TABLE IF NOT EXISTS connector_task_events (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id VARCHAR(64) NOT NULL,
  attempt BIGINT NOT NULL,
  action VARCHAR(32) NOT NULL,
  from_status VARCHAR(32) NOT NULL,
  to_status VARCHAR(32) NOT NULL,
  agent_id VARCHAR(64) NOT NULL DEFAULT '',
  owner_id BIGINT NOT NULL DEFAULT 0,
  lease_until DATETIME(3) NULL,
  deadline_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_connector_task_events (task_id, id)
);
