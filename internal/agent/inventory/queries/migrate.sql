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

CREATE TABLE IF NOT EXISTS network_resources (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  node_id TEXT NOT NULL,
  network TEXT NOT NULL,
  port TEXT NOT NULL,
  mac TEXT NOT NULL,
  ip TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid),
  UNIQUE (installation_id, port)
);

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

CREATE TABLE IF NOT EXISTS published_ports (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  guest_port INTEGER NOT NULL CHECK(guest_port BETWEEN 1 AND 65535),
  host_port INTEGER NOT NULL CHECK(host_port BETWEEN 1 AND 65535),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid, guest_port),
  UNIQUE (host_port),
  FOREIGN KEY (installation_id, owner_uid)
    REFERENCES network_resources (installation_id, owner_uid)
    ON DELETE CASCADE
);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
INSERT OR IGNORE INTO schema_migrations(version) VALUES (2);
INSERT OR IGNORE INTO schema_migrations(version) VALUES (3);
