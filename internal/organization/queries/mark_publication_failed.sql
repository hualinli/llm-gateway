UPDATE policy_publications
SET status = 'failed', last_error = $2, next_retry_at = $3, updated_at = $4
WHERE id = $1
