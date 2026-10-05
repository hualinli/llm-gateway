UPDATE policy_publications
SET status = 'published', last_error = NULL, next_retry_at = NULL,
    operation_id = $2, updated_at = $3
WHERE id = $1
