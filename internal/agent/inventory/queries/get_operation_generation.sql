SELECT generation
  FROM operations
 WHERE installation_id=?
   AND owner_uid=?
   AND idempotency_key=?
