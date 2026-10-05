# 当前实现状态

本页说明仓库里已有的行为，不代表未来业务目标已经落地。组件与数据流见[架构说明](ARCHITECTURE.md)，目标规则见[业务方案](BUSINESS.md)。

当前代码包含 Gateway 流式代理、Control 配置与上游凭据管理、Console 登录和组织管理、Metering 持久计量与账务，以及 React Web 控制台。现有 Console 以组织作为账户边界，包含 `owner`、`admin`、`member` 和只读账单角色；Key、用量、余额、账本和审计按组织授权。

用户 2026-10-06 确认的新业务方案仍属于待实施目标。个人账户、成员月度额度、账户模型范围和相应调用准入规则，不能因旧代码或验收材料存在而视为已完成。第一版成员共用账户模型范围；成员级模型限制保留为后续 TODO，详见[业务方案](BUSINESS.md)。

当前运行验证仍有边界：配置写入按单个 Control writer 设计；生产 mTLS 部署、真实付费 Provider、长时间稳定性和生产备份恢复尚未验证。通过本地 Demo、模拟 Provider 或发布候选检查，不等于这些生产条件已经满足。

开发数据库允许直接重建；本项目不要求迁移旧消费、旧 Key 或旧成员关系。此约定仅适用于开发数据，不构成生产账务数据的删除流程。

开发与检查入口见[开发说明](DEVELOPMENT.md)。Console HTTP 契约以[OpenAPI 文件](openapi/console-v1.yaml)为准；账务异常的值班处理见[账务恢复手册](runbooks/billing-recovery.md)。
