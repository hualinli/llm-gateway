-- 000012：Gateway 实例心跳（T07B 恢复前置条件）。
--
-- T07B（docs/runbooks/billing-recovery.md）规定：僵尸 reserved/executing 只有在
-- 「实例失活、最大 attempt 时长 + 恢复宽限已过、同 execution 不存在成功 receipt、
-- 恢复检查留证」之后才能有审计地释放。前两条里唯一无法从内存/Redis 得到的事实是
-- 「拥有这条 execution 的 gateway 进程现在还活着吗」—— 进程的存活必须写在一个
-- 崩溃后仍然存在的地方，因此这里建一张极小的实例表：
--
--   gateway_instances(instance_id PK, started_at, last_seen_at, created_at, updated_at)
--
-- 为什么是独立表而不是复用 Control 的内存报告（internal/control/gateway_status.go）：
--   * 内存报告随 Control 进程消失，Control 重启后旧时间戳不能当在线（T09 的
--     明确要求）；恢复决策不能依赖一个会在重启后清空的 map；
--   * 心跳是 Gateway 对 PostgreSQL 的一次 UPSERT，与 Control 通道的可用性无关：
--     Control 挂了不影响「这个实例是否还在续租」这个事实；
--   * 本迁移刻意不改 Control 的内存报告：它继续服务 T09 的配置 applied/stale
--     语义，两处事实各自有明确的读者。
--
-- 语义（与 internal/billing 的 InstanceAliveFor 一一对应）：
--   * started_at —— 该 instance_id 当前这次进程生命的起点（进程重启且复用同一
--     instance_id 时会推进）。恢复据此判断「准入这条 execution 的那个进程」是否
--     还是现在这个进程：started_at <= execution.created_at 才算同一次生命；
--   * last_seen_at —— 最近一次心跳时刻。恢复只在 now - last_seen_at 超过存活
--     租约（liveness lease）时认为实例失活；心跳写入用 GREATEST 单调推进，乱序
--     或重放的心跳不可能把 last_seen_at 拉回过去；
--   * created_at/updated_at —— 本行的簿记，不参与恢复判断。
--
-- 刻意不做的事：
--   * 不建外键（instance_id 来自 GATEWAY_INSTANCE_ID/主机名+pid，legacy 合成
--     identity 没有对应行，与 billing_executions.gateway_id 同一口径）；
--   * 不做 TTL/自动删除：删除一行等于让「未知拥有者」变成「从未见过」，两者在恢复
--     策略里都必须保守处理，但它们的诊断含义不同，不能靠删除混淆；
--   * 不在这里做自动释放：本表只承载存活事实，释放仍在 internal/billing 的
--     RecoverExecution 单事务里，带审计行。
--
-- 版本号由父任务在 T07B 迁移就位后统一提升（migrations.Version），本文件不修改它。
BEGIN;

CREATE TABLE gateway_instances (
  instance_id TEXT PRIMARY KEY,
  started_at TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  -- 与 Control 的 maxInstanceIDBytes 同一口径：instance_id 是一个身份，不是走私通道。
  CONSTRAINT gateway_instances_instance_id_check
    CHECK (instance_id = btrim(instance_id) AND length(instance_id) BETWEEN 1 AND 128),
  -- 心跳不可能早于它自己的进程启动：倒挂的时间戳说明写入方在伪造事实。
  -- created_at 是这一行的诞生时刻，进程重启复用 instance_id 后 started_at 会
  -- 推进到新的进程起点，所以它只受 updated_at >= created_at 约束。
  CONSTRAINT gateway_instances_seen_check CHECK (last_seen_at >= started_at),
  CONSTRAINT gateway_instances_timestamps_check CHECK (updated_at >= created_at)
);

-- 平台维度的实例视图（T09 的 stale 判定、T07B 的“还有哪些实例在续租”）：
-- 按最近心跳倒序/范围扫描都由这个索引提供，不需要全表排序。
CREATE INDEX idx_gateway_instances_last_seen ON gateway_instances (last_seen_at);

COMMIT;
