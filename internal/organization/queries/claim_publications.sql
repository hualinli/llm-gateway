-- 声明（租约）单行：attempts +1，next_retry_at 推到租约到期。
-- 在选出批次的同一个短事务里逐行调用，然后立即提交。
UPDATE policy_publications
SET attempts = attempts + 1, next_retry_at = $2, updated_at = $3
WHERE id = $1
