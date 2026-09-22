# 数据库结构迁移

本项目使用 golang-migrate v4，未引入 GORM 或 AutoMigrate。迁移 SQL 随二进制嵌入，命令不依赖当前目录；日常查询 SQL 独立放在各模块 queries 目录。

## 执行顺序

先停止旧版本 Control、Metering 和 Gateway，设置目标数据库的 POSTGRES_DSN，再执行：

```sh
make migrate-up
make migrate-version
```

当前版本为 3。成功后启动新版服务。服务只读取 schema_migrations 校验版本和 dirty 状态，不会自动改变表结构。数据库已连接但版本不符合时直接失败，不会静默切换内存台账。Control 与 Metering 默认共用数据库；如果使用不同数据库，需要分别运行迁移，当前每个库都会建立完整项目 schema。

命令入口是 cmd/migrate，内部调用 golang-migrate。up 运行所有尚未执行的迁移；再次运行没有新版本时视为成功。迁移库负责版本记录、dirty 标记和 PostgreSQL 迁移锁。SQL 使用显式事务，失败时不会留下半套业务表结构，但 dirty 状态仍需人工调查。

## 文件与旧数据

- 000001_initial：建立计量、控制配置、审计表。IF NOT EXISTS 用于接纳此前没有迁移版本表的开发数据库。
- 000002_event_id：若存在旧 request_id 列，则重命名为 event_id；已经完成改名的开发库跳过这一步。原 ID、金额和约束保留。
- 000003_provider_credentials：新增 `provider_credentials`，只保存 AES-256-GCM 密文、nonce、key id 和指纹；审计表增加 credential/provider/gateway 元数据列。历史 `env://` 或 `env:` Provider 引用不会被读取或写入数据库。升级前要先把密钥导入凭据仓库，并把配置改成 `control://provider-credentials/<id>`，否则 Control 会拒绝该快照。
- 每个版本都有 up.sql / down.sql。已有迁移发布后保持不变，后续变更增加新编号。
- check.sql 仅供服务读取版本，不属于编号迁移。

接纳旧库针对本项目已有的两种 schema（改名前/改名后），不是任意数据库结构修复器。应先用副本验证，不通过 force 跳过未知差异。此轮只在隔离测试数据库验证，没有对日常开发数据库执行迁移。

## 回退与故障

```sh
make migrate-down-one
```

该命令只回退一个版本。3 → 2 删除 `provider_credentials` 和审计扩展列，不删除账单或配置历史；2 → 1 仅恢复旧列名，不删除账单。回退后应使用匹配版本的程序。1 → 0 会删除业务表及其数据，执行前必须查看 down.sql。不要在运行中的服务旁执行不兼容迁移。

如果状态为 dirty，先排查失败原因、确认事务和实际 schema。修复后再按 golang-migrate 运维流程校正版本；项目命令故意不提供自动 force 或自动清库。

控制配置中旧 raw-key JSON 转换为 hash 的既有数据规范化仍由 PGStore.MigrateLegacyPayloads 处理，它涉及 Go/protobuf 逻辑，不是本次 schema 迁移；对应查询 SQL 也已分离。不要把 schema version 理解为这项数据转换的完成标志。

## 验证

```sh
make integration-test
```

集成测试先显式迁移各自的测试数据库/schema，再创建业务 Store/Ledger。迁移专项测试覆盖：空库启动不建表、空库迁移、重复 up、两种无版本旧库保留账单升级、2 → 1 → 2、dirty 状态拒绝，以及业务计量迁移后的重复投递去重。
