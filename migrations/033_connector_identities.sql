-- Optional portable connector identity enrollment (fork).
-- Additive tables only; no user, credential or starter-catalog seed data.
CREATE TABLE IF NOT EXISTS connector_bind_codes (
  hash VARCHAR(64) NOT NULL PRIMARY KEY,
  owner_id BIGINT NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  consumed_at DATETIME(3) NULL,
  INDEX idx_connector_bind_codes_owner_id (owner_id)
);
CREATE TABLE IF NOT EXISTS connector_devices (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  owner_id BIGINT NOT NULL,
  token_hash VARCHAR(64) NOT NULL,
  name VARCHAR(128) NOT NULL,
  os VARCHAR(16) NOT NULL,
  arch VARCHAR(16) NOT NULL,
  revoked BOOLEAN NOT NULL DEFAULT FALSE,
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY idx_connector_devices_token_hash (token_hash),
  INDEX idx_connector_devices_owner_id (owner_id)
);
CREATE TABLE IF NOT EXISTS connector_agents (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  device_id VARCHAR(64) NOT NULL,
  provider VARCHAR(32) NOT NULL,
  token_hash VARCHAR(64) NOT NULL,
  version VARCHAR(256) NOT NULL,
  revoked BOOLEAN NOT NULL DEFAULT FALSE,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY idx_connector_agents_token_hash (token_hash),
  UNIQUE KEY idx_connector_device_provider (device_id, provider)
);
