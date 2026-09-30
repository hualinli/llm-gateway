-- 有界批次声明：pending，或退避时间已到的 failed。
-- next_retry_at 同时充当“可见性租约”：声明时把它推到 now+lease，崩溃的
-- worker 留下的行会在租约到期后被重新拾起；SKIP LOCKED 让多实例不互相阻塞。
-- 调用方在同一个短事务里把选中的行标记为已声明后立即提交，绝不在持有行锁时
-- 调用 Control（ADR-0003 §2.6）。
SELECT id, org_id, org_revision, scope, target_id, status, attempts
FROM policy_publications
WHERE status IN ('pending', 'failed')
  AND (next_retry_at IS NULL OR next_retry_at <= $1)
ORDER BY org_id, org_revision, id
LIMIT $2
FOR UPDATE SKIP LOCKED
