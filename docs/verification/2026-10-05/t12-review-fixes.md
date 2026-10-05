# T12 review 修复验证（2026-10-05）

基线：`origin/codex/t12-release-candidate`，`ece0695`。修复分支：`codex/t12-review-fixes`。本次不合并 main、不处理 tenant-user-billing 的迁移冲突。

## 修复行为

1. 邀请接受与成员变更共用组织锁；active 成员不被邀请覆盖（409）。只有移除后新创建的邀请能恢复 removed 成员。接受时重新校验邀请者当前授予权限，并在取得组织锁后重新检查有效期。
2. PG 计费仅接受省略 n 或整数 n=1，多份输出在预占和上游调用前被拒绝（400 invalid_request）。
3. PG 计费仅接受文本字符串或纯 text 内容数组；图片、音频、文件、tool/function 内容及未知 message 字段在预占和上游调用前被拒绝。
4. SSE 必须收到 [DONE] 才正常完成；EOF 没有完成标记时保留已收到的文字并报不完整响应错误。
5. 对账测试的 Redis Stream ID 使用固定测试窗口内的时间，避免随执行日期变化而失败。

## 回归覆盖

- 数据库：邀请不能改变 owner/admin/member/billing_viewer 的活跃角色；唯一 owner、状态、org_revision 保持不变；旧 admin 邀请不能覆盖后接受的 member 邀请或恢复之后移除的成员；移除后的新邀请可以恢复；邀请者失去管理/授予角色权限后旧邀请被拒绝；并发接受不同角色邀请恰好一次成功、一次 conflict，只递增一次 revision。
- 真实 HTTP + PostgreSQL：n=8 与 remote image 请求返回 400，未调用上游，未创建 execution 或预占余额。
- 纯函数：非法/溢出 n、远程及内联图片、音频、文件、工具内容、未知字段、畸形消息被拒绝；n=1 与纯 text 数组仍保持保守上界和原样转发。
- 前端：正常 [DONE]、分块 CRLF、断流保留部分文本并报错、空/截断流拒绝完成。

## 验证结果

- `go test -race ./... -count=1 -timeout=180s`：通过；启用 CONTROL/IAM/METERING/GATEWAY 的 PostgreSQL 集成以及 METERING/LIMITER 的 Redis 集成，使用本次专用临时容器。
- 最终补充锁后有效期检查与并发邀请用例后：`go test -race ./internal/organization -count=1 -timeout=90s`（真实 PostgreSQL）通过。
- `go vet ./...` 与 `git diff --check`：通过。
- `npm test`：4 个测试文件、62 个测试通过。
- `npm run build`：TypeScript 检查与生产构建通过。

未运行浏览器端到端或发布环境验收。测试容器仅使用 tmpfs，验证后清理。
