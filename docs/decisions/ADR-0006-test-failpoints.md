# ADR-0006：测试专用 failpoint（T10）

> 状态：Accepted（2026-10-01）
> 关联任务：T10（故障、安全与恢复验收）
> 上游：[ADR-0005](ADR-0005-terminal-event-gate.md)（终态 gate）、[billing-recovery runbook §7](../runbooks/billing-recovery.md)（T07B 明确留给 T10 的进程级 kill 窗口）、[BENCHMARK §5](../plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md)（F01–F20）
> 实现：`internal/testsupport/failpoint/`、`scripts/failure-matrix.py`、`cmd/gateway/billing.go`、`internal/billing/{account_pg,receipt_pg,settlement_settle_pg,execution_pg,recovery_pg}.go`、`internal/metering/receipts.go`

## 1. 问题

BENCHMARK §5 要求「kill -9 不是随机撞运气」：必须能说清某次故障注入时事务是否 commit、`[DONE]` 是否发出、ACK 是否发生。T07B §7 把「Gateway 进程被 SIGKILL 时正在进行 COMMIT」这一信号窗口明确留给 T10。

没有可控窗口时，唯一的手段是「跑快一点再杀」，得到的是概率性证据：无法区分「receipt 没提交」和「还没来得及写 receipt」，也无法在 `after insert / before COMMIT` 这种微秒级窗口上稳定复现。这一半的验收价值全部依赖窗口的确定性。

## 2. 决策

### 2.1 failpoint 只存在于带 build tag 的构建里

实现放在 `internal/testsupport/failpoint/failpoint_enabled.go`，由 `//go:build failpoint` 保护。默认构建（`go build ./...`、`go test ./...`、发布构建）编译 `failpoint.go`：`Attach` 是空操作，`Enabled`/`Armed` 恒为 false，`Hit` 恒返回 nil。**发布二进制里没有任何能在资金窗口停住、杀死或让进程失败的代码**，与环境变量无关。

### 2.2 三重门禁

| # | 门禁 | 位置 | 强度 |
|---|---|---|---|
| 1 | build tag | `failpoint_enabled.go` 的 `//go:build failpoint` | 结构性的：未带 tag 的二进制没有实现 |
| 2 | `Attach` 拒绝 prod | `failpoint_enabled.go` | 带 tag 的构建在 `GATEWAY_PROFILE=prod` + `GATEWAY_FAILPOINTS` 非空时返回错误，各服务启动即 `log.Fatalf` |
| 3 | 生产 preflight | `gatewayChecks` / `meteringChecks` | **所有构建**都要求 `GATEWAY_FAILPOINTS` 为空，因此即使有人把带 tag 的二进制放进生产，preflight 也会拒绝启动 |

门禁 3 的常量 `failpoint.EnvVar` 声明在无 build 约束的 `doc.go` 里，正是为了让不带 tag 的构建也能拒绝它。

### 2.3 窗口清单与动作

环境变量 `GATEWAY_FAILPOINTS="name=action,..."`，动作 `hold`（默认）/`kill`/`exit`/`panic`/`error`。每个名字每个进程最多触发一次。命中时向 stderr 写一行机器可读标记：

```text
FAILPOINT {"event":"hit","name":"receipt.before_insert","action":"hold","pid":4711,"at":"..."}
```

runner 监听该行，确认进程**确实停在窗口上**之后才发 SIGKILL。窗口清单（完整说明见 `internal/testsupport/failpoint/doc.go`）：admission before/after reserve、after mark-executing、admission/receipt/settlement COMMIT 响应丢失；receipt before insert、after insert before COMMIT、after COMMIT before `[DONE]`；settlement before/after commit；settlement ACK before/after；recovery release before/after commit。

### 2.4 为什么不是「测试里直接调 store」

store 级测试（已有 T06B/T07A 的那批）能证明事务语义，但证明不了**进程边界**：SIGKILL 打断 COMMIT、连接随进程消失、重启后重投、心跳让 janitor 判定拥有者死亡。窗口必须落在真实进程的真实代码路径上，否则 F05/F06/F07/F19 无法闭环。

## 3. 后果

* 生产构建、`go vet ./...`、`go test ./...` 都看不到 failpoint 代码；带 tag 的验证必须显式运行（`go build -tags failpoint ./...`、`go test -tags failpoint ./...`）。
* `cmd/gateway` 与 `cmd/metering` 各多一次 `failpoint.Attach` 调用：未带 tag 时是空操作，不产生 I/O。
* `PGAccountStore.withTx`/`commitUnknownTx` 多一个 `op string` 参数。未命名（`op == ""`）的调用路径完全跳过查表，行为与改造前逐字节一致。
* `cmd/upstream-mock` 增加 `-usage normal|missing|negative|mismatch` 与 `x_mock_usage`，用于 F04 的三种 usage 形态；它是本地假上游，不是生产组件。

## 4. 明确不做

* 不做运行时可远程开启的开关（那会把门禁 1 变成一句注释）。
* 不做随机 chaos：窗口是枚举出来的，每个窗口对应 F 矩阵里的一条断言。
* faithful 的进程级注入只覆盖 gateway/metering；control/console 的 failpoint 由各自任务按需增加。
