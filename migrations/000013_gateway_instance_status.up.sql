-- 000013：Gateway 实例的「已应用配置」持久事实（T09）。
--
-- T09（docs/ARCHITECTURE.md）要求：PG persisted ≠ 全实例
-- active；心跳超时必须显示 unknown/stale，单 writer 必须明确。000012 的
-- gateway_instances 只回答「这个进程还活着吗」（T07B 恢复用），不回答「它应用
-- 了哪个版本」。本表补上后半句，并让它在 Control 重启后依然可读。
--
-- 为什么是独立表而不是给 gateway_instances 加列：
--   * 两张事实的**写入方**不同。gateway_instances 由 Gateway 自己经计费连接池
--     UPSERT（T07B：心跳必须由写 billing_executions.gateway_id 的同一个进程写）。
--     本表由 Control 在 ReportGatewayStatus RPC 里写。把 Control 的观察混进
--     gateway_instances，会让「控制面听到过某实例」变成「数据面证明它还活着」，
--     而 T07B 的恢复策略正是禁止这种推断（它可能让一个已死实例的执行永远不被
--     释放）。两处事实必须能各自独立地被推翻。
--   * 两张事实的**读写者权限**不同：本表只服务 T09 的 desired/applied 视图，
--     恢复路径不读它。
--   * 语义不同：gateway_instances 的心跳是进程存活租约；本表的心跳是**控制面
--     观察到该实例配置状态的时刻**（reported_at 用 Control 的时钟，避免一个
--     时钟跑偏的 Gateway 把自己的陈旧报告伪装成新鲜心跳）。
--
-- 语义：
--   * reported_at —— Control 最近一次收到该实例状态报告的服务端时刻。写入用
--     GREATEST 单调推进，重放/乱序的旧报告不能把心跳拉回过去（否则一次重放
--     就能让 stale 实例看起来 active）。
--   * started_at —— Gateway 报告的**本次进程生命**起点（gRPC metadata 携带）。
--     只增不减：同 id 重启后它前进，Control 据此区分「同一 instance_id 的新进程」
--     与仍在报告的老进程。NULL = 该次报告没有声明进程起点（例如不带 metadata 的
--     旧客户端）：这时保留已有值，绝不拿 reported_at 冒充进程启动时刻。
--   * applied_version —— 该实例最近一次成功服务的快照版本；一行存在即代表至少
--     报告过一次。
--   * state/rejected_version/reject_reason —— 最近一次应用尝试的结果。applied 与
--     rejected 是同一行的两个侧面：一次成功的应用不会抹掉上一次拒绝的版本号，
--     除非 Gateway 明确报告了更新的 rejected_version（与内存报告的语义一致）。
--   * 行不存在 = **unknown**，不是「active」也不是「stale」。删掉一行等于把
--     「从没报告过」变成「从没存在过」，两者在运维视图里含义不同，所以本表不做
--     TTL 自动删除。
--
-- instance_id 的口径与 gateway_instances / internal/billing.MaxInstanceIDBytes /
-- internal/control 的 maxInstanceIDBytes 一致：1..128 字节，无首尾空白。
BEGIN;

CREATE TABLE gateway_instance_status (
  instance_id TEXT PRIMARY KEY,
  -- applied 或 rejected；未知由「没有行」表达，不用第三个值代替。
  state TEXT NOT NULL,
  applied_version BIGINT NOT NULL,
  rejected_version BIGINT NOT NULL DEFAULT 0,
  reject_reason TEXT NOT NULL DEFAULT '',
  started_at TIMESTAMPTZ,
  reported_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT gateway_instance_status_instance_id_check
    CHECK (instance_id = btrim(instance_id) AND length(instance_id) BETWEEN 1 AND 128),
  CONSTRAINT gateway_instance_status_state_check
    CHECK (state IN ('applied', 'rejected')),
  CONSTRAINT gateway_instance_status_versions_check
    CHECK (applied_version >= 0 AND rejected_version >= 0),
  -- 与 Control 的 maxRejectReasonBytes 同一上限：拒绝原因不是走私通道。
  CONSTRAINT gateway_instance_status_reason_check
    CHECK (length(reject_reason) <= 512),
  -- started_at 未知（NULL）是允许的；已知时它不可能晚于报告时刻。
  CONSTRAINT gateway_instance_status_timestamps_check
    CHECK (updated_at >= created_at AND (started_at IS NULL OR reported_at >= started_at))
);

-- T09 的 stale/unknown 判定按 reported_at 年龄扫描；实例列表按最近心跳倒序。
CREATE INDEX idx_gateway_instance_status_reported_at
  ON gateway_instance_status (reported_at DESC);

COMMIT;
