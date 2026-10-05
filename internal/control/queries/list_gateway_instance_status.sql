-- T09：一页实例状态，最近报告在前。
-- unknown（没有行）不在这里表达：调用方用「存活表并集 - 本表」得到 unknown。
SELECT instance_id, state, applied_version, rejected_version, reject_reason,
       started_at, reported_at, created_at, updated_at
FROM gateway_instance_status
ORDER BY reported_at DESC, instance_id ASC
LIMIT $1
