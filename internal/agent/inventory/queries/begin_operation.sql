INSERT OR IGNORE INTO operation_journal
	(installation_id, owner_uid, node_id, idempotency_key, operation_kind, request_hash, generation, state, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
