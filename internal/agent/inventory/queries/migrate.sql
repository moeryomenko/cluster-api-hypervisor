CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY);

CREATE TABLE IF NOT EXISTS operations (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  generation INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid, idempotency_key)
);

CREATE TABLE IF NOT EXISTS operation_journal (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  node_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  operation_kind TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  generation INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('intent', 'completed', 'failed')),
  result TEXT NOT NULL DEFAULT '',
  failure TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid, idempotency_key)
);

CREATE INDEX IF NOT EXISTS operation_journal_pending_idx ON operation_journal(state, updated_at);

CREATE TABLE IF NOT EXISTS vms (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  node_id TEXT NOT NULL,
  unit TEXT NOT NULL,
  generation INTEGER NOT NULL,
  pid INTEGER NOT NULL,
  disk TEXT NOT NULL,
  api_socket TEXT NOT NULL,
  vhost_socket TEXT NOT NULL,
  network TEXT NOT NULL,
  port TEXT NOT NULL,
  mac TEXT NOT NULL,
  ip TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid),
  UNIQUE (installation_id, unit)
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
