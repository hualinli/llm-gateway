# 数据库结构

迁移由 golang-migrate 执行，SQL 随二进制嵌入；服务仅检查版本和 dirty 状态，不自动迁移。当前版本以 `migrations.Version` 为准。

## 操作

停止需要升级的服务，为目标数据库设置 `POSTGRES_DSN`，然后执行：

```sh
make migrate-up
make migrate-version
```

没有新迁移时重复执行 up 视为成功。Control、Metering、Console、Gateway 使用的数据库均需匹配 schema；不同数据库需要分别迁移。

开发阶段允许重建数据库，不要求兼容旧数据。新业务目标见 [业务方案](../docs/BUSINESS.md)。当前编号迁移仍描述现有实现，文档整理不会改变 schema。

## 修改与故障

- 迁移 SQL 使用显式事务；版本、dirty 标记和迁移锁由迁移库管理。
- 编号文件成对提供 up/down；新增结构变更时保持版本常量和迁移一致。
- `check.sql` 是服务启动时的只读检查，不是编号迁移。
- dirty 时先调查失败原因和实际 schema，不应直接跳过版本检查。
- `make migrate-down-one` 只回退一版，可能删除表和数据；先检查对应 down.sql，再使用匹配版本的程序。

表归属及资金约束见 [架构](../docs/ARCHITECTURE.md) 和 [技术决策](../docs/decisions.md)。
