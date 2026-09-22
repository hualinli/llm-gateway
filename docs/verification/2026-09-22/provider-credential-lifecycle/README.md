# FEAT-001 验收记录

日期：2026-09-22  
范围：Provider 动态凭据的创建、热更新、重启恢复、轮换、冲突撤销、坏快照保留和明文扫描。  
本记录不包含测试 secret。泄漏扫描只记录匹配数量。

## 环境

- 隔离依赖：`docker-compose.test.yml` 的 PostgreSQL `127.0.0.1:15433` 与 Redis `127.0.0.1:16379`
- 命令在实现提交前的工作树上执行；提交后的 Git 版本以该提交为准

## 命令与结果

```text
go test ./... -count=1
go vet ./...
go build ./...
docker compose config --quiet
git diff --check
```

结果：通过。

```text
make integration-test
```

第一次完整执行时，除 `TestProviderCredentialLifecyclePostgres` 外的包均通过。该测试当时把“不存在的凭据引用”误判为已持久化，原因是配置错误没有把稳定错误码放进 error chain。修复为 `%w` 包装后重跑：

```text
CONTROL_TEST_POSTGRES_DSN=postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable \
  go test -race ./internal/control ./cmd/control -count=1 -timeout=120s
```

结果：通过。生命周期测试覆盖：

1. 创建动态凭据，响应不含明文。
2. 不重建 Registry 即用新凭据访问要求 Authorization 的进程内 Mock，流以 `[DONE]` 结束。
3. 用同一主密钥文件重新打开 PostgreSQL store 和 Control，新 Registry 重新解析后请求仍成功。
4. 新引用轮换后请求成功，旧凭据可撤销。
5. 撤销当前引用返回 `credential_in_use`。
6. 不存在的引用被拒绝，desired version 不前进，旧 Provider 仍可完成请求。
7. 密文被破坏后的新快照在 Gateway 侧拒绝，上一 Provider 继续服务。
8. 删除 Provider 后撤销成功。
9. `control_config.payload`、密文字段和审计文本中的测试 secret 匹配数为 0。
10. revoked 行的 ciphertext/nonce 已清空。错误主密钥下 `VerifyMasterKey` 失败。

## 泄漏扫描

生命周期测试在数据库配置历史、密文和审计文本中检查两条测试 secret。匹配数量：0。

## 未在本次进程级 Compose 演练中覆盖

- 没有单独停掉 Control/Gateway 容器再拉起的手工剧本；重启语义由同一数据库上的新 Store、新 Server 和新 Registry 覆盖。
- 没有把这次凭据请求送进 Metering Consumer 做对账。计量链路本身未改，本记录不把它算作新的入账证据。
- 多 Gateway 全部切换、master key 在线轮换和外部 Secret Manager 仍未实现。
