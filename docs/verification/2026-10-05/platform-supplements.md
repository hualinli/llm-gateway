# 平台闭环补充验证（2026-10-05）

> 后续更新（2026-10-06）：三个分支已修复审核发现并合入 main；本页保留 10 月 5 日验收时点，最新证据见[审核合并报告](../2026-10-06/parallel-review-merge.md)。

主线基线 `3bc04a7`；新增功能尚未合入 main。三个远端分支：

| 分支 | 代码提交 | 内容 |
|---|---|---|
| `codex/funds-review-console` | `ea40a2b`（实现 `a22ab0c`） | 登录操作人、CSRF、全局分页人工队列、理由与确认、释放/人工结算、提交后的账户与分录 |
| `codex/transport-recovery-alerts` | `180bc13` | 长传输故障持续退避、历史异常受控重投、事务审计、六类 Prometheus 告警与规则 CI |
| `codex/project-status-refresh` | 验证代码基线 `3f47a3e` | 组合前两个功能分支，刷新 README、状态、限制、证据与运维入口 |

## 验证结果

| 验证 | 结果 | 保存证据 |
|---|---|---|
| `make integration-test`（全项目 `go test -race`，CONTROL/IAM/METERING/GATEWAY 的 PG，METERING/LIMITER 的 Redis） | PASS | [integration.txt](platform-supplements/integration.txt) |
| 前端单元 | 64/64 PASS | [web-unit.txt](platform-supplements/web-unit.txt) |
| TypeScript 与生产构建 | PASS | [web-build.txt](platform-supplements/web-build.txt) |
| Prometheus 配置、六条规则、持续异常与恢复解除测试 | PASS | [alerts.txt](platform-supplements/alerts.txt) |
| 隔离全栈平台验收（9 步：登录、Key、真实调用、收费、余额拒绝、撤销/移除、审计、Redis 故障恢复、Gateway 重启）及安全扫描 | PASS；0 secret leaks | [summary](platform-supplements/release-candidate-summary.json) |
| 三角色浏览器套件，包含人工队列交互 | 28/28 PASS，0 failed | [browser.txt](platform-supplements/browser.txt) |

资金操作通过真实 Console HTTP + PG 验证：分页队列、释放与按预占人工结算、真实登录 actor 入审计、终态重复请求 409、账户/分录一致。
浏览器人工队列用受控 fixture 验证交互，不声称浏览器 fixture 证明真实账本。

长故障用真实 PG 和可控失败 publisher/时钟验证：连续 40 次传输错误超过旧尝试预算仍保留原收据，退避封顶且不热循环，恢复后恰好一条 debit；重复结算返回 replay。
历史传输 exception 并发重投仅一次成功，原始身份/digest 不变，重投事务不移动资金，审计一次；未知 usage、编码异常、缺 actor/理由、已结算或已释放均拒绝。
全栈验收另覆盖真实 Redis 中断与恢复；本次没有进行数小时真实停机或 60 分钟 soak。

## 修复和运行环境说明

首轮浏览器 27 通过、1 失败，定位为理由输入框缺少可访问标签。`ea40a2b` 增加标签并处理表单校验拒绝，复跑全部通过。
复跑重新构建时 Docker Hub 基础镜像 TLS 超时；最终使用 `--skip-build` 复用首轮本次构建的后端镜像（后端源码未改变），前端 Vite 使用修复后的源码。
本地临时指定缓存 Chromium revision 1234，避免重新下载浏览器；该配置已还原，没有提交路径或改变 CI 的锁定浏览器配置。
截图用例按既有发布候选脚本排除，避免覆盖历史交付截图；28 项为该脚本选择的实际执行数，不替代 T08 历史 33 项数字。
测试栈按 compose project 隔离并回收，未删除其它项目资源。

主线 [CI #18](https://github.com/hualinli/llm-gateway/actions/runs/37321863065) 已成功。
新增分支已推送触发 CI；本次桌面锁定使远端运行页面无法继续核验，**不将新增分支标为远端 CI 已通过**。上述表格是本地组合验证结果。

## 操作入口与剩余边界

平台管理员使用「平台计费运维 → 待人工处理的资金请求」。理由必填，actor 从会话取；金额/ID 用字符串传输。
人工 settle 按准入预占收费，release 明确不向客户收费；未知响应先查状态和审计，前端不自动重试资金操作。
Metering 旧异常重投走私有管理 bearer；它只恢复派发，不能绕过原结算幂等与收据校验。

按用户选择先在 Prometheus `/alerts` 查看，不接群消息或外部通知。
规则和处置步骤见[手册](../../runbooks/dispatch-recovery-alerts.md)。生产抓取须改用私有诊断监听。
真实支付、生产 HA/mTLS、付费 Provider 验证、长时间容量验收仍未完成；这次补充不将项目称为已生产上线。
