INSERT INTO control_operations (
  operation_id, kind, payload_digest, actor_scope, request_id, result,
  expected_version, resulting_version, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
