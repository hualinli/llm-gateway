SELECT operation_id, kind, payload_digest, actor_scope, request_id, result,
       expected_version, resulting_version, created_at
FROM control_operations
WHERE operation_id = $1
