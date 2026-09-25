SELECT installation_id
     , owner_uid
     , node_id
     , idempotency_key
     , operation_kind
     , request_hash
     , generation
     , state
     , result
     , failure
     , created_at
     , updated_at
  FROM operation_journal
 WHERE state=?
 ORDER BY created_at, installation_id, owner_uid, idempotency_key
