# 并行审核、修复与主线合并（2026-10-06）

三个 GPT-6 Luna（max）分别审核资金 Console、传输恢复和组合候选。审核发现均已修复；最终独立复核没有剩余阻断问题。三个分支按顺序以 merge commit 合入 main，合并后源码与验证候选 `f3222cd` 完全一致，随后只更新当前状态和部署文档。

| 分支 | 最终候选 | main 合并提交 |
|---|---|---|
| `codex/funds-review-console` | `3ef1f4a` | `8059b64` |
| `codex/transport-recovery-alerts` | `980b9d0` | `6d5ccd1` |
| `codex/project-status-refresh` | `f3222cd` | `56a55f0` |

## 审核修复

- Console 每次读取 Metering 权威安全窗口，缺失/超时/非法策略时返回 503；不再独立猜测 40 分钟窗口。只读凭据仅允许 GET `/recovery/policy`，不向 Console 共享 Metering 管理 token。
- 人工队列联读 receipt 的真实异常原因，展示 Metering 配置的 SLA；理由限制统一为 512 UTF-8 字节。
- 旧 consumer 拒绝异常收据后，与操作员重投交错时不再把 pending 收据重新打回 exception；确定性 PG/Redis 交错回归证明旧 consumer 不再覆盖 pending retry；独立 PG 重试、派发与结算重放用例验证最多一笔 debit。
- loop=off/nil runtime 的策略端点和异常行为一致：前者返回真实配置，后者安全返回 503。
- 最大执行时长加宽限的 duration 溢出在启动时拒绝，包括 off/auto/on 三种模式；测试覆盖最大合法和及下一纳秒溢出。
- README/runbook 补齐只读凭据、Console URL、503 排查和多 Metering 副本策略一致配置要求。

## 已执行验证

| 验证 | 结果 | 证据 |
|---|---|---|
| 全项目真实 PG/Redis race 集成 | PASS | [integration.txt](parallel-review/integration.txt) |
| 真实 HTTP/PG 人工资金操作及 stale consumer 确定性交错，`-race -v` | 两项明确 PASS，无 SKIP | [critical-regressions.txt](parallel-review/critical-regressions.txt) |
| 前端单元、TypeScript、生产构建 | 64/64 PASS，构建 PASS | [web-unit.txt](parallel-review/web-unit.txt)、[web-build.txt](parallel-review/web-build.txt) |
| Prometheus 配置、六条告警规则和规则测试 | PASS | [alerts.txt](parallel-review/alerts.txt) |
| 隔离冷启动全栈候选、安全检查、秘密扫描 | 9 步 PASS，0 secret leaks | [summary](parallel-review/release-candidate-summary.json) |
| 三角色浏览器套件 | 28/28 PASS | [browser.txt](parallel-review/browser.txt) |
| 窗口溢出补丁后 billing/Metering race 回归 | PASS | [window-overflow.txt](parallel-review/window-overflow.txt) |

全栈 S7 从真实 Console HTTP 读取到仅在 Metering 配置的安全窗口 **7800 秒**与 SLA **7200 秒**，证明跨进程策略接线。`go vet ./...` 和差异空白检查通过。完整全栈验证源码为 `2853fbf`（证据提交 `3499436`）；其后的 `f3222cd` 只新增窗口溢出拒绝与对应回归，已单独复核、测试。

浏览器人工队列用受控 fixture 验证交互；真实资金行为由 HTTP/PG 回归验证。当地使用缓存 Chromium revision 1234，临时启动设置已还原，CI 仍使用锁定的浏览器配置。未将本机结果冒充远端 CI。

## CI 状态口径与剩余边界

历史 main `3bc04a7` 的 [CI #18](https://github.com/hualinli/llm-gateway/actions/runs/37321863065) 成功；补丁前 recovery、funds、组合候选的 CI #20、#21、#22 也已确认成功。资金修复 `3ef1f4a` 的 [CI #25](https://github.com/hualinli/llm-gateway/actions/runs/37338471204) 与恢复修复 `980b9d0` 的 [CI #23](https://github.com/hualinli/llm-gateway/actions/runs/37338470581) 已确认成功；组合候选 #24 被后续溢出修复 #26 按同分支并发规则取代（cancelled，非测试失败）。main 合并后的最终远端结论以[GitHub Actions](https://github.com/hualinli/llm-gateway/actions)对应 main 提交为准；本文提交时正在确认，不能借用旧提交绿灯。

告警仍先在 Prometheus `/alerts` 查看，不接外部通知。生产 HA、真实支付、真实付费 Provider、60 分钟 soak、生产 mTLS/密钥轮换未验证；本次没有扩充这些承诺。
