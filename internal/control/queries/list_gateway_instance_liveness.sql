-- T09：数据面存活事实，只读。
-- gateway_instances 由 Gateway 自己经计费连接池写入（000012 / T07B）；Control 只
-- 读它来回答「这个实例的进程还在续租吗」，绝不用它推断配置状态，也不写它。
SELECT instance_id, started_at, last_seen_at
FROM gateway_instances
ORDER BY last_seen_at DESC, instance_id ASC
LIMIT $1
