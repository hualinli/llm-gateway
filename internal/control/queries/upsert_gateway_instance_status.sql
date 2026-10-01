-- T09：一次 Gateway 状态报告的 UPSERT。
--
-- reported_at/started_at 单调推进（GREATEST）：重放或乱序到达的旧报告不能把
-- 心跳拉回过去，也不能把实例的进程起点拉回过去（那会让「同 id 重启」不可见）。
-- started_at 为 NULL 表示该次报告没有声明进程起点（例如不带 metadata 的旧客户端），
-- 此时保留已有值，绝不拿 reported_at 冒充进程启动时刻。
-- applied_version 只前进：一次被拒绝的应用不能把已经成功应用的版本号抹掉。
INSERT INTO gateway_instance_status (
  instance_id, state, applied_version, rejected_version, reject_reason,
  started_at, reported_at, created_at, updated_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $7, $7
)
ON CONFLICT (instance_id) DO UPDATE SET
  state = EXCLUDED.state,
  applied_version = GREATEST(gateway_instance_status.applied_version, EXCLUDED.applied_version),
  rejected_version = CASE
    WHEN EXCLUDED.rejected_version = 0 THEN gateway_instance_status.rejected_version
    ELSE GREATEST(gateway_instance_status.rejected_version, EXCLUDED.rejected_version)
  END,
  reject_reason = CASE
    WHEN EXCLUDED.state = 'rejected' THEN EXCLUDED.reject_reason
    WHEN EXCLUDED.rejected_version = 0 THEN gateway_instance_status.reject_reason
    ELSE EXCLUDED.reject_reason
  END,
  started_at = GREATEST(gateway_instance_status.started_at, EXCLUDED.started_at),
  reported_at = GREATEST(gateway_instance_status.reported_at, EXCLUDED.reported_at),
  updated_at = GREATEST(gateway_instance_status.updated_at, EXCLUDED.reported_at)
RETURNING instance_id, state, applied_version, rejected_version, reject_reason,
          started_at, reported_at, created_at, updated_at
